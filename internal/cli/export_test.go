package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

func archive(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, _ := zw.Create(n)
		w.Write([]byte("data of " + n))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (h *harness) twoDomains() {
	h.srv.SetExportDomains(
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{archive(h.t, "a.jpg"), archive(h.t, "b.jpg")}},
		testserver.ExportDomain{Domain: "directUpload", Archives: [][]byte{archive(h.t, "c.png")}},
	)
}

func TestExportStartDownloadsEverything(t *testing.T) {
	h := newHarness(t).withKey()
	h.twoDomains()
	out := t.TempDir()
	if code := h.run("export", "start", "-o", out); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	got := h.stdout.String()
	for _, want := range []string{
		"Started export 1.\n",
		"Export 1: running\n",
		"  example.com: completed, 2 archives\n",
		"downloaded  " + filepath.Join(out, "optimole-export-1", "example.com", "images-2.zip"),
		"Export 1: completed\n",
		"Export 1 completed: 3 downloaded, 0 already there, 0 failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout lacks %q:\n%s", want, got)
		}
	}
	for _, rel := range []string{"example.com/images-1.zip", "example.com/images-2.zip", "directUpload/images-1.zip", "export.json"} {
		if _, err := os.Stat(filepath.Join(out, "optimole-export-1", filepath.FromSlash(rel))); err != nil {
			t.Error(err)
		}
	}

	// A second run finds everything in place.
	if code := h.run("export", "download", "1", "-o", out); code != ExitOK {
		t.Fatalf("download exit %d", code)
	}
	if !strings.Contains(h.stdout.String(), "0 downloaded, 3 already there") {
		t.Errorf("second run: %s", h.stdout.String())
	}
}

func TestExportStartJSON(t *testing.T) {
	h := newHarness(t).withKey()
	h.twoDomains()
	out := t.TempDir()
	if code := h.run("export", "start", "--json", "--output", out, "--domain", "example.com"); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	if strings.Contains(h.stdout.String(), "X-Amz-Signature") || strings.Contains(h.stdout.String(), "http") {
		t.Errorf("output holds a URL: %s", h.stdout.String())
	}
	lines := h.jsonLines()
	if lines[0]["event"] != "started" || lines[0]["created"] != true || lines[0]["id"] != float64(1) {
		t.Errorf("first line: %v", lines[0])
	}
	archives := 0
	for _, l := range lines {
		if l["event"] == "archive" {
			archives++
			if l["status"] != "downloaded" || l["domain"] != "example.com" || l["size"] == nil || !strings.HasPrefix(l["path"].(string), out) {
				t.Errorf("archive line: %v", l)
			}
		}
		if l["domain"] == "directUpload" {
			t.Errorf("unselected domain reported: %v", l)
		}
	}
	sum, _ := lines[len(lines)-1]["summary"].(map[string]any)
	if archives != 2 || sum["status"] != "completed" || sum["downloaded"] != float64(2) || sum["dir"] != filepath.Join(out, "optimole-export-1") {
		t.Errorf("archives %d, summary %v", archives, sum)
	}
}

func TestExportStartFollowsTheRunningExport(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		h := newHarness(t).withKey()
		h.srv.LegacyExportConflicts = legacy
		h.srv.AddExport("completed")
		running := h.srv.AddExport("running", testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{archive(t, "a")}})
		out := t.TempDir()
		if code := h.run("export", "start", "-o", out); code != ExitOK {
			t.Fatalf("legacy=%v: exit %d, stderr %s", legacy, code, h.stderr.String())
		}
		if !strings.Contains(h.stdout.String(), "Export 2 is already running; following it.") {
			t.Errorf("legacy=%v: %s", legacy, h.stdout.String())
		}
		if _, err := os.Stat(filepath.Join(out, "optimole-export-2", "example.com", "images-1.zip")); err != nil {
			t.Errorf("legacy=%v: %v", legacy, err)
		}
		if ids := h.srv.ExportIDs(); len(ids) != 2 || ids[1] != running {
			t.Errorf("legacy=%v: exports %v", legacy, ids)
		}
		// The coded answer names the export; only older servers need the list.
		if lists := h.srv.Count(testserver.RouteExportList); (lists == 0) == legacy {
			t.Errorf("legacy=%v: %d list requests", legacy, lists)
		}
	}
}

func TestExportStartFollowsAnExportThatFinishedAfterTheConflict(t *testing.T) {
	h := newHarness(t).withKey()
	// An older server's bare 409 for a running export, which finished
	// before the CLI looked at the list.
	h.srv.AddExport("completed", testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{archive(t, "a")}})
	h.srv.Inject(testserver.RouteExportCreate, testserver.Response{Status: 409, Body: `{"message":"Asset export is already in progress"}`})
	out := t.TempDir()
	if code := h.run("export", "start", "-o", out); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Export 1 is already running; following it.") || !strings.Contains(h.stdout.String(), "Export 1 completed: 1 downloaded") {
		t.Errorf("stdout: %s", h.stdout.String())
	}
}

func TestExportStartTrustsTheLimitCode(t *testing.T) {
	h := newHarness(t).withKey()
	// The limit was reached, then an export was deleted before the CLI
	// looked at the list: still not an export to follow.
	h.srv.AddExport("completed")
	h.srv.Inject(testserver.RouteExportCreate, testserver.Response{Status: 409, Body: `{"code":"export_limit_reached","message":"Maximum asset export count reached (Limit: 5)","limit":5,"count":5}`})
	if code := h.run("export", "start"); code != ExitFailure || !strings.Contains(h.stderr.String(), `"optimole export delete ID"`) {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
}

func TestExportStartAtTheLimit(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		h := newHarness(t).withKey()
		h.srv.LegacyExportConflicts = legacy
		for i := 0; i < testserver.ExportLimit; i++ {
			h.srv.AddExport("completed")
		}
		if code := h.run("export", "start"); code != ExitFailure {
			t.Fatalf("legacy=%v: exit %d", legacy, code)
		}
		stderr := h.stderr.String()
		for _, want := range []string{"Maximum asset export count reached", `"optimole export delete ID"`, "Oldest: export 1 (completed, 2026-11-04 10:01 UTC)."} {
			if !strings.Contains(stderr, want) {
				t.Errorf("legacy=%v: stderr lacks %q:\n%s", legacy, want, stderr)
			}
		}
		if !legacy && !strings.Contains(stderr, "export_limit_reached; limit 5, used 5") {
			t.Errorf("no code: %s", stderr)
		}
	}
}

func TestExportStartNoWait(t *testing.T) {
	h := newHarness(t).withKey()
	h.twoDomains()
	if code := h.run("export", "start", "--no-wait"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if h.stdout.String() != "Started export 1.\n" || h.srv.Count(testserver.RouteExportGet) != 0 {
		t.Errorf("stdout %q, %d looks at the export", h.stdout.String(), h.srv.Count(testserver.RouteExportGet))
	}
}

func TestExportStartAfterALostAnswer(t *testing.T) {
	h := newHarness(t).withKey()
	h.twoDomains()
	h.srv.Inject(testserver.RouteExportCreate, testserver.Response{CommitThenClose: true})
	if code := h.run("export", "start", "-o", t.TempDir()); code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, h.stderr.String())
	}
	if n := len(h.srv.ExportIDs()); n != 1 || h.srv.Count(testserver.RouteExportCreate) != 1 {
		t.Errorf("exports %d, create requests %d", n, h.srv.Count(testserver.RouteExportCreate))
	}
}

func TestExportDownloadFailedExport(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddExport("failed",
		testserver.ExportDomain{Domain: "broken.com", Fail: true},
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{archive(t, "a")}},
	)
	if code := h.run("export", "download", "-o", t.TempDir()); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stdout.String(), "Export 1 failed (failed: broken.com): 1 downloaded") {
		t.Errorf("stdout: %s", h.stdout.String())
	}
}

func TestExportDownloadStopsAndTellsHowToResume(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.PauseExports(true)
	h.srv.AddExport("running", testserver.ExportDomain{Domain: "example.com"})
	out := filepath.Join(t.TempDir(), "my exports")

	if code := h.run("export", "download", "latest", "-o", out, "--timeout", "150ms"); code != ExitFailure {
		t.Fatalf("timeout: exit %d", code)
	}
	resume := "Resume with: optimole export download 1 -o " + shellQuote(out)
	if !strings.Contains(h.stderr.String(), "timed out after 150ms") || !strings.Contains(h.stderr.String(), resume) {
		t.Errorf("timeout stderr: %s", h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "Export 1 running: 0 downloaded") || !strings.Contains(h.stdout.String(), "Timed out.") {
		t.Errorf("timeout stdout: %s", h.stdout.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if code := h.runCtx(ctx, "export", "download", "1", "-o", out); code != ExitInterrupted {
		t.Fatalf("interrupt: exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), resume) || !strings.Contains(h.stdout.String(), "Interrupted.") {
		t.Errorf("interrupt: stdout %s stderr %s", h.stdout.String(), h.stderr.String())
	}
}

func TestExportUsageErrors(t *testing.T) {
	h := newHarness(t).withKey()
	for _, args := range [][]string{
		{"export"},
		{"export", "nope"},
		{"export", "start", "extra"},
		{"export", "start", "-c", "9"},
		{"export", "start", "--timeout", "-1s"},
		{"export", "start", "--domain", " "},
		{"export", "download", "abc"},
		{"export", "download", "1", "2"},
		{"export", "status", "0"},
		{"export", "list", "x"},
		{"export", "delete"},
		{"export", "delete", "latest"},
	} {
		if code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if h.srv.Count(testserver.RouteExportCreate)+h.srv.Count(testserver.RouteExportList)+h.srv.Count(testserver.RouteExportDelete) != 0 {
		t.Error("a usage error reached the server")
	}

	h.srv.AddExport("completed", testserver.ExportDomain{Domain: "example.com"})
	if code := h.run("export", "download", "1", "--domain", "other.com", "-o", t.TempDir()); code != ExitUsage {
		t.Errorf("unknown domain: exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), `export has no domain "other.com" (it has: example.com)`) {
		t.Errorf("unknown domain: %s", h.stderr.String())
	}
}

func TestExportHelp(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"help", "export"}, {"export", "help"}} {
		if code := h.run(args...); code != ExitOK {
			t.Errorf("%v: exit %d", args, code)
		}
		if !strings.Contains(h.stdout.String()+h.stderr.String(), "optimole export <command>") {
			t.Errorf("%v: no usage", args)
		}
	}
	if code := h.run("export", "start", "-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "--no-wait") {
		t.Errorf("start -h: exit %d", code)
	}
}

func TestExportStatus(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddExport("completed",
		testserver.ExportDomain{Domain: "example.com", Archives: [][]byte{make([]byte, 2048), make([]byte, 1024)}},
		testserver.ExportDomain{Domain: "directUpload"},
	)
	if code := h.run("export", "status"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	got := h.stdout.String()
	for _, want := range []string{"Export 1: completed, started 2026-11-04 10:01 UTC", "example.com   completed  2         3.0 KB", "directUpload  completed  0         -"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout lacks %q:\n%s", want, got)
		}
	}
	if code := h.run("export", "status", "1", "--json"); code != ExitOK {
		t.Fatalf("json exit %d", code)
	}
	if strings.Contains(h.stdout.String(), "http") {
		t.Errorf("status JSON holds a URL: %s", h.stdout.String())
	}
	l := h.jsonLines()
	domains := l[0]["domains"].([]any)
	first := domains[0].(map[string]any)["archives"].([]any)[0].(map[string]any)
	if l[0]["id"] != float64(1) || first["part_number"] != float64(1) || first["size"] != float64(2048) {
		t.Errorf("status JSON: %v", l[0])
	}
}

func TestExportList(t *testing.T) {
	h := newHarness(t).withKey()
	if code := h.run("export", "list"); code != ExitOK || !strings.Contains(h.stdout.String(), "No exports.") {
		t.Fatalf("empty list: exit %d %q", code, h.stdout.String())
	}
	if code := h.run("export", "download"); code != ExitFailure || !strings.Contains(h.stderr.String(), "there are no exports yet") {
		t.Errorf("latest of none: exit %d %s", code, h.stderr.String())
	}
	h.srv.AddExport("completed", testserver.ExportDomain{Domain: "a.com"}, testserver.ExportDomain{Domain: "b.com"})
	h.srv.AddExport("running")
	if code := h.run("export", "list"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "2   running") || !strings.HasPrefix(lines[2], "1   completed  2026-11-04 10:01 UTC  2") {
		t.Errorf("list:\n%s", h.stdout.String())
	}
	if code := h.run("export", "list", "--json"); code != ExitOK {
		t.Fatalf("json exit %d", code)
	}
	if l := h.jsonLines(); len(l) != 2 || l[1]["id"] != float64(1) || len(l[1]["domains"].([]any)) != 2 {
		t.Errorf("list JSON: %v", l)
	}
}

func TestExportDelete(t *testing.T) {
	h := newHarness(t).withKey()
	done := h.srv.AddExport("completed")
	h.srv.AddExport("running")
	if code := h.run("export", "delete", "1"); code != ExitOK || h.stdout.String() != "Deleted export 1.\n" {
		t.Fatalf("delete: exit %d %q", code, h.stdout.String())
	}
	if code := h.run("export", "delete", "2"); code != ExitFailure || !strings.Contains(h.stderr.String(), "Only finished exports can be deleted") {
		t.Errorf("running: exit %d %s", code, h.stderr.String())
	}
	if code := h.run("export", "delete", "1"); code != ExitFailure || !strings.Contains(h.stderr.String(), "HTTP 404") {
		t.Errorf("deleted twice: exit %d %s", code, h.stderr.String())
	}
	if ids := h.srv.ExportIDs(); len(ids) != 1 || ids[0] == done {
		t.Errorf("exports left: %v", ids)
	}
}

func TestExportNeedsAFullAccessKey(t *testing.T) {
	h := newHarness(t)
	const scoped = "optml-s-scoped-key-0123456789"
	h.srv.AddKey(scoped, testserver.Key{Scoped: true})
	h.vars[config.EnvAPIKey] = scoped
	if code := h.run("export", "start"); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), "Exports need a full-access API key") || strings.Contains(h.stderr.String(), scoped) {
		t.Errorf("stderr: %s", h.stderr.String())
	}
}

func TestExportMissingExport(t *testing.T) {
	h := newHarness(t).withKey()
	if code := h.run("export", "download", "99", "-o", t.TempDir()); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stderr.String(), "No such export: finished exports are deleted 10 days after they finish") {
		t.Errorf("stderr: %s", h.stderr.String())
	}
}

func TestExportStartInterruptedDuringTheConflictLookup(t *testing.T) {
	for _, lostAnswer := range []bool{false, true} {
		h := newHarness(t).withKey()
		h.srv.LegacyExportConflicts = true
		h.srv.AddExport("running")
		if lostAnswer {
			h.srv.Inject(testserver.RouteExportCreate, testserver.Response{CommitThenClose: true})
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.srv.Hook = func(route testserver.Route, _ *http.Request) {
			if route == testserver.RouteExportList {
				cancel()
			}
		}
		if code := h.runCtx(ctx, "export", "start"); code != ExitInterrupted {
			t.Errorf("lostAnswer=%v: exit %d, stderr %s", lostAnswer, code, h.stderr.String())
		}
	}
}

func TestExportListFollowsCursors(t *testing.T) {
	h := newHarness(t).withKey()
	for i := 0; i < exportsPerPage+3; i++ {
		h.srv.AddExport("completed")
	}
	if code := h.run("export", "list", "--json"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if n := len(h.jsonLines()); n != exportsPerPage+3 || h.srv.Count(testserver.RouteExportList) != 2 {
		t.Errorf("%d exports in %d pages", n, h.srv.Count(testserver.RouteExportList))
	}

	// A server that hands out the same cursor again is not followed forever.
	g := newHarness(t).withKey()
	page := `{"data":[{"id":%d,"status":"completed","created_at":"2026-11-04T10:00:00.000000Z","domains":[]}],"meta":{"next_cursor":"same","has_more":true}}`
	g.srv.Inject(testserver.RouteExportList, testserver.Response{Status: 200, Body: fmt.Sprintf(page, 2)}, testserver.Response{Status: 200, Body: fmt.Sprintf(page, 1)})
	if code := g.run("export", "list", "--json"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if n := len(g.jsonLines()); n != 2 || g.srv.Count(testserver.RouteExportList) != 2 {
		t.Errorf("%d exports in %d pages", n, g.srv.Count(testserver.RouteExportList))
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"out":              "out",
		"/backups/2026-11": "/backups/2026-11",
	}
	if runtime.GOOS == "windows" {
		cases[`C:\Backups\Optimole`] = `C:\Backups\Optimole`
		cases["my exports"] = `"my exports"`
		cases[`C:\My Exports\`] = `"C:\My Exports\\"`
	} else {
		cases["my exports"] = "'my exports'"
		cases["it's"] = `'it'\''s'`
		cases["$HOME"] = "'$HOME'"
		cases[`exports\archive`] = `'exports\archive'`
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
