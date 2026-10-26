package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

// sleeps records the waits between retries instead of sleeping.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func newTestClient(t *testing.T, baseURL string) (*Client, *sleeps) {
	t.Helper()
	sl := &sleeps{}
	c, err := New(Options{BaseURL: baseURL, APIKey: testserver.DefaultKey, UserAgent: "optimole-cli/test", Sleep: sl.sleep})
	if err != nil {
		t.Fatal(err)
	}
	return c, sl
}

func TestParseBaseURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://dashboard.optimole.com/api", "https://dashboard.optimole.com/api", true},
		{"https://staging-dashboard.optimole.com/api/", "https://staging-dashboard.optimole.com/api", true},
		{"http://localhost:8080/api", "http://localhost:8080/api", true},
		{"http://127.0.0.1:5555/api", "http://127.0.0.1:5555/api", true},
		{"http://dashboard.optimole.com/api", "", false},
		{"http://localhost.evil.com/api", "", false},
		{"http://[::1]:80/api", "", false},
		{"ftp://dashboard.optimole.com", "", false},
		{"dashboard.optimole.com/api", "", false},
		{"https://user:pass@dashboard.optimole.com/api", "", false},
		{"https://dashboard.optimole.com/api?x=1", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		u, err := ParseBaseURL(tc.in)
		if tc.ok != (err == nil) {
			t.Errorf("ParseBaseURL(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && u.String() != tc.want {
			t.Errorf("ParseBaseURL(%q) = %q, want %q", tc.in, u, tc.want)
		}
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Options{BaseURL: DefaultBaseURL}); err == nil {
		t.Fatal("New without a key succeeded")
	}
}

func TestThreeStepHappyPath(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	body := []byte("%PDF-1.4 fake pdf body")

	created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: "doc.pdf", Size: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	target, err := created.Target()
	if err != nil {
		t.Fatal(err)
	}
	if target.Headers["Content-Type"] != "application/pdf" || target.Method != http.MethodPut || target.ExpiresAt.IsZero() {
		t.Fatalf("target = %+v", target)
	}
	if err := c.PutObject(ctx, target, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	asset, err := c.CompleteAsset(ctx, created.Asset.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Status != "done" || !strings.Contains(asset.URL, "/files/id:"+created.Asset.Key+"/directUpload/doc.pdf") {
		t.Fatalf("asset = %+v", asset)
	}
}

// TestSVGSignsContentDisposition: for SVG the server also signs
// Content-Disposition: attachment, and storage refuses a PUT without it;
// other images get only Content-Type. The client sends whatever is listed.
func TestSVGSignsContentDisposition(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	for _, name := range []string{"icon.svg", "icon.svgz"} {
		created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: name, Size: 3})
		if err != nil {
			t.Fatal(err)
		}
		target, _ := created.Target()
		if len(target.Headers) != 2 || target.Headers["Content-Disposition"] != "attachment" || target.Headers["Content-Type"] != "image/svg+xml" {
			t.Fatalf("%s: headers = %v", name, target.Headers)
		}
		partial := target
		partial.Headers = map[string]string{"Content-Type": "image/svg+xml"}
		var se *StorageError
		if err := c.PutObject(ctx, partial, strings.NewReader("abc"), 3); !errors.As(err, &se) || se.StatusCode != 403 {
			t.Fatalf("%s: PUT without Content-Disposition: %v", name, err)
		}
		if err := c.PutObject(ctx, target, strings.NewReader("abc"), 3); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: "a.png", Size: 3, FolderPath: "/Brand/Logos/", CreateFolders: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.FolderPath(created.Asset.FolderID.Value); got != "Brand/Logos" {
		t.Fatalf("folder = %q (leading and trailing slashes are trimmed)", got)
	}
	target, _ := created.Target()
	if len(target.Headers) != 1 || target.Headers["Content-Type"] != "image/png" {
		t.Fatalf("png headers = %v, want only Content-Type", target.Headers)
	}
	if err := c.PutObject(ctx, target, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
}

func TestPutObjectRejectsWrongSizeOrType(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: "a.png", Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := created.Target()

	// Size differs from the signed one: storage answers 403.
	err = c.PutObject(ctx, target, strings.NewReader("123456789"), 9)
	var se *StorageError
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden || se.Code != "SignatureDoesNotMatch" || !se.NeedsNewAsset() || se.Attempts != 1 {
		t.Fatalf("short body: err = %v", err)
	}

	// Content-Type differs.
	wrong := target
	wrong.Headers = map[string]string{"Content-Type": "image/jpeg"}
	err = c.PutObject(ctx, wrong, strings.NewReader("0123456789"), 10)
	if !errors.As(err, &se) || se.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong type: err = %v", err)
	}

	// Exact size and type: accepted.
	if err := c.PutObject(ctx, target, strings.NewReader("0123456789"), 10); err != nil {
		t.Fatal(err)
	}
}

// chunkReader records the largest single read, to prove the body is streamed.
type chunkReader struct {
	size    int64
	mu      sync.Mutex
	maxRead int
}

func (c *chunkReader) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	if len(p) > c.maxRead {
		c.maxRead = len(p)
	}
	c.mu.Unlock()
	if off >= c.size {
		return 0, io.EOF
	}
	n := len(p)
	if rem := c.size - off; int64(n) > rem {
		n = int(rem)
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	if int64(n)+off == c.size {
		return n, io.EOF
	}
	return n, nil
}

func TestPutObjectStreams(t *testing.T) {
	const size = 8 << 20
	var mu sync.Mutex
	var got, gotLen int64
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		got, gotLen = n, r.ContentLength
		mu.Unlock()
	}))
	defer s3.Close()
	c, _ := newTestClient(t, "http://127.0.0.1:1/api")
	r := &chunkReader{size: size}
	err := c.PutObject(context.Background(), UploadTarget{URL: s3.URL + "/k", Method: http.MethodPut}, r, size)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != size || gotLen != size {
		t.Fatalf("server got %d bytes, Content-Length %d; want %d", got, gotLen, size)
	}
	if r.maxRead >= size/8 {
		t.Fatalf("largest read was %d bytes; the body is not streamed", r.maxRead)
	}
}

func TestPutObjectNeverSendsAPIKeyAndHidesSignature(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
		}
	}))
	defer s3.Close()
	c, _ := newTestClient(t, "http://127.0.0.1:1/api")
	err := c.PutObject(context.Background(), UploadTarget{URL: s3.URL + "/k?X-Amz-Signature=supersecretsig", Method: http.MethodPut}, strings.NewReader("abc"), 3)
	var se *StorageError
	if !errors.As(err, &se) || se.StatusCode != 0 || se.NeedsNewAsset() || !se.Uncertain || se.Attempts != MaxAttempts {
		t.Fatalf("err = %#v, want a network StorageError after %d attempts", err, MaxAttempts)
	}
	if strings.Contains(err.Error(), "supersecretsig") {
		t.Fatalf("error leaks the presigned URL: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != MaxAttempts {
		t.Fatalf("storage saw %d requests, want %d", len(auths), MaxAttempts)
	}
	for _, a := range auths {
		if a != "" {
			t.Fatalf("storage request carried Authorization %q", a)
		}
	}
}

// putAsset reserves an asset on the fake and returns the client and target.
func putAsset(t *testing.T, srv *testserver.Server, size int64) (*Client, *sleeps, *CreateAssetResponse, UploadTarget) {
	t.Helper()
	c, sl := newTestClient(t, srv.URL)
	created, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.txt", Size: size})
	if err != nil {
		t.Fatal(err)
	}
	target, err := created.Target()
	if err != nil {
		t.Fatal(err)
	}
	return c, sl, created, target
}

func TestPutObjectRetriesTheSameURL(t *testing.T) {
	cases := []struct {
		name   string
		inject []testserver.Response
		puts   int
	}{
		{"stored, then the connection dropped", []testserver.Response{{CommitThenClose: true}}, 2},
		{"no answer", []testserver.Response{{CloseConn: true}, {CloseConn: true}}, 3},
		{"storage 500", []testserver.Response{{Status: 500, Body: "<Error><Code>InternalError</Code></Error>"}}, 2},
		{"storage 503", []testserver.Response{{Status: 503, Body: "<Error><Code>SlowDown</Code></Error>"}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testserver.New(t)
			srv.Inject(testserver.RoutePut, tc.inject...)
			c, sl, created, target := putAsset(t, srv, 3)
			if err := c.PutObject(context.Background(), target, strings.NewReader("abc"), 3); err != nil {
				t.Fatal(err)
			}
			for _, e := range srv.Events() {
				if e.Route == testserver.RoutePut && e.Key != created.Asset.Key {
					t.Fatalf("PUT went to asset %s, want %s", e.Key, created.Asset.Key)
				}
			}
			if n := srv.Count(testserver.RoutePut); n != tc.puts || len(sl.all()) != tc.puts-1 {
				t.Fatalf("PUTs = %d, sleeps = %d; want %d PUTs", n, len(sl.all()), tc.puts)
			}
			if _, err := c.CompleteAsset(context.Background(), created.Asset.Key, nil); err != nil {
				t.Fatal(err)
			}
			if n := srv.Count(testserver.RouteCreate); n != 1 {
				t.Fatalf("creates = %d", n)
			}
		})
	}
}

func TestPutObjectStopsWhenTheURLExpires(t *testing.T) {
	srv := testserver.New(t)
	srv.UploadExpiresIn = -time.Minute
	srv.Inject(testserver.RoutePut, testserver.Response{CloseConn: true}, testserver.Response{CloseConn: true})
	c, _, _, target := putAsset(t, srv, 3)
	err := c.PutObject(context.Background(), target, strings.NewReader("abc"), 3)
	var se *StorageError
	if !errors.As(err, &se) || !se.Expired || !se.Uncertain || !se.NeedsNewAsset() || se.Attempts != 1 {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "the upload URL expired") {
		t.Fatalf("error text = %q", err)
	}
	if n := srv.Count(testserver.RoutePut); n != 1 {
		t.Fatalf("PUTs = %d, want 1 (the expired URL is not retried)", n)
	}
}

func TestPutObjectRefusesRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		io.Copy(io.Discard, r.Body)
	}))
	defer other.Close()
	for _, status := range []int{301, 302, 307, 308} {
		srv := testserver.New(t)
		srv.Inject(testserver.RoutePut, testserver.Response{Status: status, Header: map[string]string{"Location": other.URL + "/bucket/x"}})
		c, _, _, target := putAsset(t, srv, 3)
		err := c.PutObject(context.Background(), target, strings.NewReader("abc"), 3)
		var se *StorageError
		if !errors.As(err, &se) || se.StatusCode != status || se.NeedsNewAsset() || se.Uncertain || se.Attempts != 1 {
			t.Fatalf("%d: err = %#v", status, err)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the file was sent to the redirect target %d times", n)
	}
}

func TestPutObjectRefusesInsecureUploadURL(t *testing.T) {
	c, _ := newTestClient(t, "http://127.0.0.1:1/api")
	err := c.PutObject(context.Background(), UploadTarget{URL: "http://bucket.example.com/k?sig=1", Method: http.MethodPut}, strings.NewReader("abc"), 3)
	if err == nil || strings.Contains(err.Error(), "sig=1") {
		t.Fatalf("err = %v", err)
	}
}

func TestTargetFallsBackToUploadURL(t *testing.T) {
	r := CreateAssetResponse{UploadURL: "https://bucket.s3.amazonaws.com/k?sig"}
	target, err := r.Target()
	if err != nil || target.URL != r.UploadURL || target.Method != http.MethodPut {
		t.Fatalf("target = %+v, err = %v", target, err)
	}
	r.Upload = &UploadInstructions{URL: "https://other/k", Method: "post"}
	if _, err := r.Target(); err == nil {
		t.Fatal("POST upload method accepted")
	}
	if _, err := (&CreateAssetResponse{}).Target(); err == nil {
		t.Fatal("missing upload URL accepted")
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteCreate, testserver.Response{
		Status: http.StatusTooManyRequests, Body: `{"message":"Too Many Attempts."}`,
		Header: map[string]string{"Retry-After": "7"},
	})
	c, sl := newTestClient(t, srv.URL)
	if _, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if got := sl.all(); len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("sleeps = %v, want [7s]", got)
	}
	if n := srv.Count(testserver.RouteCreate); n != 2 {
		t.Fatalf("create calls = %d, want 2", n)
	}
}

func TestRetryableStatusesUseJitteredBackoff(t *testing.T) {
	for _, status := range []int{429, 502, 503, 504} {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteFolders,
			testserver.Response{Status: status, Body: `{"message":"try again"}`},
			testserver.Response{Status: status, Body: `{"message":"try again"}`},
		)
		c, sl := newTestClient(t, srv.URL)
		if _, err := c.ListFolders(context.Background(), FolderQuery{}); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		got := sl.all()
		if len(got) != 2 {
			t.Fatalf("status %d: sleeps = %v", status, got)
		}
		// Attempt n waits between half and all of 500ms * 2^(n-1).
		for i, d := range got {
			full := defaultBaseDelay << i
			if d < full/2 || d > full {
				t.Errorf("status %d: sleep %d = %v, want in [%v, %v]", status, i, d, full/2, full)
			}
		}
	}
}

// TestCreateRetriesOnlyUncommittedFailures: a repeated POST /assets reserves
// a second asset, so creation is retried only when nothing was reserved.
func TestCreateRetriesOnlyUncommittedFailures(t *testing.T) {
	retried := []testserver.Response{
		{Status: 429, Body: `{"message":"Too Many Attempts."}`},
		{Status: 503, Body: `{"code":"upload_service_unavailable","message":"Storage is unavailable."}`},
	}
	for _, resp := range retried {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteCreate, resp)
		c, sl := newTestClient(t, srv.URL)
		if _, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1}); err != nil {
			t.Fatalf("%d: %v", resp.Status, err)
		}
		if n := srv.Count(testserver.RouteCreate); n != 2 || len(sl.all()) != 1 {
			t.Fatalf("%d: creates = %d, sleeps = %d", resp.Status, n, len(sl.all()))
		}
	}

	notRetried := map[string]testserver.Response{
		"502":                       {Status: 502, Body: `<html>Bad Gateway</html>`, Header: map[string]string{"Content-Type": "text/html"}},
		"504":                       {Status: 504, Body: `{"message":"Gateway Timeout"}`},
		"503 without the code":      {Status: 503, Body: `{"message":"Service Unavailable"}`},
		"reserved, then dropped":    {CommitThenClose: true},
		"dropped after it was sent": {CloseConn: true},
	}
	for name, resp := range notRetried {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteCreate, resp)
		c, sl := newTestClient(t, srv.URL)
		_, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1})
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if n := srv.Count(testserver.RouteCreate); n != 1 || len(sl.all()) != 0 {
			t.Fatalf("%s: creates = %d, sleeps = %d; want 1 and 0", name, n, len(sl.all()))
		}
		var ne *NetworkError
		if resp.Status == 0 && (!errors.As(err, &ne) || !ne.MaybeCommitted || !strings.Contains(err.Error(), "not retried")) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// Nothing was retried, so at most one asset was reserved per attempt.
}

func TestCreateRetriesConnectionFailures(t *testing.T) {
	srv := testserver.New(t)
	var dials atomic.Int32
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	sl := &sleeps{}
	c, err := New(Options{BaseURL: srv.URL, APIKey: testserver.DefaultKey, UserAgent: "optimole-cli/test", Sleep: sl.sleep, Transport: tr})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if n := srv.Count(testserver.RouteCreate); n != 1 || len(sl.all()) != 1 || dials.Load() != 2 {
		t.Fatalf("creates = %d, sleeps = %d, dials = %d", n, len(sl.all()), dials.Load())
	}
}

func TestNetworkErrorsAreRetried(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteComplete, testserver.Response{CloseConn: true})
	c, sl := newTestClient(t, srv.URL)
	ctx := context.Background()
	created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: "a.txt", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := created.Target()
	if err := c.PutObject(ctx, target, strings.NewReader("abc"), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CompleteAsset(ctx, created.Asset.Key, nil); err != nil {
		t.Fatal(err)
	}
	if len(sl.all()) != 1 || srv.Count(testserver.RouteComplete) != 2 {
		t.Fatalf("sleeps = %v, completion calls = %d", sl.all(), srv.Count(testserver.RouteComplete))
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	srv := testserver.New(t)
	for range MaxAttempts + 2 {
		srv.Inject(testserver.RouteCreate, testserver.Response{Status: 503, Body: `{"code":"upload_service_unavailable","message":"Storage is unavailable."}`})
	}
	c, sl := newTestClient(t, srv.URL)
	_, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 || apiErr.Code != CodeUploadServiceUnavailable || apiErr.Attempts != MaxAttempts {
		t.Fatalf("err = %#v", err)
	}
	if n := srv.Count(testserver.RouteCreate); n != MaxAttempts {
		t.Fatalf("calls = %d, want %d", n, MaxAttempts)
	}
	if n := len(sl.all()); n != MaxAttempts-1 {
		t.Fatalf("sleeps = %d, want %d", n, MaxAttempts-1)
	}
	for _, d := range sl.all() {
		if d > defaultMaxDelay {
			t.Fatalf("sleep %v exceeds the cap %v", d, defaultMaxDelay)
		}
	}
	if !strings.Contains(err.Error(), "after 4 attempts") {
		t.Fatalf("error text %q does not mention the attempts", err)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	cases := []testserver.Response{
		{Status: 422, Body: `{"message":"The file type is not allowed.","errors":{"filename":["The file type is not allowed."]}}`},
		{Status: 403, Body: `{"code":"upload_limit_reached","message":"Upload limit reached.","limit":50000,"count":50000}`},
		{Status: 409, Body: `{"code":"upload_not_found","message":"Upload the file first."}`},
		{Status: 400, Body: `{"message":"Bad request."}`},
		{Status: 404, Body: `{"message":"Not found."}`},
		{Status: 500, Body: `{"message":"Server Error"}`},
	}
	for _, resp := range cases {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteCreate, resp)
		c, sl := newTestClient(t, srv.URL)
		_, err := c.CreateAsset(context.Background(), CreateAssetRequest{Filename: "a.png", Size: 1})
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != resp.Status {
			t.Fatalf("status %d: err = %v", resp.Status, err)
		}
		if n := srv.Count(testserver.RouteCreate); n != 1 || len(sl.all()) != 0 {
			t.Fatalf("status %d: %d calls, %d sleeps; want 1 and 0", resp.Status, n, len(sl.all()))
		}
	}
}

func TestErrorDetails(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteCreate,
		testserver.Response{Status: 403, Body: `{"code":"upload_limit_reached","message":"You have reached your upload limit.","limit":50000,"count":"50000"}`},
		testserver.Response{Status: 422, Body: `{"message":"The selected folder is invalid. (and 1 more error)","errors":{"folder_path":["The selected folder is invalid."],"size":"Too big."}}`},
		testserver.Response{Status: 502, Body: `<html>Bad Gateway</html>`, Header: map[string]string{"Content-Type": "text/html"}},
	)
	c, _ := newTestClient(t, srv.URL)
	req := CreateAssetRequest{Filename: "a.png", Size: 1}

	_, err := c.CreateAsset(context.Background(), req)
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Limit.Valid || apiErr.Limit.Value != 50000 || apiErr.Count.Value != 50000 {
		t.Fatalf("limit error = %#v", err)
	}
	if want := "create asset: You have reached your upload limit. (HTTP 403; upload_limit_reached; limit 50000, used 50000)"; err.Error() != want {
		t.Fatalf("got %q\nwant %q", err.Error(), want)
	}

	_, err = c.CreateAsset(context.Background(), req)
	if !errors.As(err, &apiErr) || !apiErr.HasField("folder_path") || apiErr.Fields["size"][0] != "Too big." {
		t.Fatalf("validation error = %#v", err)
	}
	if !strings.Contains(err.Error(), "folder_path: The selected folder is invalid.") || !strings.Contains(err.Error(), "size: Too big.") {
		t.Fatalf("validation text = %q", err)
	}

	// A non-JSON 502 is reported with its content type.
	_, err = c.CreateAsset(context.Background(), req)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 502 || !strings.Contains(err.Error(), "unexpected non-JSON response (text/html)") {
		t.Fatalf("502 error = %v", err)
	}
}

func TestCompletionErrors(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	created, err := c.CreateAsset(ctx, CreateAssetRequest{Filename: "a.txt", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Not uploaded yet: 409 upload_not_found, no retry.
	_, err = c.CompleteAsset(ctx, created.Asset.Key, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.Code != CodeUploadNotFound || srv.Count(testserver.RouteComplete) != 1 {
		t.Fatalf("err = %v", err)
	}
	srv.Inject(testserver.RouteComplete, testserver.Response{Status: 422, Body: `{"code":"upload_size_mismatch","message":"Size mismatch.","expected":3,"actual":2}`})
	_, err = c.CompleteAsset(ctx, created.Asset.Key, nil)
	if !errors.As(err, &apiErr) || apiErr.Expected.Value != 3 || apiErr.Actual.Value != 2 || !strings.Contains(err.Error(), "expected 3 bytes, storage has 2") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.CompleteAsset(ctx, "../../me", nil); err == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestDecodeAssetShapes(t *testing.T) {
	for _, body := range []string{
		`{"resourceS3":"k","url":"https://x/u","folder_id":12}`,
		`{"asset":{"resourceS3":"k","url":"https://x/u","folder_id":"12"}}`,
		`{"data":{"resourceS3":"k","url":"https://x/u","folder_id":12}}`,
	} {
		a, err := decodeAsset([]byte(body))
		if err != nil || a.URL != "https://x/u" || a.FolderID.Value != 12 {
			t.Errorf("decodeAsset(%s) = %+v, %v", body, a, err)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "https://elsewhere.example.com/api/folders", http.StatusMovedPermanently)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL+"/api")
	_, err := c.ListFolders(context.Background(), FolderQuery{})
	if err == nil || !strings.Contains(err.Error(), "unexpected redirect") || hits.Load() != 1 {
		t.Fatalf("err = %v, hits = %d", err, hits.Load())
	}
}

func TestCancelStopsRetries(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteFolders, testserver.Response{Status: 503, Body: `{}`})
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(Options{BaseURL: srv.URL, APIKey: testserver.DefaultKey, UserAgent: "optimole-cli/test",
		Sleep: func(ctx context.Context, d time.Duration) error { cancel(); return sleepContext(ctx, d) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListFolders(ctx, FolderQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := srv.Count(testserver.RouteFolders); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestListFoldersQuery(t *testing.T) {
	srv := testserver.New(t)
	srv.AddFolder("A/B")
	c, _ := newTestClient(t, srv.URL)
	page, err := c.ListFolders(context.Background(), FolderQuery{ParentID: "root", PerPage: 1, Cursor: "c0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 || page.Data[0].Name != "A" || page.Data[0].ParentID.Valid {
		t.Fatalf("page = %+v", page)
	}
	ev := srv.Events()[0]
	if ev.Query != "cursor=c0&parent_id=root&per_page=1" {
		t.Fatalf("query = %q", ev.Query)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"0":                             0,
		"-3":                            0,
		"12":                            12 * time.Second,
		"99999":                         maxRetryAfter,
		"Mon, 26 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Mon, 26 Oct 2026 11:00:00 GMT": 0,
		"soon":                          0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBackoffIsCapped(t *testing.T) {
	c := &Client{baseDelay: defaultBaseDelay, maxDelay: defaultMaxDelay}
	for attempt := 1; attempt <= 40; attempt++ {
		d := c.backoff(attempt)
		if d <= 0 || d > defaultMaxDelay {
			t.Fatalf("backoff(%d) = %v", attempt, d)
		}
	}
}
