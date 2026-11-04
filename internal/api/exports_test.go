package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

func TestExportDomainDecodesBothShapes(t *testing.T) {
	var e Export
	body := `{"id":7,"status":"running","domains":["example.com",{"domain":"directUpload","status":"completed","zip_files":[{"part_number":1,"url":"https://s3/x","size":"12"}]}]}`
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Domains) != 2 || e.Domains[0].Domain != "example.com" || e.Domains[0].Status != "" {
		t.Fatalf("name-only domain: %+v", e.Domains)
	}
	d := e.Domains[1]
	if d.Domain != "directUpload" || d.Status != "completed" || len(d.Parts) != 1 || d.Parts[0].Number.Value != 1 || d.Parts[0].Size.Value != 12 {
		t.Fatalf("domain object: %+v", d)
	}
	if err := json.Unmarshal([]byte(`{"domains":[42]}`), &e); err == nil {
		t.Fatal("a number is neither a name nor a domain object")
	}
}

func TestIsFinalStatus(t *testing.T) {
	for status, want := range map[string]bool{
		ExportQueued: false, ExportRunning: false, "": false,
		ExportCompleted: true, ExportFailed: true, ExportCancelled: true,
	} {
		if got := IsFinalStatus(status); got != want {
			t.Errorf("IsFinalStatus(%q) = %v", status, got)
		}
	}
}

func TestCreateExportConflicts(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()

	exp, err := c.CreateExport(ctx)
	if err != nil || exp.ID <= 0 || exp.Status != ExportQueued {
		t.Fatalf("create: %+v, %v", exp, err)
	}
	_, err = c.CreateExport(ctx)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != CodeExportInProgress || apiErr.ExportID.Value != exp.ID {
		t.Fatalf("in progress: %#v", err)
	}
	if !strings.Contains(err.Error(), "export_in_progress; export 1") {
		t.Errorf("message: %v", err)
	}

	srv2 := testserver.New(t)
	for i := 0; i < testserver.ExportLimit; i++ {
		srv2.AddExport(ExportCompleted)
	}
	c2, _ := newTestClient(t, srv2.URL)
	_, err = c2.CreateExport(ctx)
	if !errors.As(err, &apiErr) || apiErr.Code != CodeExportLimitReached || apiErr.Limit.Value != 5 || apiErr.Count.Value != 5 {
		t.Fatalf("limit: %#v", err)
	}

	srv2.LegacyExportConflicts = true
	_, err = c2.CreateExport(ctx)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "" || apiErr.Limit.Valid {
		t.Fatalf("legacy limit: %#v", err)
	}
}

func TestCreateExportIsNotRepeatedOnceItMayHaveStarted(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteExportCreate, testserver.Response{CommitThenClose: true})
	c, _ := newTestClient(t, srv.URL)
	_, err := c.CreateExport(context.Background())
	var netErr *NetworkError
	if !errors.As(err, &netErr) || !netErr.MaybeCommitted {
		t.Fatalf("got %v", err)
	}
	if n := srv.Count(testserver.RouteExportCreate); n != 1 {
		t.Errorf("create requests: %d, want 1", n)
	}
	if ids := srv.ExportIDs(); len(ids) != 1 {
		t.Errorf("exports: %v", ids)
	}
}

func TestDeleteExport(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	done := srv.AddExport(ExportCompleted)
	running := srv.AddExport(ExportRunning)

	var apiErr *Error
	if err := c.DeleteExport(ctx, running); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("running export: %v", err)
	}
	if err := c.DeleteExport(ctx, 999); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown export: %v", err)
	}
	// The first DELETE works but its answer is lost; the retry's 404 means
	// it is gone.
	srv.Inject(testserver.RouteExportDelete, testserver.Response{CommitThenClose: true})
	if err := c.DeleteExport(ctx, done); err != nil {
		t.Fatalf("delete after a lost answer: %v", err)
	}
	if ids := srv.ExportIDs(); len(ids) != 1 || ids[0] != running {
		t.Errorf("exports left: %v", ids)
	}
}

func TestListExportsQuery(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":3,"status":"completed","created_at":"2026-11-04T10:00:00.000000Z","domains":["a.com"]}],"meta":{"next_cursor":"abc","has_more":true}}`)
	}))
	defer ts.Close()
	c, _ := newTestClient(t, ts.URL+"/api")
	page, err := c.ListExports(context.Background(), ExportQuery{PerPage: 5, Cursor: "xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/api/asset-exports?cursor=xyz&per_page=5" {
		t.Errorf("request: %s", got)
	}
	if len(page.Data) != 1 || page.Data[0].Domains[0].Domain != "a.com" || !page.Meta.HasMore || *page.Meta.NextCursor != "abc" {
		t.Errorf("page: %+v", page)
	}
}

// archiveURL returns the URL of an export's first archive.
func archiveURL(t *testing.T, srv *testserver.Server, c *Client, data []byte) string {
	t.Helper()
	id := srv.AddExport(ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
	exp, err := c.GetExport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return exp.Domains[0].Parts[0].URL
}

func TestGetObjectRanges(t *testing.T) {
	srv := testserver.New(t)
	c, _ := newTestClient(t, srv.URL)
	ctx := context.Background()
	data := []byte("0123456789")
	u := archiveURL(t, srv, c, data)

	read := func(offset int64) (*Object, string) {
		t.Helper()
		obj, err := c.GetObject(ctx, u, offset)
		if err != nil {
			t.Fatalf("offset %d: %v", offset, err)
		}
		defer obj.Body.Close()
		b, _ := io.ReadAll(obj.Body)
		return obj, string(b)
	}
	if obj, body := read(0); obj.Offset != 0 || obj.Size != 10 || body != "0123456789" {
		t.Errorf("full: %+v %q", obj, body)
	}
	if obj, body := read(4); obj.Offset != 4 || obj.Size != 10 || body != "456789" {
		t.Errorf("range: %+v %q", obj, body)
	}
	_, err := c.GetObject(ctx, u, 10)
	var se *StorageError
	if !errors.As(err, &se) || se.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past the end: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "download archive: ") || strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Errorf("message: %v", err)
	}

	srv.Inject(testserver.RouteDownload, testserver.Response{IgnoreRange: true})
	if obj, body := read(4); obj.Offset != 0 || body != "0123456789" {
		t.Errorf("range ignored: %+v %q", obj, body)
	}

	srv.ExpireArchiveURLs()
	if _, err := c.GetObject(ctx, u, 0); !errors.As(err, &se) || se.StatusCode != http.StatusForbidden || se.Code != "AccessDenied" {
		t.Errorf("expired URL: %v", err)
	}
}

func TestGetObjectRefusals(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("storage GET carried an Authorization header")
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "https://elsewhere.example/x", http.StatusFound)
		case "/bad-range":
			w.Header().Set("Content-Range", "bytes 0-9/10")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "0123456789")
		}
	}))
	defer ts.Close()
	c, _ := newTestClient(t, ts.URL+"/api")
	ctx := context.Background()

	_, err := c.GetObject(ctx, ts.URL+"/redirect?X-Amz-Signature=secret", 0)
	if err == nil || !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), "secret") {
		t.Errorf("redirect: %v", err)
	}
	if _, err := c.GetObject(ctx, ts.URL+"/bad-range", 4); err == nil || !strings.Contains(err.Error(), "unexpected range") {
		t.Errorf("range that starts elsewhere: %v", err)
	}
	if _, err := c.GetObject(ctx, "http://storage.example/x", 0); err == nil || !strings.Contains(err.Error(), "refusing the download URL") {
		t.Errorf("plain http: %v", err)
	}
}
