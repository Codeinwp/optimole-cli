package export

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

// zipOf builds a ZIP archive holding the given files.
func zipOf(t *testing.T, files ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(w, "content of %s, padded so that ranges have room to land: %s", name, strings.Repeat("x", 200))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fixture struct {
	t      *testing.T
	srv    *testserver.Server
	client *api.Client
	dir    string
	mu     sync.Mutex
	events []Event
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	srv := testserver.New(t)
	c, err := api.New(api.Options{BaseURL: srv.URL, APIKey: testserver.DefaultKey, UserAgent: "optimole-cli/test", Sleep: immediate})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, srv: srv, client: c, dir: filepath.Join(t.TempDir(), "optimole-export")}
}

func immediate(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func (f *fixture) run(ctx context.Context, id int64, domains ...string) Outcome {
	f.t.Helper()
	f.mu.Lock()
	f.events = nil
	f.mu.Unlock()
	return Run(ctx, Options{
		Client:      f.client,
		ExportID:    id,
		Dir:         f.dir,
		Domains:     domains,
		Concurrency: 2,
		Sleep:       immediate,
		OnEvent: func(e Event) {
			f.mu.Lock()
			f.events = append(f.events, e)
			f.mu.Unlock()
		},
	})
}

func (f *fixture) file(rel string) []byte {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

// downloads returns the archive requests in the order they started: the fake
// records a request when it ends, and a stalled one can end after its retry.
func (f *fixture) downloads() []testserver.Event {
	var out []testserver.Event
	for _, e := range f.srv.Events() {
		if e.Route == testserver.RouteDownload {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// partSizes maps each archive of example.com that has a partial file
// ("images-1.zip") to that file's size. It reads the size through an open
// file, which is current on every platform even while a download still
// writes to it.
func (f *fixture) partSizes() map[string]int64 {
	out := map[string]int64{}
	paths, _ := filepath.Glob(filepath.Join(f.dir, "example.com", "*"+partSuffix))
	for _, p := range paths {
		name := filepath.Base(p)
		i := strings.Index(name, ".zip.")
		file, err := os.Open(p)
		if i < 0 || err != nil {
			continue
		}
		st, err := file.Stat()
		file.Close()
		if err == nil {
			out[name[:i+len(".zip")]] = st.Size()
		}
	}
	return out
}

func TestRunDownloadsArchivesWhileTheExportRuns(t *testing.T) {
	f := newFixture(t)
	a1, a2, d1 := zipOf(t, "example.com/a.jpg"), zipOf(t, "example.com/b.jpg"), zipOf(t, "c.png")
	f.srv.SetExportDomains(
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{a1, a2}},
		testserver.ExportDomain{Domain: "directUpload", Archives: [][]byte{d1}},
	)
	exp, err := f.client.CreateExport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := f.run(context.Background(), exp.ID)
	if out.Err != nil || out.Downloaded != 3 || out.Present != 0 || out.Failed != 0 {
		t.Fatalf("outcome: %+v", out)
	}
	if out.Export.Status != api.ExportCompleted || out.Bytes != int64(len(a1)+len(a2)+len(d1)) {
		t.Errorf("export %s, bytes %d", out.Export.Status, out.Bytes)
	}
	for rel, want := range map[string][]byte{"example.com/images-1.zip": a1, "example.com/images-2.zip": a2, "directUpload/images-1.zip": d1} {
		if !bytes.Equal(f.file(rel), want) {
			t.Errorf("%s differs", rel)
		}
	}
	// The first archive started before the export finished.
	started, completed := -1, -1
	for i, e := range f.events {
		if e.Kind == PartStarted && started < 0 {
			started = i
		}
		if e.Kind == ExportStatus && e.Status == api.ExportCompleted {
			completed = i
		}
	}
	if started < 0 || completed < 0 || started > completed {
		t.Errorf("first download at event %d, export completed at %d", started, completed)
	}
	for i, e := range f.events {
		if e.Kind == DomainStatus && i > completed {
			t.Errorf("domain event %d (%s %s) after the export completed (%d)", i, e.Domain, e.Status, completed)
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(f.dir, "*", "*"+partSuffix)); len(leftovers) > 0 {
		t.Errorf("partial files left: %v", leftovers)
	}
}

func TestRunSkipsArchivesAlreadyDownloaded(t *testing.T) {
	f := newFixture(t)
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a"), zipOf(t, "b")}})
	if out := f.run(context.Background(), id); out.Downloaded != 2 {
		t.Fatalf("first run: %+v", out)
	}
	before := len(f.downloads())
	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 0 || out.Present != 2 || out.Bytes != 0 {
		t.Fatalf("second run: %+v", out)
	}
	if n := len(f.downloads()); n != before {
		t.Errorf("second run downloaded again (%d requests)", n-before)
	}
	for _, e := range f.events {
		if e.Kind == PartStarted {
			t.Errorf("download started for an archive on disk: %+v", e)
		}
	}
}

func TestRunResumesAPartialFile(t *testing.T) {
	f := newFixture(t)
	data := zipOf(t, "a.jpg", "b.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
	final := filepath.Join(f.dir, "example.com", "images-1.zip")
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := tempPath(job{path: final, size: int64(len(data))})
	if err := os.WriteFile(tmp, data[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	// A partial file of another version of the archive is removed.
	stale := final + ".12345" + partSuffix
	os.WriteFile(stale, []byte("old"), 0o644)

	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 1 || out.Bytes != int64(len(data)-100) {
		t.Fatalf("outcome: %+v", out)
	}
	if !bytes.Equal(f.file("example.com/images-1.zip"), data) {
		t.Error("resumed archive differs")
	}
	if d := f.downloads(); len(d) != 1 || d[0].Range != "bytes=100-" {
		t.Errorf("downloads: %+v", d)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale partial file kept: %v", err)
	}
}

func TestRunRecoversFromStorageFailures(t *testing.T) {
	data := zipOf(t, "a.jpg", "b.jpg")
	cases := []struct {
		name      string
		responses []testserver.Response
		ranges    []string
	}{
		{"connection cut mid-body", []testserver.Response{{CutAfter: 50}}, []string{"", "bytes=50-"}},
		{"server error, then cut, then success", []testserver.Response{{Status: http.StatusInternalServerError}, {CutAfter: 30}}, []string{"", "", "bytes=30-"}},
		{"range ignored on resume", []testserver.Response{{CutAfter: 40}, {IgnoreRange: true}}, []string{"", "bytes=40-"}},
		{"expired URL", []testserver.Response{{Status: http.StatusForbidden, Body: "<Error><Code>AccessDenied</Code></Error>"}}, []string{"", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
			f.srv.Inject(testserver.RouteDownload, tc.responses...)
			out := f.run(context.Background(), id)
			if out.Err != nil || out.Downloaded != 1 || out.Failed != 0 {
				t.Fatalf("outcome: %+v (events %+v)", out, f.events)
			}
			if !bytes.Equal(f.file("example.com/images-1.zip"), data) {
				t.Error("archive differs")
			}
			var ranges []string
			for _, d := range f.downloads() {
				ranges = append(ranges, d.Range)
			}
			if strings.Join(ranges, "|") != strings.Join(tc.ranges, "|") {
				t.Errorf("ranges %q, want %q", ranges, tc.ranges)
			}
		})
	}
}

func TestRunGetsAFreshURLWhenTheArchiveChanges(t *testing.T) {
	old := zipOf(t, "a.jpg", "b.jpg")
	for name, rewritten := range map[string][]byte{"bigger": zipOf(t, "a.jpg", "b.jpg", "c.jpg"), "smaller": zipOf(t, "a.jpg")} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{old}})
			// The archive is rewritten between the listing and the download.
			var once sync.Once
			f.srv.Hook = func(route testserver.Route, _ *http.Request) {
				if route == testserver.RouteDownload {
					once.Do(func() { f.srv.RewriteArchive(id, "example.com", 1, rewritten) })
				}
			}
			out := f.run(context.Background(), id)
			if out.Err != nil || out.Downloaded != 1 {
				t.Fatalf("outcome: %+v", out)
			}
			if !bytes.Equal(f.file("example.com/images-1.zip"), rewritten) {
				t.Error("kept the old version")
			}
			if n, d := f.srv.Count(testserver.RouteExportGet), len(f.downloads()); n != 2 || d != 2 {
				t.Errorf("looks at the export: %d, downloads %d, want 2 and 2", n, d)
			}
		})
	}
}

func TestRunDownloadsAnArchiveAgainWhenItsSizeChanged(t *testing.T) {
	f := newFixture(t)
	a, b := zipOf(t, "a.jpg"), zipOf(t, "b.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{a, b}})
	f.run(context.Background(), id)
	bigger := zipOf(t, "b.jpg", "c.jpg")
	f.srv.RewriteArchive(id, "example.com", 2, bigger)
	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 1 || out.Present != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	if !bytes.Equal(f.file("example.com/images-2.zip"), bigger) {
		t.Error("archive 2 not replaced")
	}
}

func TestRunGivesUpOnAnArchiveThatIsNotAZip(t *testing.T) {
	f := newFixture(t)
	good := zipOf(t, "a.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{[]byte("not a zip at all"), good}})
	out := f.run(context.Background(), id)
	if out.Downloaded != 1 || out.Failed != 1 || out.Err != nil {
		t.Fatalf("outcome: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "example.com", "images-1.zip")); !os.IsNotExist(err) {
		t.Errorf("broken archive kept: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(f.dir, "example.com", "*"+partSuffix)); len(leftovers) > 0 {
		t.Errorf("partial files left: %v", leftovers)
	}
	var failedErr error
	for _, e := range f.events {
		if e.Kind == PartDone && e.Result == Failed {
			failedErr = e.Err
		}
	}
	if failedErr == nil || !strings.Contains(failedErr.Error(), "not a valid ZIP") {
		t.Errorf("failure: %v", failedErr)
	}
	tries := 0
	for _, d := range f.downloads() {
		if strings.HasSuffix(d.Key, "/1") {
			tries++
		}
	}
	if tries != 2 {
		t.Errorf("downloads of the broken archive: %d, want 2", tries)
	}
}

func TestRunDoesNotRetryWhatCannotSucceed(t *testing.T) {
	data := zipOf(t, "a.jpg")
	t.Run("missing archive", func(t *testing.T) {
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
		f.srv.Inject(testserver.RouteDownload, testserver.Response{Status: http.StatusNotFound, Body: "<Error><Code>NoSuchKey</Code></Error>"})
		if out := f.run(context.Background(), id); out.Failed != 1 || len(f.downloads()) != 1 {
			t.Fatalf("outcome %+v after %d downloads", out, len(f.downloads()))
		}
	})
	t.Run("unwritable folder", func(t *testing.T) {
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
		os.MkdirAll(f.dir, 0o755)
		os.WriteFile(filepath.Join(f.dir, "example.com"), []byte("a file, not a folder"), 0o644)
		out := f.run(context.Background(), id)
		if out.Failed != 1 || len(f.downloads()) != 0 {
			t.Fatalf("outcome %+v after %d downloads", out, len(f.downloads()))
		}
	})
	t.Run("URL refused every time", func(t *testing.T) {
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
		var denials []testserver.Response
		for i := 0; i < 10; i++ {
			denials = append(denials, testserver.Response{Status: http.StatusForbidden})
		}
		f.srv.Inject(testserver.RouteDownload, denials...)
		out := f.run(context.Background(), id)
		if out.Failed != 1 || len(f.downloads()) != maxURLRefreshes+1 {
			t.Fatalf("outcome %+v after %d downloads", out, len(f.downloads()))
		}
	})
}

func TestRunRestartsAStalledDownload(t *testing.T) {
	saved := stallTimeout
	stallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stallTimeout = saved })
	f := newFixture(t)
	data := zipOf(t, "a.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
	f.srv.Inject(testserver.RouteDownload, testserver.Response{StallAfter: 60})
	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	// The stalled request is recorded once its handler sees the cancel,
	// which can be after the run returns.
	d := f.downloads()
	for deadline := time.Now().Add(2 * time.Second); len(d) < 2 && time.Now().Before(deadline); d = f.downloads() {
		time.Sleep(5 * time.Millisecond)
	}
	if len(d) != 2 || d[1].Range != "bytes=60-" {
		t.Errorf("downloads: %+v", d)
	}
}

func TestRunKeepsTheOtherDomainsOfAFailedExport(t *testing.T) {
	f := newFixture(t)
	f.srv.SetExportDomains(
		testserver.ExportDomain{Domain: "broken.com", Fail: true},
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a.jpg")}},
	)
	exp, _ := f.client.CreateExport(context.Background())
	out := f.run(context.Background(), exp.ID)
	if out.Err != nil || out.Export.Status != api.ExportFailed || out.Downloaded != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	var failedDomain bool
	for _, e := range f.events {
		if e.Kind == DomainStatus && e.Domain == "broken.com" && e.Status == api.ExportFailed {
			failedDomain = true
		}
	}
	if !failedDomain {
		t.Errorf("no failed-domain event: %+v", f.events)
	}
}

func TestRunDomainFilter(t *testing.T) {
	f := newFixture(t)
	id := f.srv.AddExport(api.ExportCompleted,
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a")}},
		testserver.ExportDomain{Domain: "other.com", Archives: [][]byte{zipOf(t, "b")}},
	)
	out := f.run(context.Background(), id, "EXAMPLE.com")
	if out.Err != nil || out.Downloaded != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "other.com")); !os.IsNotExist(err) {
		t.Errorf("other.com downloaded: %v", err)
	}

	g := newFixture(t)
	id = g.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a")}})
	out = g.run(context.Background(), id, "nope.com")
	var unknown *UnknownDomainError
	if !errors.As(out.Err, &unknown) || unknown.Domain != "nope.com" || strings.Join(unknown.Known, ",") != "example.com" {
		t.Fatalf("unknown domain: %v", out.Err)
	}
	if _, err := os.Stat(g.dir); !os.IsNotExist(err) {
		t.Errorf("wrote into the folder for an unknown domain: %v", err)
	}
}

func TestRunKeepsHostileDomainNamesInsideTheFolder(t *testing.T) {
	f := newFixture(t)
	domains := []string{"../evil", "a/b", `c:\d`, "host:8080", "*.example.com", "CON", "con.example", "Example.com", "example.com", ".hidden", ".."}
	var spec []testserver.ExportDomain
	for _, d := range domains {
		spec = append(spec, testserver.ExportDomain{Domain: d, Archives: [][]byte{zipOf(t, "x")}})
	}
	id := f.srv.AddExport(api.ExportCompleted, spec...)
	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != len(domains) {
		t.Fatalf("outcome: %+v", out)
	}
	seen := map[string]bool{}
	for _, d := range domains {
		name := out.Folders[d]
		if name == "" || strings.ContainsAny(name, `/\:*?"<>|`) || strings.HasPrefix(name, ".") || seen[strings.ToLower(name)] {
			t.Errorf("%q -> folder %q", d, name)
		}
		seen[strings.ToLower(name)] = true
	}
	entries, _ := os.ReadDir(f.dir)
	if len(entries) != len(domains)+2 { // + export.json and the lock
		t.Errorf("%d entries in the export folder, want %d", len(entries), len(domains)+2)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.dir), "evil")); !os.IsNotExist(err) {
		t.Errorf("wrote outside the export folder: %v", err)
	}
}

func TestFolderNames(t *testing.T) {
	var m folderMap
	m.add([]string{"example.com", "Example.com", "CON", "nul.txt", "*.x.com", "host:8080", "..", "", "bücher.de", strings.Repeat("a", 300)})
	want := map[string]string{
		"Example.com": "Example.com",
		"example.com": "example.com-2",
		"CON":         "_CON",
		"nul.txt":     "_nul.txt",
		"*.x.com":     "_.x.com",
		"host:8080":   "host_8080",
		"":            "domain",
		"..":          "domain-2",
		"bücher.de":   "bücher.de",
	}
	for d, name := range want {
		if m.names[d] != name {
			t.Errorf("%q -> %q, want %q", d, m.names[d], name)
		}
	}
	if got := m.names[strings.Repeat("a", 300)]; len(got) != maxFolderBytes {
		t.Errorf("long name kept %d bytes", len(got))
	}
	// The cap counts bytes: 50 three-byte runes would be 150.
	wide := strings.Repeat("界", 50) + ".com"
	m.add([]string{wide})
	if got := m.names[wide]; len(got) > maxFolderBytes || !utf8.ValidString(got) || !strings.HasPrefix(got, "界界") {
		t.Errorf("multibyte name -> %q (%d bytes)", got, len(got))
	}
	// Domains seen later never take an earlier domain's folder.
	m.add([]string{"EXAMPLE.COM"})
	if m.names["EXAMPLE.COM"] != "EXAMPLE.COM-3" || m.names["example.com"] != "example.com-2" {
		t.Errorf("later domain: %v", m.names)
	}
}

func TestRunWritesAManifestWithoutURLs(t *testing.T) {
	f := newFixture(t)
	id := f.srv.AddExport(api.ExportCompleted,
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a"), []byte("broken")}},
		testserver.ExportDomain{Domain: "skipped.com", Archives: [][]byte{zipOf(t, "b")}},
	)
	f.run(context.Background(), id, "example.com")
	raw := f.file(ManifestName)
	if bytes.Contains(raw, []byte("X-Amz-Signature")) || bytes.Contains(raw, []byte("http")) {
		t.Errorf("manifest holds a URL: %s", raw)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != id || m.Status != api.ExportCompleted || len(m.Domains) != 2 {
		t.Fatalf("manifest: %+v", m)
	}
	ex := m.Domains[0]
	if ex.Folder != "example.com" || len(ex.Archives) != 2 || !ex.Archives[0].Downloaded || ex.Archives[1].Downloaded || ex.Archives[0].File != "example.com/images-1.zip" {
		t.Errorf("example.com: %+v", ex)
	}
	if sk := m.Domains[1]; len(sk.Archives) != 1 || sk.Archives[0].Downloaded {
		t.Errorf("skipped.com: %+v", sk)
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	f := newFixture(t)
	f.srv.PauseExports(true)
	id := f.srv.AddExport(api.ExportRunning, testserver.ExportDomain{Domain: "example.com"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out := f.run(ctx, id)
	if !errors.Is(out.Err, context.DeadlineExceeded) || out.Export == nil || out.Export.Status != api.ExportRunning {
		t.Fatalf("outcome: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ManifestName)); err != nil {
		t.Errorf("no manifest: %v", err)
	}
}

func TestRunReportsAMissingExport(t *testing.T) {
	f := newFixture(t)
	out := f.run(context.Background(), 404)
	var apiErr *api.Error
	if !errors.As(out.Err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || out.Export != nil {
		t.Fatalf("outcome: %+v", out)
	}
}

// newRunner returns a runner on the fixture, for tests of single steps.
func (f *fixture) newRunner() *runner {
	return &runner{client: f.client, opts: Options{Dir: f.dir, Concurrency: 1}, sleep: immediate, domainStatus: map[string]string{}, parts: map[partKey]*part{}}
}

func TestDownloadRefusesAnArchiveLongerThanListed(t *testing.T) {
	data := zipOf(t, "a.jpg")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Content-Length: the body is chunked.
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		w.(http.Flusher).Flush()
		w.Write([]byte("more than the export listed"))
	}))
	defer storage.Close()
	f := newFixture(t)
	path := filepath.Join(f.dir, "example.com", "images-1.zip")
	res := f.newRunner().download(context.Background(), job{path: path, url: storage.URL + "/x", size: int64(len(data))})
	if !errors.Is(res.err, errNeedURL) {
		t.Fatalf("got %+v", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("archive put in place: %v", err)
	}
}

func TestDownloadEmptiesAPartialFileLongerThanTheArchive(t *testing.T) {
	f := newFixture(t)
	data := zipOf(t, "a.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
	exp, err := f.client.GetExport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir, "example.com", "images-1.zip")
	j := job{path: path, url: exp.Domains[0].Parts[0].URL, size: -1}
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(tempPath(j), bytes.Repeat([]byte("x"), len(data)+10), 0o644)
	res := f.newRunner().download(context.Background(), j)
	if res.err != nil || res.size != int64(len(data)) || !bytes.Equal(f.file("example.com/images-1.zip"), data) {
		t.Fatalf("got %+v", res)
	}
	if d := f.downloads(); len(d) != 2 || d[0].Range == "" || d[1].Range != "" {
		t.Errorf("downloads: %+v", d)
	}
}

func TestFinishQueuesAnArchiveListedWithANewSizeDuringItsDownload(t *testing.T) {
	f := newFixture(t)
	r := f.newRunner()
	p := &part{key: partKey{"example.com", 1}, size: 200, state: active}
	r.finish(context.Background(), finished{part: p, res: jobResult{size: 100}})
	if p.state != pending || len(r.queue) != 1 {
		t.Errorf("state %v, queue %d", p.state, len(r.queue))
	}
}

func TestApplyFailsAWaitingArchiveTheFinalExportNoLongerLists(t *testing.T) {
	f := newFixture(t)
	r := f.newRunner()
	r.exp = &api.Export{ID: 1, Status: api.ExportRunning}
	waiting := &part{key: partKey{"example.com", 2}, state: needURL}
	r.parts[waiting.key] = waiting
	if err := r.apply(&api.Export{ID: 1, Status: api.ExportCompleted, Domains: []api.ExportDomain{{Domain: "example.com", Status: api.ExportCompleted}}}); err != nil {
		t.Fatal(err)
	}
	if waiting.state != failed {
		t.Errorf("state %v", waiting.state)
	}
}

func TestUpdateQueuesADownloadedArchiveListedWithANewSize(t *testing.T) {
	f := newFixture(t)
	r := f.newRunner()
	key := partKey{"example.com", 1}
	p := &part{key: key, size: 100, doneSize: 100, state: done}
	r.parts[key] = p
	r.update(key, "https://storage/new", 100)
	if p.state != done || len(r.queue) != 0 {
		t.Fatalf("same size: state %v, queue %d", p.state, len(r.queue))
	}
	r.update(key, "https://storage/new", 200)
	if p.state != pending || len(r.queue) != 1 || p.url != "https://storage/new" {
		t.Errorf("new size: state %v, queue %d", p.state, len(r.queue))
	}
}

func TestFetchNamesAStalledDownload(t *testing.T) {
	saved := stallTimeout
	stallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stallTimeout = saved })
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer storage.Close()
	f := newFixture(t)
	os.MkdirAll(f.dir, 0o755)
	n, err := f.newRunner().fetchOnce(context.Background(), job{path: filepath.Join(f.dir, "a.zip"), url: storage.URL + "/x", size: 100}, filepath.Join(f.dir, "a.zip.part"))
	if !errors.Is(err, errStalled) || n != 10 {
		t.Fatalf("got %d bytes, %v", n, err)
	}
}

// storedZip builds an uncompressed ZIP, so contents of the same length give
// archives of the same size.
func storedZip(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRunDetectsAResumeAcrossASameSizeRewrite(t *testing.T) {
	f := newFixture(t)
	old := storedZip(t, "a.jpg", strings.Repeat("o", 300))
	rewritten := storedZip(t, "a.jpg", strings.Repeat("n", 300))
	if len(old) != len(rewritten) {
		t.Fatal("fixture archives differ in size")
	}
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{old}})
	// The first download is cut inside the file data; the archive is then
	// rewritten with the same size, so the resume joins two versions.
	f.srv.Inject(testserver.RouteDownload, testserver.Response{CutAfter: 100})
	var once sync.Once
	f.srv.Hook = func(route testserver.Route, r *http.Request) {
		if route == testserver.RouteDownload && r.Header.Get("Range") != "" {
			once.Do(func() { f.srv.RewriteArchive(id, "example.com", 1, rewritten) })
		}
	}
	out := f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 1 {
		t.Fatalf("outcome: %+v", out)
	}
	if !bytes.Equal(f.file("example.com/images-1.zip"), rewritten) {
		t.Error("kept a mix of two versions")
	}
	var ranges []string
	for _, d := range f.downloads() {
		ranges = append(ranges, d.Range)
	}
	if strings.Join(ranges, "|") != "|bytes=100-|" {
		t.Errorf("ranges %q: want a resume, then a fresh download", ranges)
	}
}

func TestDownloadRefusesAnArchiveOfUnknownSize(t *testing.T) {
	requests := 0
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // chunked: no Content-Length
		w.Write(bytes.Repeat([]byte("x"), 1<<16))
	}))
	defer storage.Close()
	f := newFixture(t)
	path := filepath.Join(f.dir, "example.com", "images-1.zip")
	res := f.newRunner().download(context.Background(), job{path: path, url: storage.URL + "/x", size: -1})
	if !errors.Is(res.err, errUnknownSize) || requests != 1 || res.written != 0 {
		t.Fatalf("got %+v after %d requests", res, requests)
	}
}

func TestRunCancelsDownloadsWhenTheExportCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.srv.SetExportDomains(testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a"), zipOf(t, "b")}})
	exp, _ := f.client.CreateExport(context.Background())
	// The first download stalls (for good: the stall timeout is minutes);
	// meanwhile the export stays running and every look at it fails.
	f.srv.Inject(testserver.RouteDownload, testserver.Response{StallAfter: 20})
	var once sync.Once
	gets := 0
	f.srv.Hook = func(route testserver.Route, _ *http.Request) {
		if route == testserver.RouteExportGet {
			// The second look listed the first archive; the export stays
			// running from the third on.
			f.mu.Lock()
			if gets++; gets == 3 {
				f.srv.PauseExports(true)
			}
			f.mu.Unlock()
		}
		if route == testserver.RouteDownload {
			once.Do(func() {
				var errs []testserver.Response
				for i := 0; i < 20; i++ {
					errs = append(errs, testserver.Response{Status: http.StatusInternalServerError, Body: `{"message":"Server Error"}`})
				}
				f.srv.Inject(testserver.RouteExportGet, errs...)
			})
		}
	}
	done := make(chan Outcome, 1)
	go func() { done <- f.run(context.Background(), exp.ID) }()
	select {
	case out := <-done:
		var apiErr *api.Error
		if !errors.As(out.Err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError || out.Failed != 0 {
			t.Fatalf("outcome: %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run waited for the stalled download")
	}
	parts, _ := filepath.Glob(filepath.Join(f.dir, "example.com", "*"+partSuffix))
	if len(parts) != 1 {
		t.Errorf("partial files: %v", parts)
	}
}

func TestRunResumesDownloadsCancelledMidway(t *testing.T) {
	f := newFixture(t)
	a, b := zipOf(t, "a"), zipOf(t, "b")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{a, b}})
	f.srv.Inject(testserver.RouteDownload, testserver.Response{StallAfter: 30}, testserver.Response{StallAfter: 40})
	// Whichever archive starts first stalls after 30 bytes, the other after
	// 40. The run is cancelled once both partial files hold those bytes,
	// however long the downloads take to get there.
	stalled := func(parts map[string]int64) bool {
		one, two := parts["images-1.zip"], parts["images-2.zip"]
		return len(parts) == 2 && (one == 30 && two == 40 || one == 40 && two == 30)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{})
	go func() {
		defer cancel()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !stalled(f.partSizes()); {
			select {
			case <-ran:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	out := f.run(ctx, id)
	close(ran)
	if !errors.Is(out.Err, context.Canceled) || out.Failed != 0 || out.Downloaded != 0 {
		t.Fatalf("cancelled run: %+v", out)
	}
	parts := f.partSizes()
	if !stalled(parts) {
		t.Fatalf("partial files: %v", parts)
	}

	// The fake files the cancelled downloads only once it notices the
	// client left, which can be after Run returned.
	f.srv.WaitIdle(t)
	before := len(f.downloads())
	out = f.run(context.Background(), id)
	if out.Err != nil || out.Downloaded != 2 {
		t.Fatalf("resumed run: %+v", out)
	}
	if !bytes.Equal(f.file("example.com/images-1.zip"), a) || !bytes.Equal(f.file("example.com/images-2.zip"), b) {
		t.Error("resumed archives differ")
	}
	resumed := f.downloads()[before:]
	if len(resumed) != 2 {
		t.Fatalf("resumed downloads: %+v", resumed)
	}
	for _, d := range resumed {
		archive := "images-" + d.Key[strings.LastIndex(d.Key, "/")+1:] + ".zip"
		if want := fmt.Sprintf("bytes=%d-", parts[archive]); d.Range != want {
			t.Errorf("%s resumed with Range %q, want %q: %+v", archive, d.Range, want, d)
		}
	}
}

func TestRunLimitsParallelDownloads(t *testing.T) {
	f := newFixture(t)
	var archives [][]byte
	for i := 0; i < 7; i++ {
		archives = append(archives, zipOf(t, fmt.Sprintf("f%d", i)))
	}
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: archives})

	// The first three downloads wait until all three have arrived, then a
	// little longer, so a fourth one would be seen if the limit let it start.
	const limit = 3
	var mu sync.Mutex
	arrived, waiting, most := 0, 0, 0
	all := make(chan struct{})
	f.srv.Hook = func(route testserver.Route, _ *http.Request) {
		if route != testserver.RouteDownload {
			return
		}
		mu.Lock()
		arrived++
		first := arrived <= limit
		if arrived == limit {
			close(all)
		}
		waiting++
		most = max(most, waiting)
		mu.Unlock()
		if first {
			select {
			case <-all:
				time.Sleep(50 * time.Millisecond)
			case <-time.After(5 * time.Second):
			}
		}
		mu.Lock()
		waiting--
		mu.Unlock()
	}
	out := Run(context.Background(), Options{Client: f.client, ExportID: id, Dir: f.dir, Concurrency: limit, Sleep: immediate})
	if out.Err != nil || out.Downloaded != 7 {
		t.Fatalf("outcome: %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if most != limit {
		t.Errorf("at most %d downloads at once, want %d", most, limit)
	}
}

func TestRunRefusesAFolderAnotherRunHolds(t *testing.T) {
	f := newFixture(t)
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{zipOf(t, "a")}})
	unlock, err := lockDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	out := f.run(context.Background(), id)
	if !errors.Is(out.Err, errLocked) || len(f.downloads()) != 0 {
		t.Fatalf("outcome %+v after %d downloads", out, len(f.downloads()))
	}
	if _, err := os.Stat(filepath.Join(f.dir, ManifestName)); !os.IsNotExist(err) {
		t.Errorf("wrote the manifest of a folder another run holds: %v", err)
	}
	unlock()
	if out := f.run(context.Background(), id); out.Err != nil || out.Downloaded != 1 {
		t.Errorf("after the lock was released: %+v", out)
	}
}

func TestRunManifestKeepsArchivesOfUnselectedDomains(t *testing.T) {
	for _, sizeless := range []bool{false, true} {
		f := newFixture(t)
		f.srv.OmitArchiveSizes = sizeless
		id := f.srv.AddExport(api.ExportCompleted,
			testserver.ExportDomain{Domain: "a.com", Archives: [][]byte{zipOf(t, "a")}},
			testserver.ExportDomain{Domain: "b.com", Archives: [][]byte{zipOf(t, "b")}},
		)
		if out := f.run(context.Background(), id); out.Downloaded != 2 {
			t.Fatalf("sizeless=%v: first run %+v", sizeless, out)
		}
		if out := f.run(context.Background(), id, "a.com"); out.Present != 1 {
			t.Errorf("sizeless=%v: filtered rerun %+v", sizeless, out)
		}
		var m manifest
		if err := json.Unmarshal(f.file(ManifestName), &m); err != nil {
			t.Fatal(err)
		}
		for _, d := range m.Domains {
			if !d.Archives[0].Downloaded {
				t.Errorf("sizeless=%v: %s marked not downloaded after a filtered rerun", sizeless, d.Domain)
			}
		}
	}
}

func TestRunRefusesLinksInTheExportFolder(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	link := func(t *testing.T, target, name string) {
		t.Helper()
		if err := os.Symlink(target, name); err != nil {
			t.Skipf("cannot create symlinks here: %v", err)
		}
	}
	data := zipOf(t, "a.jpg")
	t.Run("domain folder", func(t *testing.T) {
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
		os.MkdirAll(f.dir, 0o755)
		link(t, outside, filepath.Join(f.dir, "example.com"))
		out := f.run(context.Background(), id)
		if out.Failed != 1 {
			t.Fatalf("outcome: %+v", out)
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("wrote through the link: %v", entries)
		}
	})
	t.Run("domain folder holding a matching archive", func(t *testing.T) {
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted,
			testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}},
			testserver.ExportDomain{Domain: "other.com", Archives: [][]byte{data}},
		)
		elsewhere := t.TempDir()
		os.WriteFile(filepath.Join(elsewhere, "images-1.zip"), data, 0o644)
		os.MkdirAll(f.dir, 0o755)
		link(t, elsewhere, filepath.Join(f.dir, "example.com"))
		link(t, elsewhere, filepath.Join(f.dir, "other.com"))
		out := f.run(context.Background(), id, "example.com")
		if out.Present != 0 || out.Failed != 1 {
			t.Fatalf("outcome: %+v", out)
		}
		var m manifest
		json.Unmarshal(f.file(ManifestName), &m)
		for _, d := range m.Domains {
			if d.Archives[0].Downloaded {
				t.Errorf("%s: an archive behind a link counted as downloaded", d.Domain)
			}
		}
	})
	t.Run("partial file and manifest", func(t *testing.T) {
		os.WriteFile(victim, []byte("keep me"), 0o644)
		f := newFixture(t)
		id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
		final := filepath.Join(f.dir, "example.com", "images-1.zip")
		os.MkdirAll(filepath.Dir(final), 0o755)
		link(t, victim, tempPath(job{path: final, size: int64(len(data))}))
		link(t, victim, filepath.Join(f.dir, ManifestName+partSuffix))
		out := f.run(context.Background(), id)
		if out.Err != nil || out.Downloaded != 1 {
			t.Fatalf("outcome: %+v", out)
		}
		if got, _ := os.ReadFile(victim); string(got) != "keep me" {
			t.Errorf("the link's target was written: %q", got)
		}
		if !bytes.Equal(f.file("example.com/images-1.zip"), data) {
			t.Error("archive differs")
		}
	})
}

// endsOnceComplete is a context that reports Canceled once path holds size
// bytes: the download completes, and the check that follows sees the end.
type endsOnceComplete struct {
	context.Context
	path string
	size int64
}

func (c endsOnceComplete) Err() error {
	if st, err := os.Stat(c.path); err == nil && st.Size() == c.size {
		return context.Canceled
	}
	return nil
}

func TestDownloadKeepsThePartialFileWhenTheCheckIsInterrupted(t *testing.T) {
	f := newFixture(t)
	data := zipOf(t, "a.jpg", "b.jpg")
	id := f.srv.AddExport(api.ExportCompleted, testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{data}})
	exp, err := f.client.GetExport(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	j := job{path: filepath.Join(f.dir, "example.com", "images-1.zip"), url: exp.Domains[0].Parts[0].URL, size: int64(len(data))}
	res := f.newRunner().download(endsOnceComplete{context.Background(), tempPath(j), j.size}, j)
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("got %+v", res)
	}
	if st, err := os.Stat(tempPath(j)); err != nil || st.Size() != j.size {
		t.Errorf("partial file: %v", err)
	}
	if _, err := os.Stat(j.path); !os.IsNotExist(err) {
		t.Errorf("archive put in place unchecked: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checkArchive(ctx, tempPath(j), j.size); !errors.Is(err, context.Canceled) {
		t.Errorf("check on an ended context: %v", err)
	}
}
