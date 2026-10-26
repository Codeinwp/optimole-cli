package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func newClient(t *testing.T, srv *testserver.Server, key string) *api.Client {
	t.Helper()
	c, err := api.New(api.Options{BaseURL: srv.URL, APIKey: key, UserAgent: "optimole-cli/test", Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// run plans root (recursively) and uploads it, returning the results by path
// relative to root.
func run(t *testing.T, ctx context.Context, srv *testserver.Server, key string, root string, opts PlanOptions, r Runner) (map[string]Result, Outcome) {
	t.Helper()
	opts.Recursive = true
	plan, err := BuildPlan([]string{root}, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.Client = newClient(t, srv, key)
	results := map[string]Result{}
	var mu sync.Mutex
	r.OnResult = func(res Result) {
		mu.Lock()
		defer mu.Unlock()
		rel, _ := filepath.Rel(root, res.Item.Path)
		rel = filepath.ToSlash(rel)
		if _, dup := results[rel]; dup {
			t.Errorf("%s reported twice", rel)
		}
		results[rel] = res
	}
	out := r.Run(ctx, plan.Selected())
	if len(results) != len(plan.Selected()) {
		t.Errorf("%d results for %d files", len(results), len(plan.Selected()))
	}
	return results, out
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeBytes(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunHappyPath(t *testing.T) {
	srv := testserver.New(t)
	root := t.TempDir()
	img := pngBytes(t, 3, 2)
	writeBytes(t, root, "logo.png", img)
	writeBytes(t, root, "docs/readme.txt", []byte("hello"))
	srv.AddFolder("Brand")

	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{Folder: "brand"}, Runner{Concurrency: 2, CreateFolders: true})
	if out.Uploaded != 2 || out.Failed != 0 || out.Stopped != nil {
		t.Fatalf("outcome = %+v, results = %+v", out, results)
	}
	logo := results["logo.png"]
	if logo.Status != StatusUploaded || !strings.HasPrefix(logo.URL, "https://app.i.optimole.com/w:auto/h:auto/q:auto/id:"+logo.Key) || logo.FolderID == 0 {
		t.Fatalf("logo = %+v", logo)
	}
	assets := srv.CompletedByName()
	sum := sha256.Sum256(img)
	if a := assets["logo.png"]; a.SHA256 != hex.EncodeToString(sum[:]) || !a.HadHints || a.Width != 3 || a.Height != 2 {
		t.Fatalf("logo asset = %+v", a)
	}
	if a := assets["readme.txt"]; a.HadHints || srv.FolderPath(a.FolderID) != "Brand/docs" {
		t.Fatalf("readme asset = %+v in %q", a, srv.FolderPath(a.FolderID))
	}
	// "brand" matched the existing "Brand" case-insensitively; only docs was created.
	if got := srv.FolderPaths(); strings.Join(got, ",") != "Brand,Brand/docs" {
		t.Fatalf("folders = %v", got)
	}
	if n := srv.Count(testserver.RouteCreate) + srv.Count(testserver.RoutePut) + srv.Count(testserver.RouteComplete); n != 6 {
		t.Fatalf("requests = %d, want 6", n)
	}
}

// TestRunFolderFirstUnderConcurrency checks that the first upload into each
// destination completes before the others start, that the others send the
// folder ID, and that no folder is created twice even though the fake server
// creates folders without a lock.
func TestRunFolderFirstUnderConcurrency(t *testing.T) {
	srv := testserver.New(t)
	srv.FolderCreateDelay = 40 * time.Millisecond
	srv.CompleteDelay = 5 * time.Millisecond
	srv.PutDelay = 5 * time.Millisecond
	root := t.TempDir()
	for i := range 6 {
		writeBytes(t, root, fmt.Sprintf("alpha/a%d.jpg", i), []byte("jpeg-ish"))
		writeBytes(t, root, fmt.Sprintf("beta/sub/b%d.jpg", i), []byte("jpeg-ish"))
		writeBytes(t, root, fmt.Sprintf("r%d.jpg", i), []byte("jpeg-ish"))
	}

	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{Folder: "Campaigns"}, Runner{Concurrency: 8, CreateFolders: true})
	if out.Uploaded != 18 || out.Failed != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if got := strings.Join(srv.FolderPaths(), ","); got != "Campaigns,Campaigns/alpha,Campaigns/beta,Campaigns/beta/sub" {
		t.Fatalf("folders = %s (a folder was created twice)", got)
	}

	events := srv.Events()
	completedAt := map[string]int64{}
	for _, e := range events {
		if e.Route == testserver.RouteComplete && e.Status == http.StatusOK {
			completedAt[e.Key] = e.End
		}
	}
	creates := map[int64][]testserver.Event{}
	for _, e := range events {
		if e.Route == testserver.RouteCreate {
			creates[e.Resolved] = append(creates[e.Resolved], e)
		}
	}
	if len(creates) != 3 {
		t.Fatalf("destinations = %d, want 3", len(creates))
	}
	for folder, list := range creates {
		sort.Slice(list, func(i, j int) bool { return list[i].Start < list[j].Start })
		leader := list[0]
		if leader.FolderPath == "" || leader.FolderID != 0 || !leader.CreateFolders {
			t.Errorf("folder %d: leader request = %+v, want a folder_path with create_folders", folder, leader)
		}
		for _, e := range list[1:] {
			if e.Start < completedAt[leader.Key] {
				t.Errorf("folder %d: %s started (seq %d) before the leader completed (seq %d)", folder, e.Filename, e.Start, completedAt[leader.Key])
			}
			if e.FolderID != folder || e.FolderPath != "" || e.CreateFolders {
				t.Errorf("folder %d: follower request = %+v, want folder_id only", folder, e)
			}
		}
	}
	if n := srv.MaxConcurrentPuts(); n < 2 || n > 8 {
		t.Errorf("max concurrent PUTs = %d, want parallelism within the limit of 8", n)
	}
	if r := results["alpha/a3.jpg"]; r.FolderID == 0 || srv.FolderPath(r.FolderID) != "Campaigns/alpha" {
		t.Errorf("alpha/a3.jpg = %+v", r)
	}
}

// TestRunSiblingLeadersDoNotDuplicateParent covers destinations that share a
// missing parent but no files of their own there: "New/Deep/alpha" and
// "New/Deep/beta/sub" would both create "New" and "New/Deep".
func TestRunSiblingLeadersDoNotDuplicateParent(t *testing.T) {
	srv := testserver.New(t)
	srv.FolderCreateDelay = 40 * time.Millisecond
	root := t.TempDir()
	for i := range 4 {
		writeBytes(t, root, fmt.Sprintf("alpha/a%d.jpg", i), []byte("x"))
		writeBytes(t, root, fmt.Sprintf("beta/sub/b%d.jpg", i), []byte("x"))
		writeBytes(t, root, fmt.Sprintf("gamma/c%d.jpg", i), []byte("x"))
	}
	_, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{Folder: "New/Deep"}, Runner{Concurrency: 8, CreateFolders: true})
	if out.Uploaded != 12 {
		t.Fatalf("outcome = %+v", out)
	}
	want := "New,New/Deep,New/Deep/alpha,New/Deep/beta,New/Deep/beta/sub,New/Deep/gamma"
	if got := strings.Join(srv.FolderPaths(), ","); got != want {
		t.Fatalf("folders = %s\nwant      %s", got, want)
	}
}

func TestRunConcurrencyLimit(t *testing.T) {
	srv := testserver.New(t)
	srv.PutDelay = 5 * time.Millisecond
	root := t.TempDir()
	for i := range 12 {
		writeBytes(t, root, fmt.Sprintf("f%02d.txt", i), []byte("x"))
	}
	_, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{Concurrency: 3})
	if out.Uploaded != 12 {
		t.Fatalf("outcome = %+v", out)
	}
	if n := srv.MaxConcurrentPuts(); n > 3 {
		t.Fatalf("max concurrent PUTs = %d, want <= 3", n)
	}
}

func TestRunStorageFailures(t *testing.T) {
	denied := testserver.Response{Status: 403, Body: "<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>"}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the file was sent to the redirect target")
	}))
	defer other.Close()
	cases := []struct {
		name    string
		inject  []testserver.Response
		creates int
		puts    int
		ok      bool
	}{
		// The same URL is retried: the signature pins key, size and type.
		{"network error once", []testserver.Response{{CloseConn: true}}, 1, 2, true},
		{"stored, then the connection dropped", []testserver.Response{{CommitThenClose: true}}, 1, 2, true},
		{"500 once", []testserver.Response{{Status: 500, Body: "<Error><Code>InternalError</Code></Error>"}}, 1, 2, true},
		{"network errors on every attempt", []testserver.Response{{CloseConn: true}, {CloseConn: true}, {CloseConn: true}, {CloseConn: true}}, 1, 4, false},
		// Storage refused the URL: one new asset.
		{"403 once", []testserver.Response{denied}, 2, 2, true},
		{"403 twice", []testserver.Response{denied, denied}, 2, 2, false},
		// Neither.
		{"400", []testserver.Response{{Status: 400, Body: "<Error><Code>BadDigest</Code></Error>"}}, 1, 1, false},
		{"redirect", []testserver.Response{{Status: 307, Header: map[string]string{"Location": other.URL + "/x"}}}, 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testserver.New(t)
			srv.Inject(testserver.RoutePut, tc.inject...)
			root := t.TempDir()
			writeBytes(t, root, "a.jpg", []byte("abc"))
			results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{Concurrency: 1})
			if c, p := srv.Count(testserver.RouteCreate), srv.Count(testserver.RoutePut); c != tc.creates || p != tc.puts {
				t.Fatalf("creates = %d, PUTs = %d; want %d and %d", c, p, tc.creates, tc.puts)
			}
			res := results["a.jpg"]
			if tc.ok {
				if res.Status != StatusUploaded || out.Uploaded != 1 || len(srv.CompletedByName()) != 1 {
					t.Fatalf("result = %+v", res)
				}
				return
			}
			var se *api.StorageError
			if res.Status != StatusFailed || !errors.As(res.Err, &se) || srv.Count(testserver.RouteComplete) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
}

// TestRunExpiredAfterUncertainPut covers a PUT whose answer was lost and whose
// URL then expired: the CLI asks the server whether the bytes arrived before
// reserving a second asset, which the server's cleanup would also publish.
func TestRunExpiredAfterUncertainPut(t *testing.T) {
	t.Run("the bytes were stored", func(t *testing.T) {
		srv := testserver.New(t)
		srv.UploadExpiresIn = -time.Minute
		srv.Inject(testserver.RoutePut, testserver.Response{CommitThenClose: true})
		root := t.TempDir()
		writeBytes(t, root, "a.jpg", []byte("abc"))
		results, _ := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{})
		if r := results["a.jpg"]; r.Status != StatusUploaded || r.URL == "" {
			t.Fatalf("result = %+v", r)
		}
		if c := srv.Count(testserver.RouteCreate); c != 1 || len(srv.Assets()) != 1 {
			t.Fatalf("creates = %d, assets = %d; want 1 and 1", c, len(srv.Assets()))
		}
	})
	t.Run("the bytes were not stored", func(t *testing.T) {
		srv := testserver.New(t)
		srv.UploadExpiresIn = -time.Minute
		srv.Inject(testserver.RoutePut, testserver.Response{CloseConn: true})
		root := t.TempDir()
		writeBytes(t, root, "a.jpg", []byte("abc"))
		results, _ := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{})
		if r := results["a.jpg"]; r.Status != StatusUploaded {
			t.Fatalf("result = %+v", r)
		}
		// Completion said upload_not_found, so a second asset was safe.
		if c := srv.Count(testserver.RouteCreate); c != 2 || len(srv.CompletedByName()) != 1 {
			t.Fatalf("creates = %d, completed = %d", c, len(srv.CompletedByName()))
		}
	})
}

// TestRunCreateCommittedThenDropped: the server reserved the asset but the
// answer was lost. Repeating the request would reserve a second asset, which
// the server's cleanup could publish, so the file fails instead.
func TestRunCreateCommittedThenDropped(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteCreate, testserver.Response{CommitThenClose: true})
	root := t.TempDir()
	writeBytes(t, root, "a.jpg", []byte("abc"))
	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{})
	var ne *api.NetworkError
	if out.Failed != 1 || !errors.As(results["a.jpg"].Err, &ne) || !ne.MaybeCommitted {
		t.Fatalf("result = %+v", results["a.jpg"])
	}
	if c, a := srv.Count(testserver.RouteCreate), len(srv.Assets()); c != 1 || a != 1 {
		t.Fatalf("creates = %d, reserved assets = %d; want 1 and 1", c, a)
	}
}

func TestRunCompletionErrors(t *testing.T) {
	t.Run("409 is not retried", func(t *testing.T) {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteComplete, testserver.Response{Status: 409, Body: `{"code":"upload_not_found","message":"Upload the file first."}`})
		root := t.TempDir()
		writeBytes(t, root, "a.jpg", []byte("abc"))
		results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{})
		var apiErr *api.Error
		if out.Failed != 1 || !errors.As(results["a.jpg"].Err, &apiErr) || apiErr.Code != api.CodeUploadNotFound {
			t.Fatalf("result = %+v", results["a.jpg"])
		}
		if srv.Count(testserver.RouteComplete) != 1 || srv.Count(testserver.RouteCreate) != 1 {
			t.Fatal("409 was retried")
		}
	})
	t.Run("503 is retried", func(t *testing.T) {
		srv := testserver.New(t)
		srv.Inject(testserver.RouteComplete, testserver.Response{Status: 503, Body: `{"code":"upload_service_unavailable","message":"Try again."}`})
		root := t.TempDir()
		writeBytes(t, root, "a.jpg", []byte("abc"))
		results, _ := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{})
		if results["a.jpg"].Status != StatusUploaded || srv.Count(testserver.RouteComplete) != 2 {
			t.Fatalf("result = %+v", results["a.jpg"])
		}
	})
}

func TestRunInvalidFolderFailsTheGroupOnce(t *testing.T) {
	srv := testserver.New(t)
	root := t.TempDir()
	for i := range 4 {
		writeBytes(t, root, fmt.Sprintf("missing/m%d.jpg", i), []byte("x"))
	}
	writeBytes(t, root, "top.jpg", []byte("x"))
	srv.AddFolder("Existing")

	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{Folder: "Existing"}, Runner{Concurrency: 4})
	if out.Uploaded != 1 || out.Failed != 4 {
		t.Fatalf("outcome = %+v", out)
	}
	for i := range 4 {
		res := results[fmt.Sprintf("missing/m%d.jpg", i)]
		var apiErr *api.Error
		if !errors.As(res.Err, &apiErr) || apiErr.StatusCode != 422 || !strings.Contains(res.Err.Error(), "--create-folders") {
			t.Errorf("m%d = %v", i, res.Err)
		}
	}
	creates := 0
	for _, e := range srv.Events() {
		if e.Route == testserver.RouteCreate && e.FolderPath == "Existing/missing" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("creates into the invalid folder = %d, want 1", creates)
	}
}

func TestRunFileErrorPromotesNextLeader(t *testing.T) {
	srv := testserver.New(t)
	srv.Inject(testserver.RouteCreate, testserver.Response{Status: 422, Body: `{"message":"The filename is invalid.","errors":{"filename":["The filename is invalid."]}}`})
	root := t.TempDir()
	for i := range 3 {
		writeBytes(t, root, fmt.Sprintf("d/f%d.jpg", i), []byte("x"))
	}
	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{Concurrency: 1, CreateFolders: true})
	if out.Uploaded != 2 || out.Failed != 1 || results["d/f0.jpg"].Status != StatusFailed {
		t.Fatalf("outcome = %+v, results = %+v", out, results)
	}
	if strings.Contains(results["d/f0.jpg"].Err.Error(), "--create-folders") {
		t.Fatalf("filename error got the folder hint: %v", results["d/f0.jpg"].Err)
	}
}

func TestRunUploadLimitStopsTheRun(t *testing.T) {
	srv := testserver.New(t)
	srv.SetUploadLimit(10, 8)
	root := t.TempDir()
	for i := range 5 {
		writeBytes(t, root, fmt.Sprintf("f%d.jpg", i), []byte("x"))
	}
	results, out := run(t, context.Background(), srv, testserver.DefaultKey, root, PlanOptions{}, Runner{Concurrency: 1})
	if out.Uploaded != 2 || out.Failed != 3 || out.NotAttempted != 2 || out.Interrupted {
		t.Fatalf("outcome = %+v", out)
	}
	var apiErr *api.Error
	if !errors.As(out.Stopped, &apiErr) || apiErr.Code != api.CodeUploadLimitReached {
		t.Fatalf("stopped = %v", out.Stopped)
	}
	if msg := results["f2.jpg"].Err.Error(); !strings.Contains(msg, "limit 10, used 10") {
		t.Fatalf("f2 error = %q", msg)
	}
	for _, f := range []string{"f3.jpg", "f4.jpg"} {
		if r := results[f]; !r.NotAttempted || !errors.As(r.Err, &apiErr) {
			t.Fatalf("%s = %+v", f, r)
		}
	}
	if n := srv.Count(testserver.RouteCreate); n != 3 {
		t.Fatalf("creates = %d, want 3", n)
	}
}

func TestRunRejectedKeyStopsTheRun(t *testing.T) {
	srv := testserver.New(t)
	root := t.TempDir()
	for i := range 4 {
		writeBytes(t, root, fmt.Sprintf("f%d.jpg", i), []byte("x"))
	}
	_, out := run(t, context.Background(), srv, "optml-wrong-key-123456", root, PlanOptions{}, Runner{Concurrency: 1})
	if out.Failed != 4 || out.NotAttempted != 3 || srv.Count(testserver.RouteCreate) != 1 {
		t.Fatalf("outcome = %+v, creates = %d", out, srv.Count(testserver.RouteCreate))
	}
}

func TestRunScopedKey(t *testing.T) {
	srv := testserver.New(t)
	granted := srv.AddFolder("Shared")
	srv.AddFolder("Private")
	const scoped = "optml-sScopedKey0123456789"
	srv.AddKey(scoped, testserver.Key{Scoped: true, Granted: []int64{granted}})
	root := t.TempDir()
	writeBytes(t, root, "a.jpg", []byte("x"))
	writeBytes(t, root, "b.jpg", []byte("x"))

	// Into a granted folder: fine.
	_, out := run(t, context.Background(), srv, scoped, root, PlanOptions{Folder: "Shared"}, Runner{})
	if out.Uploaded != 2 {
		t.Fatalf("granted folder: %+v", out)
	}
	// Into the root: 403 for the destination, sent once.
	before := srv.Count(testserver.RouteCreate)
	results, out := run(t, context.Background(), srv, scoped, root, PlanOptions{}, Runner{})
	if out.Failed != 2 || out.NotAttempted != 0 || srv.Count(testserver.RouteCreate)-before != 1 {
		t.Fatalf("root: %+v", out)
	}
	if !strings.Contains(results["b.jpg"].Err.Error(), "HTTP 403") {
		t.Fatalf("b.jpg = %v", results["b.jpg"].Err)
	}
	// Into a folder it was not granted: the same 422 as a missing folder.
	_, out = run(t, context.Background(), srv, scoped, root, PlanOptions{Folder: "Private"}, Runner{})
	if out.Failed != 2 {
		t.Fatalf("private: %+v", out)
	}
}

func TestRunCancel(t *testing.T) {
	srv := testserver.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.Hook = func(route testserver.Route, _ *http.Request) {
		if route == testserver.RoutePut {
			cancel()
		}
	}
	root := t.TempDir()
	for i := range 4 {
		writeBytes(t, root, fmt.Sprintf("f%d.jpg", i), []byte("x"))
	}
	results, out := run(t, ctx, srv, testserver.DefaultKey, root, PlanOptions{}, Runner{Concurrency: 1})
	if !out.Interrupted || out.Uploaded != 0 || out.Failed != 4 || out.NotAttempted != 3 {
		t.Fatalf("outcome = %+v", out)
	}
	if r := results["f0.jpg"]; !errors.Is(r.Err, ErrInterrupted) || r.NotAttempted {
		t.Fatalf("in-flight file = %+v", r)
	}
	if r := results["f3.jpg"]; !errors.Is(r.Err, ErrInterrupted) || !r.NotAttempted {
		t.Fatalf("queued file = %+v", r)
	}
	if n := srv.Count(testserver.RouteCreate); n != 1 {
		t.Fatalf("creates = %d, want 1", n)
	}
}

func TestRunFileChangedSinceListing(t *testing.T) {
	srv := testserver.New(t)
	root := t.TempDir()
	writeBytes(t, root, "a.jpg", []byte("abc"))
	plan, err := BuildPlan([]string{root}, PlanOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.jpg"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var got Result
	r := Runner{Client: newClient(t, srv, testserver.DefaultKey), OnResult: func(res Result) { got = res }}
	r.Run(context.Background(), plan.Selected())
	if got.Status != StatusFailed || !strings.Contains(got.Err.Error(), "file changed since it was listed") || srv.Count(testserver.RouteCreate) != 0 {
		t.Fatalf("result = %+v", got)
	}
}

// TestRunRefusesFilesReplacedAfterPlanning: between planning and upload a
// file, or one of its directories, is swapped for something else. Nothing
// outside what was listed may be uploaded.
func TestRunRefusesFilesReplacedAfterPlanning(t *testing.T) {
	outside := t.TempDir()
	writeBytes(t, outside, "secret.jpg", []byte("SECRET"))
	writeBytes(t, outside, "sub/a.jpg", []byte("SECRET"))

	cases := []struct {
		name    string
		symlink bool
		swap    func(t *testing.T, root string)
		want    string
	}{
		{"file replaced by a symlink", true, func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "a.jpg"))
			mustSymlink(t, filepath.Join(outside, "secret.jpg"), filepath.Join(root, "a.jpg"))
		}, "replaced"},
		{"parent directory replaced by a symlink", true, func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "sub"))
			mustSymlink(t, filepath.Join(outside, "sub"), filepath.Join(root, "sub"))
		}, "replaced"},
		{"file replaced by another file", false, func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "a.jpg"))
			writeBytes(t, root, "a.jpg", []byte("OTHER!"))
		}, "replaced"},
		{"file grew", false, func(t *testing.T, root string) {
			writeBytes(t, root, "a.jpg", []byte("SECRET+more"))
		}, "changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeBytes(t, root, "a.jpg", []byte("public"))
			writeBytes(t, root, "sub/a.jpg", []byte("public"))
			if tc.symlink {
				probe := filepath.Join(root, "probe")
				if err := os.Symlink(root, probe); err != nil {
					t.Skipf("cannot create symlinks here: %v", err)
				}
				mustRemove(t, probe)
			}
			plan, err := BuildPlan([]string{root}, PlanOptions{Recursive: true})
			if err != nil {
				t.Fatal(err)
			}
			tc.swap(t, root)

			srv := testserver.New(t)
			results := map[string]Result{}
			r := Runner{Client: newClient(t, srv, testserver.DefaultKey), OnResult: func(res Result) {
				rel, _ := filepath.Rel(root, res.Item.Path)
				results[filepath.ToSlash(rel)] = res
			}}
			r.Run(context.Background(), plan.Selected())

			bad := "a.jpg"
			if strings.HasPrefix(tc.name, "parent") {
				bad = "sub/a.jpg"
			}
			if res := results[bad]; res.Status != StatusFailed || !strings.Contains(res.Err.Error(), tc.want) {
				t.Fatalf("%s = %+v", bad, res)
			}
			for name, a := range srv.CompletedByName() {
				if a.Size == 6 && a.SHA256 == sha256Hex("SECRET") {
					t.Fatalf("uploaded %s from outside the listed tree", name)
				}
			}
			if n := srv.Count(testserver.RouteCreate); n != 1 {
				t.Fatalf("creates = %d, want 1 (only the untouched file)", n)
			}
		})
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
