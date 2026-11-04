package testserver

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The fake's export endpoints.
const (
	RouteExportCreate Route = "export-create"
	RouteExportGet    Route = "export-get"
	RouteExportList   Route = "export-list"
	RouteExportDelete Route = "export-delete"
	RouteDownload     Route = "download"
)

// ExportLimit is how many exports an account keeps.
const ExportLimit = 5

// ExportDomain is a domain of a fake export and the ZIP archives it ends with.
type ExportDomain struct {
	Domain   string
	Archives [][]byte
	// Fail ends the domain as failed once its archives are listed.
	Fail bool
}

// fakeExport is an export the fake runs. Every look at it (GET) moves it on:
// queued -> running, then one more archive per domain, then each domain
// ends, and the export's status follows the dashboard's rules.
type fakeExport struct {
	id      int64
	status  string
	created time.Time
	domains []*fakeExportDomain
}

type fakeExportDomain struct {
	name     string
	archives [][]byte
	listed   int
	fail     bool
	status   string
}

// SetExportDomains sets the domains the next created exports cover (default:
// "directUpload" with no archives).
func (s *Server) SetExportDomains(domains ...ExportDomain) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exportTemplate = domains
}

// AddExport stores an export in the given status, its archives all listed
// when the status is final, and returns its ID.
func (s *Server) AddExport(status string, domains ...ExportDomain) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.newExportLocked(domains)
	e.status = status
	if finalStatus(status) {
		for _, d := range e.domains {
			d.listed = len(d.archives)
			d.status = "completed"
			if d.fail {
				d.status = "failed"
			}
		}
	}
	return e.id
}

// PauseExports stops (or restarts) the progress of running exports.
func (s *Server) PauseExports(paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exportsPaused = paused
}

// RewriteArchive replaces the bytes of a listed archive, as a repeated server
// step does.
func (s *Server) RewriteArchive(id int64, domain string, part int, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.exports[id].domains {
		if d.name == domain {
			d.archives[part-1] = data
		}
	}
}

// ExpireArchiveURLs makes every archive URL handed out so far fail with 403.
func (s *Server) ExpireArchiveURLs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urlGeneration++
}

// MaxConcurrentDownloads is the highest number of archive downloads seen at
// once.
func (s *Server) MaxConcurrentDownloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxDownloads
}

// ExportIDs lists the stored exports, oldest first.
func (s *Server) ExportIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.exports))
	for id := range s.exports {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func finalStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func (s *Server) newExportLocked(domains []ExportDomain) *fakeExport {
	if s.exports == nil {
		s.exports = map[int64]*fakeExport{}
	}
	s.nextExport++
	e := &fakeExport{
		id:      s.nextExport,
		status:  "queued",
		created: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC).Add(time.Duration(s.nextExport) * time.Minute),
	}
	if len(domains) == 0 {
		domains = []ExportDomain{{Domain: "directUpload"}}
	}
	for _, d := range domains {
		e.domains = append(e.domains, &fakeExportDomain{name: d.Domain, archives: d.Archives, fail: d.Fail, status: "queued"})
	}
	s.exports[e.id] = e
	return e
}

// advanceLocked moves a running export one step on.
func (e *fakeExport) advanceLocked() {
	if finalStatus(e.status) {
		return
	}
	if e.status == "queued" {
		e.status = "running"
		for _, d := range e.domains {
			d.status = "running"
		}
		return
	}
	counts := map[string]int{}
	for _, d := range e.domains {
		if d.status == "running" {
			if d.listed < len(d.archives) {
				d.listed++
			} else if d.fail {
				d.status = "failed"
			} else {
				d.status = "completed"
			}
		}
		counts[d.status]++
	}
	switch {
	case counts["running"] > 0:
		e.status = "running"
	case counts["queued"] > 0:
		e.status = "queued"
	case counts["failed"] > 0:
		e.status = "failed"
	case counts["cancelled"] > 0:
		e.status = "cancelled"
	default:
		e.status = "completed"
	}
}

func (s *Server) exportSummaryLocked(e *fakeExport) map[string]any {
	names := make([]string, 0, len(e.domains))
	for _, d := range e.domains {
		names = append(names, d.name)
	}
	return map[string]any{"id": e.id, "status": e.status, "created_at": e.created.Format("2006-01-02T15:04:05.000000Z"), "domains": names}
}

func (s *Server) exportDetailLocked(e *fakeExport) map[string]any {
	domains := []map[string]any{}
	for _, d := range e.domains {
		files := []map[string]any{}
		for i := 0; i < d.listed; i++ {
			file := map[string]any{"part_number": i + 1, "url": s.archiveURLLocked(e.id, d.name, i+1)}
			if !s.OmitArchiveSizes {
				file["size"] = len(d.archives[i])
			}
			files = append(files, file)
		}
		domains = append(domains, map[string]any{"domain": d.name, "status": d.status, "zip_files": files})
	}
	out := s.exportSummaryLocked(e)
	out["domains"] = domains
	return out
}

func (s *Server) archiveURLLocked(id int64, domain string, part int) string {
	return fmt.Sprintf("%s/exports/%d/%s/images-%d.zip?X-Amz-Signature=%s", s.S3.URL, id, url.PathEscape(domain), part, s.archiveSigLocked(id, domain, part))
}

func (s *Server) archiveSigLocked(id int64, domain string, part int) string {
	return fmt.Sprintf("sig-%d-%d-%x-%d", s.urlGeneration, id, domain, part)
}

// exportAuth authenticates an export request: full-access keys only.
func (s *Server) exportAuth(w http.ResponseWriter, r *http.Request) (int, bool) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return http.StatusUnauthorized, false
	}
	if key.Scoped {
		return writeJSON(w, http.StatusForbidden, map[string]string{"message": "Full API access required"}), false
	}
	return 0, true
}

// handleExports serves GET (list) and POST (create) /api/asset-exports.
func (s *Server) handleExports(w http.ResponseWriter, r *http.Request) {
	route := RouteExportList
	if r.Method == http.MethodPost {
		route = RouteExportCreate
	}
	ev, injected := s.begin(route, r)
	status := 0
	var drop func()
	defer func() { s.finishThenDrop(ev, status, drop) }()
	if injected != nil && injected.CommitThenClose {
		w, drop = commitThenClose(w)
		injected = nil
	}
	if injected != nil {
		status, drop = writeInjected(w, injected)
		return
	}
	var ok bool
	if status, ok = s.exportAuth(w, r); !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		status = s.createExport(w)
	case http.MethodGet:
		status = s.listExports(w, r)
	default:
		status = writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (s *Server) createExport(w http.ResponseWriter) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *fakeExport
	for _, e := range s.exports {
		if latest == nil || e.id > latest.id {
			latest = e
		}
	}
	legacy := s.LegacyExportConflicts
	if latest != nil && !finalStatus(latest.status) {
		body := map[string]any{"message": "Asset export is already in progress"}
		if !legacy {
			body["code"] = "export_in_progress"
			body["export_id"] = latest.id
		}
		return writeJSON(w, http.StatusConflict, body)
	}
	if len(s.exports) >= ExportLimit {
		body := map[string]any{"message": fmt.Sprintf("Maximum asset export count reached (Limit: %d)", ExportLimit)}
		if !legacy {
			body["code"] = "export_limit_reached"
			body["limit"] = ExportLimit
			body["count"] = len(s.exports)
		}
		return writeJSON(w, http.StatusConflict, body)
	}
	e := s.newExportLocked(s.exportTemplate)
	return writeJSON(w, http.StatusCreated, s.exportSummaryLocked(e))
}

func (s *Server) listExports(w http.ResponseWriter, r *http.Request) int {
	q := r.URL.Query()
	perPage := 20
	if v := q.Get("per_page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return validation(w, "per_page", "The per page field must be between 1 and 100.")
		}
		perPage = n
	}
	var before int64 = 1 << 62
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return validation(w, "cursor", "The cursor is invalid.")
		}
		before = n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.exports))
	for id := range s.exports {
		if id < before {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	data := []map[string]any{}
	for _, id := range ids {
		if len(data) == perPage {
			break
		}
		data = append(data, s.exportSummaryLocked(s.exports[id]))
	}
	more := len(ids) > len(data)
	var next any
	if more {
		next = strconv.FormatInt(data[len(data)-1]["id"].(int64), 10)
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"meta": map[string]any{"per_page": perPage, "prev_cursor": nil, "next_cursor": next, "has_more": more},
	})
}

var exportPathPattern = regexp.MustCompile(`^/api/asset-exports/([0-9]+)$`)

// handleExport serves GET and DELETE /api/asset-exports/{id}.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	route := RouteExportGet
	if r.Method == http.MethodDelete {
		route = RouteExportDelete
	}
	ev, injected := s.begin(route, r)
	status := 0
	var drop func()
	defer func() { s.finishThenDrop(ev, status, drop) }()
	if injected != nil && injected.CommitThenClose {
		w, drop = commitThenClose(w)
		injected = nil
	}
	if injected != nil {
		status, drop = writeInjected(w, injected)
		return
	}
	var ok bool
	if status, ok = s.exportAuth(w, r); !ok {
		return
	}
	m := exportPathPattern.FindStringSubmatch(r.URL.Path)
	if m == nil {
		status = writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found."})
		return
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.exports[id]
	if e == nil {
		status = writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !s.exportsPaused {
			e.advanceLocked()
		}
		status = writeJSON(w, http.StatusOK, s.exportDetailLocked(e))
	case http.MethodDelete:
		if !finalStatus(e.status) {
			status = writeJSON(w, http.StatusConflict, map[string]string{"message": fmt.Sprintf("Asset export with status %q cannot be deleted", e.status)})
			return
		}
		delete(s.exports, id)
		w.WriteHeader(http.StatusNoContent)
		status = http.StatusNoContent
	default:
		status = writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

var archivePathPattern = regexp.MustCompile(`^/exports/([0-9]+)/([^/]+)/images-([0-9]+)\.zip$`)

// handleDownload serves archive GETs like S3: the signature must be current,
// "Range: bytes=N-" gets a 206, a range past the end a 416.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	ev, injected := s.begin(RouteDownload, r)
	ev.Range = r.Header.Get("Range")
	status := 0
	var drop func()
	defer func() { s.finishThenDrop(ev, status, drop) }()
	s.mu.Lock()
	s.inflightDownloads++
	s.maxDownloads = max(s.maxDownloads, s.inflightDownloads)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflightDownloads--
		s.mu.Unlock()
	}()
	if s.DownloadDelay > 0 {
		time.Sleep(s.DownloadDelay)
	}
	if r.Header.Get("Authorization") != "" {
		s.violate("archive download carried an Authorization header")
	}
	if injected != nil && injected.CutAfter == 0 && !injected.IgnoreRange && injected.StallAfter == 0 {
		status, drop = writeInjected(w, injected)
		return
	}
	s3Error := func(code int, name string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(code)
		fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>%s</Message></Error>", name, name)
		status = code
	}
	m := archivePathPattern.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil || r.Method != http.MethodGet {
		s3Error(http.StatusForbidden, "AccessDenied")
		return
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	domain, _ := url.PathUnescape(m[2])
	part, _ := strconv.Atoi(m[3])
	ev.Key = fmt.Sprintf("%d/%s/%d", id, domain, part)
	s.mu.Lock()
	var data []byte
	if e := s.exports[id]; e != nil && r.URL.Query().Get("X-Amz-Signature") == s.archiveSigLocked(id, domain, part) {
		for _, d := range e.domains {
			if d.name == domain && part >= 1 && part <= d.listed {
				data = d.archives[part-1]
			}
		}
	}
	s.mu.Unlock()
	if data == nil {
		s3Error(http.StatusForbidden, "AccessDenied")
		return
	}

	start := int64(0)
	if rg := r.Header.Get("Range"); rg != "" && (injected == nil || !injected.IgnoreRange) {
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64)
		if err != nil || !strings.HasPrefix(rg, "bytes=") || !strings.HasSuffix(rg, "-") {
			s.violate("unexpected Range header %q", rg)
		}
		if n >= int64(len(data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			s3Error(http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
		start = n
	}
	body := data[start:]
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		status = http.StatusPartialContent
	} else {
		w.WriteHeader(http.StatusOK)
		status = http.StatusOK
	}
	switch {
	case injected != nil && injected.CutAfter > 0 && injected.CutAfter < int64(len(body)):
		w.Write(body[:injected.CutAfter])
		// Flushed first: hijacking discards what is still buffered.
		w.(http.Flusher).Flush()
		drop = func() { dropConn(w) }
	case injected != nil && injected.StallAfter > 0 && injected.StallAfter < int64(len(body)):
		w.Write(body[:injected.StallAfter])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	default:
		w.Write(body)
	}
}
