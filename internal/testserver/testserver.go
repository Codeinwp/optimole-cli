// Package testserver is an in-memory fake of the dashboard's unified upload API
// and of the presigned storage endpoint, for tests only. It follows the API
// contract: POST /api/assets, PUT to the presigned URL, POST
// /api/assets/{key}/completion and GET /api/folders.
package testserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultKey is the full-access API key every server accepts.
const DefaultKey = "optml-test-full-access-key-0123456789"

// MaxSize is the contract's upload cap (100 MB).
const MaxSize = 104857600

// Route names a fake endpoint, for injected responses and events.
type Route string

// The fake's endpoints.
const (
	RouteCreate   Route = "create"
	RoutePut      Route = "put"
	RouteComplete Route = "complete"
	RouteFolders  Route = "folders"
)

// Response is a canned answer injected in front of the normal handling.
type Response struct {
	Status int
	Body   string
	Header map[string]string
	// CloseConn drops the connection without an answer (a network error).
	CloseConn bool
}

// Key is an API key the fake accepts.
type Key struct {
	// Scoped keys (prefix optml-s) may only upload into granted folders and
	// their subfolders, and cannot create folders or list folders.
	Scoped  bool
	Granted []int64
}

// Folder is a folder in the fake library.
type Folder struct {
	ID       int64
	Name     string
	ParentID int64 // 0 = root
}

// Asset is an asset reserved through the fake.
type Asset struct {
	Key          string
	Filename     string
	Size         int64
	ContentType  string
	FolderID     int64
	Uploaded     bool
	UploadedSize int64
	SHA256       string
	Completed    bool
	Width        int
	Height       int
	HadHints     bool
}

// Event records one request the fake handled, with sequence numbers taken
// when the handler started and finished.
type Event struct {
	Route         Route
	Start, End    int64
	Status        int
	Filename      string
	FolderPath    string
	FolderID      int64 // as requested
	CreateFolders bool
	Resolved      int64 // folder the asset landed in
	Key           string
	Query         string
}

// Server is the fake. Create it with New.
type Server struct {
	API *httptest.Server
	S3  *httptest.Server
	// URL is the API base URL, ending in /api.
	URL string

	// FolderCreateDelay widens the window between "folder missing" and
	// "folder created", like a server without a lock, so racing requests
	// with create_folders produce duplicate folders.
	FolderCreateDelay time.Duration
	// CompleteDelay slows down completion.
	CompleteDelay time.Duration
	// PutDelay slows down storage uploads.
	PutDelay time.Duration
	// Hook, when set, runs at the start of every request (outside the lock).
	Hook func(route Route, r *http.Request)

	mu          sync.Mutex
	seq         int64
	keys        map[string]Key
	folders     []*Folder
	nextFolder  int64
	assets      map[string]*Asset
	uploadLimit int
	uploads     int
	injected    map[Route][]Response
	events      []Event
	violations  []string
	inflightPut int
	maxPut      int
}

// New starts a fake and closes it when the test ends. The test fails if a
// request broke the contract's client rules (headers, auth on storage).
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		keys:       map[string]Key{DefaultKey: {}},
		assets:     map[string]*Asset{},
		injected:   map[Route][]Response{},
		nextFolder: 100,
	}
	api := http.NewServeMux()
	api.HandleFunc("/api/assets", s.handleCreate)
	api.HandleFunc("/api/assets/", s.handleComplete)
	api.HandleFunc("/api/folders", s.handleFolders)
	s.API = httptest.NewServer(api)
	s.S3 = httptest.NewServer(http.HandlerFunc(s.handlePut))
	s.URL = s.API.URL + "/api"
	t.Cleanup(func() {
		s.API.Close()
		s.S3.Close()
		for _, v := range s.Violations() {
			t.Errorf("contract violation: %s", v)
		}
	})
	return s
}

// AddKey registers another API key.
func (s *Server) AddKey(key string, k Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[key] = k
}

// SetUploadLimit makes the account full after limit uploads (0 = unlimited).
func (s *Server) SetUploadLimit(limit, used int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploadLimit, s.uploads = limit, used
}

// AddFolder creates every missing folder of a "/"-separated path and returns
// the last one's ID.
func (s *Server) AddFolder(p string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parent int64
	for _, name := range strings.Split(p, "/") {
		f := s.childLocked(parent, name)
		if f == nil {
			f = s.newFolderLocked(name, parent)
		}
		parent = f.ID
	}
	return parent
}

// Inject queues canned answers for a route; each request pops one.
func (s *Server) Inject(route Route, responses ...Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected[route] = append(s.injected[route], responses...)
}

// Events returns a copy of the request log.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Count returns how many requests reached a route.
func (s *Server) Count(route Route) int {
	n := 0
	for _, e := range s.Events() {
		if e.Route == route {
			n++
		}
	}
	return n
}

// Violations lists contract rules the client broke.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// Assets returns copies of every reserved asset.
func (s *Server) Assets() []Asset {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Asset, 0, len(s.assets))
	for _, a := range s.assets {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// CompletedByName returns the completed assets keyed by filename.
func (s *Server) CompletedByName() map[string]Asset {
	out := map[string]Asset{}
	for _, a := range s.Assets() {
		if a.Completed {
			out[a.Filename] = a
		}
	}
	return out
}

// FolderPaths returns the full path of every folder, one entry per folder
// (duplicates show up twice).
func (s *Server) FolderPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.folders))
	for _, f := range s.folders {
		out = append(out, s.pathLocked(f.ID))
	}
	sort.Strings(out)
	return out
}

// FolderPath returns the full path of a folder ID.
func (s *Server) FolderPath(id int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pathLocked(id)
}

// MaxConcurrentPuts is the highest number of storage PUTs seen at once.
func (s *Server) MaxConcurrentPuts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxPut
}

// --- handlers ---------------------------------------------------------------

func (s *Server) begin(route Route, r *http.Request) (Event, *Response) {
	if s.Hook != nil {
		s.Hook(route, r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev := Event{Route: route, Start: s.seq, Query: r.URL.RawQuery}
	if q := s.injected[route]; len(q) > 0 {
		resp := q[0]
		s.injected[route] = q[1:]
		return ev, &resp
	}
	return ev, nil
}

func (s *Server) finish(ev Event, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.End = s.seq
	ev.Status = status
	s.events = append(s.events, ev)
}

func (s *Server) violate(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
}

func writeInjected(w http.ResponseWriter, resp *Response) int {
	if resp.CloseConn {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return 0
			}
		}
		panic(http.ErrAbortHandler)
	}
	for k, v := range resp.Header {
		w.Header().Set(k, v)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.Status)
	io.WriteString(w, resp.Body)
	return resp.Status
}

func writeJSON(w http.ResponseWriter, status int, v any) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
	return status
}

func validation(w http.ResponseWriter, field, msg string) int {
	return writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"message": msg,
		"errors":  map[string][]string{field: {msg}},
	})
}

// authenticate checks the headers every dashboard request must carry.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (Key, bool) {
	if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "optimole-cli/") {
		s.violate("%s %s: User-Agent %q", r.Method, r.URL.Path, ua)
	}
	if a := r.Header.Get("Accept"); a != "application/json" {
		s.violate("%s %s: Accept %q", r.Method, r.URL.Path, a)
	}
	if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
		s.violate("%s %s: Content-Type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	k, known := s.keys[token]
	s.mu.Unlock()
	if !ok || !known {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
		return Key{}, false
	}
	return k, true
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RouteCreate, r)
	status := 0
	defer func() { s.finish(ev, status) }()

	var body struct {
		Filename      *string `json:"filename"`
		Size          *int64  `json:"size"`
		FolderID      *int64  `json:"folder_id"`
		FolderPath    *string `json:"folder_path"`
		CreateFolders *bool   `json:"create_folders"`
	}
	raw, _ := io.ReadAll(r.Body)
	decodeErr := json.Unmarshal(raw, &body)
	if body.Filename != nil {
		ev.Filename = *body.Filename
	}
	if body.FolderPath != nil {
		ev.FolderPath = *body.FolderPath
	}
	if body.FolderID != nil {
		ev.FolderID = *body.FolderID
	}
	ev.CreateFolders = body.CreateFolders != nil && *body.CreateFolders

	if injected != nil {
		status = writeInjected(w, injected)
		return
	}
	if r.Method != http.MethodPost {
		status = writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		status = http.StatusUnauthorized
		return
	}
	if decodeErr != nil {
		status = validation(w, "filename", "The request body must be JSON.")
		return
	}
	if body.Filename == nil || *body.Filename == "" || len([]rune(*body.Filename)) > 255 {
		status = validation(w, "filename", "The filename field is required.")
		return
	}
	name := path.Base(strings.ReplaceAll(*body.Filename, "\\", "/"))
	ct, allowed := contentType(name)
	if !allowed {
		status = validation(w, "filename", "The file type is not allowed.")
		return
	}
	if body.Size == nil || *body.Size < 1 || *body.Size > MaxSize {
		status = validation(w, "size", "The size field must be between 1 and 104857600.")
		return
	}
	if body.FolderID != nil && body.FolderPath != nil {
		status = validation(w, "folder_path", "The folder path field prohibits folder id from being present.")
		return
	}
	create := body.CreateFolders != nil && *body.CreateFolders
	if key.Scoped && create {
		status = writeJSON(w, http.StatusForbidden, map[string]string{"message": "Folder-scoped API keys cannot create folders."})
		return
	}
	if key.Scoped && body.FolderID == nil && body.FolderPath == nil {
		status = writeJSON(w, http.StatusForbidden, map[string]string{"message": "Folder-scoped API keys must upload into a folder."})
		return
	}
	s.mu.Lock()
	full := s.uploadLimit > 0 && s.uploads >= s.uploadLimit
	limit, used := s.uploadLimit, s.uploads
	s.mu.Unlock()
	if full {
		status = writeJSON(w, http.StatusForbidden, map[string]any{
			"code": "upload_limit_reached", "message": "You have reached the upload limit of your plan.",
			"limit": limit, "count": used,
		})
		return
	}

	const invalidFolder = "The selected folder is invalid."
	var folderID int64
	switch {
	case body.FolderID != nil:
		s.mu.Lock()
		f := s.folderLocked(*body.FolderID)
		s.mu.Unlock()
		if f == nil {
			status = validation(w, "folder_id", invalidFolder)
			return
		}
		folderID = f.ID
	case body.FolderPath != nil:
		id, err := s.resolvePath(*body.FolderPath, create)
		if err != nil {
			status = validation(w, "folder_path", invalidFolder)
			return
		}
		folderID = id
	}
	if key.Scoped && !s.granted(key, folderID) {
		status = validation(w, firstNonEmpty(body.FolderPath != nil, "folder_path", "folder_id"), invalidFolder)
		return
	}

	assetKey := randomKey()
	s.mu.Lock()
	s.uploads++
	s.assets[assetKey] = &Asset{Key: assetKey, Filename: name, Size: *body.Size, ContentType: ct, FolderID: folderID}
	s.mu.Unlock()
	ev.Resolved = folderID
	ev.Key = assetKey

	uploadURL := fmt.Sprintf("%s/bucket/%s?X-Amz-Signature=sig-%s&X-Amz-SignedHeaders=content-length%%3Bcontent-type%%3Bhost", s.S3.URL, assetKey, assetKey)
	var folder any
	if folderID > 0 {
		folder = folderID
	}
	status = writeJSON(w, http.StatusCreated, map[string]any{
		"asset": map[string]any{
			"resourceS3": assetKey, "folder_id": folder, "itemStatus": "pending", "mimeType": ct, "url": nil,
		},
		"uploadUrl": uploadURL,
		"upload": map[string]any{
			"url": uploadURL, "method": "PUT",
			"headers":    map[string]string{"Content-Type": ct},
			"expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		},
	})
}

func firstNonEmpty(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

var completionPath = regexp.MustCompile(`^/api/assets/([0-9a-fA-F]{32})/completion$`)

func (s *Server) handleComplete(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RouteComplete, r)
	status := 0
	defer func() { s.finish(ev, status) }()
	m := completionPath.FindStringSubmatch(r.URL.Path)
	if m != nil {
		ev.Key = m[1]
	}
	if injected != nil {
		status = writeInjected(w, injected)
		return
	}
	if m == nil || r.Method != http.MethodPost {
		status = writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found."})
		return
	}
	if _, ok := s.authenticate(w, r); !ok {
		status = http.StatusUnauthorized
		return
	}
	var hints struct {
		Width  *int `json:"width"`
		Height *int `json:"height"`
	}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &hints); err != nil {
			status = validation(w, "width", "The request body must be JSON.")
			return
		}
	}
	if s.CompleteDelay > 0 {
		time.Sleep(s.CompleteDelay)
	}
	s.mu.Lock()
	a := s.assets[strings.ToLower(m[1])]
	var resp map[string]any
	switch {
	case a == nil:
		status = http.StatusNotFound
	case !a.Uploaded:
		status = http.StatusConflict
	case a.UploadedSize != a.Size:
		status = http.StatusUnprocessableEntity
	default:
		status = http.StatusOK
		a.Completed = true
		ev.Resolved = a.FolderID
		if hints.Width != nil && hints.Height != nil {
			a.Width, a.Height, a.HadHints = *hints.Width, *hints.Height, true
		}
		resp = s.assetJSONLocked(a)
	}
	var expected, actual int64
	if a != nil {
		expected, actual = a.Size, a.UploadedSize
	}
	s.mu.Unlock()

	switch status {
	case http.StatusNotFound:
		writeJSON(w, status, map[string]string{"message": "Not found."})
	case http.StatusConflict:
		writeJSON(w, status, map[string]string{"code": "upload_not_found", "message": "The file has not been uploaded yet."})
	case http.StatusUnprocessableEntity:
		writeJSON(w, status, map[string]any{"code": "upload_size_mismatch", "message": "The uploaded file size does not match.", "expected": expected, "actual": actual})
	default:
		writeJSON(w, status, resp)
	}
}

func (s *Server) assetJSONLocked(a *Asset) map[string]any {
	url := fmt.Sprintf("https://app.i.optimole.com/files/id:%s/directUpload/%s", a.Key, a.Filename)
	if strings.HasPrefix(a.ContentType, "image/") {
		url = fmt.Sprintf("https://app.i.optimole.com/w:auto/h:auto/q:auto/id:%s/directUpload/%s", a.Key, a.Filename)
	}
	var folder any
	if a.FolderID > 0 {
		folder = a.FolderID
	}
	return map[string]any{
		"resourceS3": a.Key, "folder_id": folder, "itemStatus": "done", "uploaded_status": 1,
		"mimeType": a.ContentType, "fileSize": a.Size, "originalWidth": a.Width, "originalHeight": a.Height,
		"url": url,
	}
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RoutePut, r)
	status := 0
	defer func() { s.finish(ev, status) }()

	s.mu.Lock()
	s.inflightPut++
	if s.inflightPut > s.maxPut {
		s.maxPut = s.inflightPut
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflightPut--
		s.mu.Unlock()
	}()

	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	ev.Key = key
	if r.Header.Get("Authorization") != "" {
		s.violate("PUT to storage carried an Authorization header")
	}
	if injected != nil {
		io.Copy(io.Discard, r.Body)
		status = writeInjected(w, injected)
		return
	}
	s3Error := func(code, msg string) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
		status = http.StatusForbidden
	}
	s.mu.Lock()
	a := s.assets[key]
	var size int64
	var ct string
	if a != nil {
		size, ct = a.Size, a.ContentType
	}
	s.mu.Unlock()
	switch {
	case a == nil || r.URL.Query().Get("X-Amz-Signature") != "sig-"+key:
		s3Error("AccessDenied", "Access Denied")
		return
	case r.Method != http.MethodPut:
		s3Error("SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return
	case r.Header.Get("Content-Type") != ct:
		s3Error("SignatureDoesNotMatch", "Content-Type does not match the signed value.")
		return
	case r.ContentLength != size:
		s3Error("SignatureDoesNotMatch", "Content-Length does not match the signed value.")
		return
	}
	if s.PutDelay > 0 {
		time.Sleep(s.PutDelay)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r.Body, size+1))
	if err != nil || n != size {
		s3Error("IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
		return
	}
	s.mu.Lock()
	a.Uploaded = true
	a.UploadedSize = n
	a.SHA256 = hex.EncodeToString(h.Sum(nil))
	s.mu.Unlock()
	w.Header().Set("ETag", `"`+a.SHA256[:32]+`"`)
	w.WriteHeader(http.StatusOK)
	status = http.StatusOK
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RouteFolders, r)
	status := 0
	defer func() { s.finish(ev, status) }()
	if injected != nil {
		status = writeInjected(w, injected)
		return
	}
	if r.Method != http.MethodGet {
		status = writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		status = http.StatusUnauthorized
		return
	}
	if key.Scoped {
		status = writeJSON(w, http.StatusForbidden, map[string]string{"message": "Valid DAM context required for restricted API keys"})
		return
	}
	q := r.URL.Query()
	perPage := 20
	if v := q.Get("per_page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			status = validation(w, "per_page", "The per page field must be between 1 and 100.")
			return
		}
		perPage = n
	}
	parent := q.Get("parent_id")
	if parent != "" && !regexp.MustCompile(`^(root|[1-9][0-9]*)$`).MatchString(parent) {
		status = validation(w, "parent_id", "The parent id field format is invalid.")
		return
	}
	offset := 0
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(c, "c"))
		if err != nil {
			status = validation(w, "cursor", "The cursor is invalid.")
			return
		}
		offset = n
	}

	s.mu.Lock()
	var matched []map[string]any
	for _, f := range s.folders {
		switch {
		case parent == "":
		case parent == "root" && f.ParentID != 0:
			continue
		case parent != "root" && strconv.FormatInt(f.ParentID, 10) != parent:
			continue
		}
		var p any
		if f.ParentID > 0 {
			p = f.ParentID
		}
		matched = append(matched, map[string]any{
			"id": f.ID, "name": f.Name, "parent_id": p, "user_id": 1, "status": "active", "is_default": false,
			"created_at": "2026-10-26T10:00:00.000000Z", "updated_at": "2026-10-26T10:00:00.000000Z",
		})
	}
	s.mu.Unlock()

	end := min(offset+perPage, len(matched))
	page := []map[string]any{}
	if offset < len(matched) {
		page = matched[offset:end]
	}
	var next any
	hasMore := end < len(matched)
	if hasMore {
		next = "c" + strconv.Itoa(end)
	}
	status = writeJSON(w, http.StatusOK, map[string]any{
		"data": page,
		"meta": map[string]any{"per_page": perPage, "prev_cursor": nil, "next_cursor": next, "has_more": hasMore},
	})
}

// --- folder helpers -----------------------------------------------------------

func (s *Server) newFolderLocked(name string, parent int64) *Folder {
	s.nextFolder++
	f := &Folder{ID: s.nextFolder, Name: name, ParentID: parent}
	s.folders = append(s.folders, f)
	return f
}

func (s *Server) folderLocked(id int64) *Folder {
	for _, f := range s.folders {
		if f.ID == id {
			return f
		}
	}
	return nil
}

// childLocked finds a child by name, case-insensitively; the oldest wins.
func (s *Server) childLocked(parent int64, name string) *Folder {
	for _, f := range s.folders {
		if f.ParentID == parent && strings.EqualFold(f.Name, name) {
			return f
		}
	}
	return nil
}

func (s *Server) pathLocked(id int64) string {
	var parts []string
	for f := s.folderLocked(id); f != nil; f = s.folderLocked(f.ParentID) {
		parts = append([]string{f.Name}, parts...)
	}
	return strings.Join(parts, "/")
}

// resolvePath finds or (with create) creates a folder path. Creation is
// deliberately not atomic: see FolderCreateDelay.
func (s *Server) resolvePath(p string, create bool) (int64, error) {
	segs := strings.Split(p, "/")
	if len(segs) > 20 {
		return 0, fmt.Errorf("too deep")
	}
	for i, seg := range segs {
		segs[i] = strings.TrimSpace(seg)
		if segs[i] == "" || len([]rune(segs[i])) > 255 {
			return 0, fmt.Errorf("bad segment")
		}
	}
	s.mu.Lock()
	var parent int64
	i := 0
	for ; i < len(segs); i++ {
		f := s.childLocked(parent, segs[i])
		if f == nil {
			break
		}
		parent = f.ID
	}
	s.mu.Unlock()
	if i == len(segs) {
		return parent, nil
	}
	if !create {
		return 0, fmt.Errorf("missing")
	}
	if s.FolderCreateDelay > 0 {
		time.Sleep(s.FolderCreateDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ; i < len(segs); i++ {
		parent = s.newFolderLocked(segs[i], parent).ID
	}
	return parent, nil
}

func (s *Server) granted(k Key, folderID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for f := s.folderLocked(folderID); f != nil; f = s.folderLocked(f.ParentID) {
		for _, g := range k.Granted {
			if g == f.ID {
				return true
			}
		}
	}
	return false
}

func randomKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// contentType maps an extension to the MIME type the fake signs. It knows
// the extensions the tests use, plus the contract's refused list.
func contentType(name string) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch ext {
	case "jpg", "jpeg", "jpe":
		return "image/jpeg", true
	case "png":
		return "image/png", true
	case "gif":
		return "image/gif", true
	case "webp":
		return "image/webp", true
	case "svg":
		return "image/svg+xml", true
	case "mp4":
		return "video/mp4", true
	case "pdf":
		return "application/pdf", true
	case "txt":
		return "text/plain", true
	case "zip":
		return "application/zip", true
	}
	return "", false
}
