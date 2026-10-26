package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

// localOnly fails the test for any request that would leave the machine.
type localOnly struct {
	t *testing.T
}

func (l localOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if h := r.URL.Hostname(); h != "127.0.0.1" && h != "localhost" {
		l.t.Errorf("request to a non-local host: %s", r.URL.Host)
		return nil, errors.New("blocked non-local request")
	}
	return http.DefaultTransport.RoundTrip(r)
}

type harness struct {
	t      *testing.T
	srv    *testserver.Server
	cfgDir string
	vars   map[string]string
	stdin  io.Reader
	stdout bytes.Buffer
	stderr bytes.Buffer
}

// newHarness points the CLI at a fake server through OPTIMOLE_API_URL and
// gives it an empty config directory.
func newHarness(t *testing.T) *harness {
	srv := testserver.New(t)
	return &harness{
		t:      t,
		srv:    srv,
		cfgDir: t.TempDir(),
		vars:   map[string]string{config.EnvAPIURL: srv.URL},
		stdin:  strings.NewReader(""),
	}
}

func (h *harness) env() Env {
	var mu sync.Mutex
	return Env{
		Stdin:  h.stdin,
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		Getenv: func(k string) string {
			mu.Lock()
			defer mu.Unlock()
			return h.vars[k]
		},
		UserConfigDir: func() (string, error) { return h.cfgDir, nil },
		Build:         BuildInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-10-26"},
		Transport:     localOnly{h.t},
		Sleep:         func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
}

func (h *harness) run(args ...string) int {
	return h.runCtx(context.Background(), args...)
}

func (h *harness) runCtx(ctx context.Context, args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return Run(ctx, args, h.env())
}

func (h *harness) withKey() *harness {
	h.vars[config.EnvAPIKey] = testserver.DefaultKey
	return h
}

func (h *harness) jsonLines() []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(h.stdout.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			h.t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestUploadHumanOutput(t *testing.T) {
	h := newHarness(t).withKey()
	root := tree(t, map[string]string{"a.jpg": "aaa", "2024/b.png": "bbb", "notes.bmp": "x", ".DS_Store": "x"})
	// Flags after the path work too.
	code := h.run("upload", root, "-r", "--folder", "Campaigns", "--create-folders")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, h.stdout.String(), h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"skipped   " + filepath.Join(root, "notes.bmp") + ": unsupported file type .bmp",
		"uploaded  " + filepath.Join(root, "a.jpg") + " -> Campaigns  https://app.i.optimole.com/w:auto/h:auto/q:auto/id:",
		"uploaded  " + filepath.Join(root, "2024", "b.png") + " -> Campaigns/2024  https://",
		"2 uploaded, 1 skipped, 0 failed (3 files; 1 hidden or excluded entry ignored).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if got := strings.Join(h.srv.FolderPaths(), ","); got != "Campaigns,Campaigns/2024" {
		t.Errorf("folders = %s", got)
	}
}

func TestUploadJSONOutput(t *testing.T) {
	h := newHarness(t).withKey()
	root := tree(t, map[string]string{"a.jpg": "aaa", "b.exe": "x"})
	folder := h.srv.AddFolder("Docs")
	code := h.run("upload", "--json", "--folder-id", fmt.Sprint(folder), filepath.Join(root, "a.jpg"), filepath.Join(root, "b.exe"))
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	lines := h.jsonLines()
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	if l := lines[0]; l["status"] != "skipped" || l["error"] != "blocked file type .exe" || l["path"] != filepath.Join(root, "b.exe") {
		t.Errorf("skipped line = %v", l)
	}
	l := lines[1]
	if l["status"] != "uploaded" || l["folder_id"] != float64(folder) || l["folder"] != "" || len(l["key"].(string)) != 32 || !strings.HasPrefix(l["url"].(string), "https://") {
		t.Errorf("uploaded line = %v", l)
	}
	sum := lines[2]["summary"].(map[string]any)
	if sum["total"] != float64(2) || sum["uploaded"] != float64(1) || sum["skipped"] != float64(1) || sum["failed"] != float64(0) {
		t.Errorf("summary = %v", sum)
	}
	if h.stderr.Len() != 0 {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestUploadExitCodes(t *testing.T) {
	t.Run("a failed file exits 1", func(t *testing.T) {
		h := newHarness(t).withKey()
		h.srv.Inject(testserver.RouteCreate, testserver.Response{Status: 422, Body: `{"message":"The file type is not allowed.","errors":{"filename":["The file type is not allowed."]}}`})
		root := tree(t, map[string]string{"a.jpg": "a", "b.jpg": "b"})
		if code := h.run("upload", "-r", "-c", "1", root); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(h.stdout.String(), "1 uploaded, 0 skipped, 1 failed") {
			t.Fatalf("stdout:\n%s", h.stdout.String())
		}
	})
	t.Run("nothing to upload exits 1", func(t *testing.T) {
		h := newHarness(t)
		root := tree(t, map[string]string{"a.bmp": "a"})
		if code := h.run("upload", filepath.Join(root, "a.bmp")); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(h.stderr.String(), "nothing to upload") || h.srv.Count(testserver.RouteCreate) != 0 {
			t.Fatalf("stderr = %q", h.stderr.String())
		}
	})
	root := tree(t, map[string]string{"a.jpg": "a", "sub/b.jpg": "b"})
	usage := map[string][]string{
		"no paths":                   {"upload"},
		"directory without -r":       {"upload", root},
		"missing file":               {"upload", filepath.Join(root, "missing.jpg")},
		"folder and folder-id":       {"upload", "--folder", "A", "--folder-id", "3", root},
		"concurrency 0":              {"upload", "-c", "0", root},
		"concurrency 17":             {"upload", "--concurrency", "17", root},
		"bad folder path":            {"upload", "--folder", "a//b", root},
		"bad folder id":              {"upload", "--folder-id", "abc", root},
		"create-folders + folder-id": {"upload", "--folder-id", "3", "--create-folders", root},
		"folder-id with subdirs":     {"upload", "-r", "--folder-id", "3", root},
		"bad exclude":                {"upload", "-r", "--exclude", "[", root},
		"unknown flag":               {"upload", "--nope", root},
		"unknown command":            {"uplaod", root},
		"no command":                 {},
	}
	for name, args := range usage {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t).withKey()
			if code := h.run(args...); code != ExitUsage {
				t.Fatalf("exit %d, want %d; stderr: %s", code, ExitUsage, h.stderr.String())
			}
			if n := len(h.srv.Events()); n != 0 {
				t.Fatalf("%d requests on a usage error", n)
			}
		})
	}
	t.Run("help exits 0", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("upload", "-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "--create-folders") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if code := h.run("help"); code != ExitOK || !strings.Contains(h.stdout.String(), "Commands:") {
			t.Fatalf("exit %d", code)
		}
	})
}

func TestUploadConfigErrorsExit2(t *testing.T) {
	root := tree(t, map[string]string{"a.jpg": "a"})
	file := filepath.Join(root, "a.jpg")
	t.Run("no key", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("upload", file); code != ExitUsage || !strings.Contains(h.stderr.String(), "optimole login") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("plain http to a remote host", func(t *testing.T) {
		h := newHarness(t).withKey()
		if code := h.run("upload", "--api-url", "http://dashboard.optimole.com/api", file); code != ExitUsage || !strings.Contains(h.stderr.String(), "https") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("plain http in the environment", func(t *testing.T) {
		h := newHarness(t).withKey()
		h.vars[config.EnvAPIURL] = "http://example.com/api"
		if code := h.run("upload", file); code != ExitUsage {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
}

func TestDryRunMakesNoRequests(t *testing.T) {
	h := newHarness(t) // no key needed
	root := tree(t, map[string]string{"a.jpg": "a", "2024/b.jpg": "b", "c.html": "c"})
	if code := h.run("upload", "--dry-run", "-r", "--folder", "X", root); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"planned   " + filepath.Join(root, "a.jpg") + " -> X (1 B)",
		"planned   " + filepath.Join(root, "2024", "b.jpg") + " -> X/2024 (1 B)",
		"skipped   " + filepath.Join(root, "c.html") + ": blocked file type .html",
		"Dry run: 2 to upload, 1 skipped (3 files).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("dry run made %d requests", n)
	}
}

func TestKeyPrecedence(t *testing.T) {
	const fileKey, envKey, flagKey = "optml-file-key-111111", "optml-env-key-222222", "optml-flag-key-333333"
	root := tree(t, map[string]string{"a.jpg": "a"})
	file := filepath.Join(root, "a.jpg")
	cases := []struct {
		name  string
		valid string // the only key the server accepts
		env   bool
		flag  bool
		want  int
	}{
		{"config file is used", fileKey, false, false, ExitOK},
		{"env beats config file", envKey, true, false, ExitOK},
		{"env really beats config file", fileKey, true, false, ExitFailure},
		{"flag beats env", flagKey, true, true, ExitOK},
		{"flag really beats env", envKey, true, true, ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.AddKey(tc.valid, testserver.Key{})
			if err := config.Save(config.Path(h.cfgDir), config.File{APIKey: fileKey}); err != nil {
				t.Fatal(err)
			}
			args := []string{"upload", file}
			if tc.env {
				h.vars[config.EnvAPIKey] = envKey
			}
			if tc.flag {
				args = append(args, "--api-key", flagKey)
			}
			if code := h.run(args...); code != tc.want {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.want, h.stderr.String())
			}
		})
	}
}

func TestURLPrecedence(t *testing.T) {
	root := tree(t, map[string]string{"a.jpg": "a"})
	file := filepath.Join(root, "a.jpg")
	h := newHarness(t).withKey()
	// The flag wins over the (valid) environment value.
	if code := h.run("upload", "--api-url", "http://203.0.113.1/api", file); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	// The environment wins over the default.
	if code := h.run("upload", file); code != ExitOK || h.srv.Count(testserver.RouteCreate) != 1 {
		t.Fatalf("exit %d", code)
	}
}

func TestKeyIsRedacted(t *testing.T) {
	const key = "optml-very-secret-key-987654321"
	echo := fmt.Sprintf(`{"message":"Key %s cannot do that.","errors":{"filename":["bad %s"]}}`, key, key)
	for _, jsonMode := range []bool{false, true} {
		h := newHarness(t)
		h.srv.AddKey(key, testserver.Key{})
		h.srv.Inject(testserver.RouteCreate, testserver.Response{Status: 422, Body: echo})
		root := tree(t, map[string]string{"a.jpg": "a"})
		args := []string{"upload", "--api-key", key, filepath.Join(root, "a.jpg")}
		if jsonMode {
			args = append(args, "--json")
		}
		if code := h.run(args...); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		all := h.stdout.String() + h.stderr.String()
		if strings.Contains(all, key) || !strings.Contains(all, "[REDACTED]") {
			t.Fatalf("json=%v output:\n%s", jsonMode, all)
		}
	}
	// A key in a bad --api-url is redacted too.
	h := newHarness(t)
	h.vars[config.EnvAPIKey] = key
	if code := h.run("folders", "--api-url", "http://x/"+key); code != ExitUsage || strings.Contains(h.stderr.String(), key) {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestUploadLimitInJSON(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.SetUploadLimit(5, 5)
	root := tree(t, map[string]string{"a.jpg": "a", "b.jpg": "b", "c.jpg": "c"})
	if code := h.run("upload", "-r", "--json", root); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	lines := h.jsonLines()
	if len(lines) != 4 {
		t.Fatalf("lines = %v", lines)
	}
	for _, l := range lines[:3] {
		if l["status"] != "failed" || l["code"] != "upload_limit_reached" || !strings.Contains(l["error"].(string), "limit 5, used 5") {
			t.Errorf("line = %v", l)
		}
	}
	if s := lines[3]["summary"].(map[string]any); s["not_attempted"] != float64(2) || s["failed"] != float64(3) {
		t.Errorf("summary = %v", s)
	}
	if h.srv.Count(testserver.RouteCreate) != 1 {
		t.Errorf("creates = %d", h.srv.Count(testserver.RouteCreate))
	}
}

func TestInvalidFolderHint(t *testing.T) {
	h := newHarness(t).withKey()
	root := tree(t, map[string]string{"a.jpg": "a"})
	if code := h.run("upload", "--folder", "Nope", filepath.Join(root, "a.jpg")); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.stdout.String(), "add --create-folders") {
		t.Fatalf("stdout:\n%s", h.stdout.String())
	}
}

func TestInterruptPrintsSummary(t *testing.T) {
	h := newHarness(t).withKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.srv.Hook = func(route testserver.Route, _ *http.Request) {
		if route == testserver.RoutePut {
			cancel()
		}
	}
	root := tree(t, map[string]string{"a.jpg": "a", "b.jpg": "b", "c.jpg": "c"})
	if code := h.runCtx(ctx, "upload", "-r", "-c", "1", root); code != ExitInterrupted {
		t.Fatalf("exit %d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "failed    "+filepath.Join(root, "a.jpg")+" -> (library root): interrupted") ||
		!strings.Contains(out, "0 uploaded, 0 skipped, 3 failed, 2 of them not attempted (3 files). Interrupted.") {
		t.Fatalf("stdout:\n%s", out)
	}
}

func TestFolders(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddFolder("Campaigns/2026/Spring")
	h.srv.AddFolder("Brand")
	for i := range 120 {
		h.srv.AddFolder(fmt.Sprintf("Bulk/f%03d", i))
	}
	if code := h.run("folders"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{"ID   PATH\n", "Campaigns/2026/Spring\n", "Bulk/f119\n", "Brand\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q", want)
		}
	}
	if n := strings.Count(out, "\n"); n != 1+4+1+120 {
		t.Errorf("%d lines, want 126", n)
	}
	if n := h.srv.Count(testserver.RouteFolders); n != 2 {
		t.Errorf("folder pages fetched = %d, want 2", n)
	}

	if code := h.run("folders", "--parent", "root", "--json"); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := h.jsonLines()
	if len(lines) != 3 || lines[0]["name"] != "Brand" || lines[0]["parent_id"] != nil {
		t.Fatalf("lines = %v", lines)
	}
	if ev := h.srv.Events(); !strings.Contains(ev[len(ev)-1].Query, "parent_id=root") {
		t.Fatalf("query = %q", ev[len(ev)-1].Query)
	}

	if code := h.run("folders", "--parent", "x1"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestFoldersScopedKey(t *testing.T) {
	h := newHarness(t)
	const scoped = "optml-sScopedKeyForTests0123"
	h.srv.AddKey(scoped, testserver.Key{Scoped: true})
	h.vars[config.EnvAPIKey] = scoped
	if code := h.run("folders"); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	if msg := h.stderr.String(); !strings.Contains(msg, "folder-scoped API key") || !strings.Contains(msg, "optimole upload --folder PATH") {
		t.Fatalf("stderr = %q", msg)
	}
}

func TestLoginLogout(t *testing.T) {
	h := newHarness(t)
	path := config.Path(h.cfgDir)

	h.stdin = strings.NewReader("\n  " + testserver.DefaultKey + "  \n")
	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	f, found, err := config.Load(path)
	if err != nil || !found || f.APIKey != testserver.DefaultKey {
		t.Fatalf("saved = %+v, %v, %v", f, found, err)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s mode = %v, want %o", p, info.Mode().Perm(), want)
			}
		}
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), testserver.DefaultKey) {
		t.Fatal("login printed the key")
	}
	ev := h.srv.Events()
	if len(ev) != 1 || ev[0].Route != testserver.RouteFolders || ev[0].Query != "per_page=1" {
		t.Fatalf("verification requests = %+v", ev)
	}

	// The saved key is used by later commands.
	if code := h.run("folders"); code != ExitOK {
		t.Fatalf("folders after login: exit %d: %s", code, h.stderr.String())
	}

	if code := h.run("logout"); code != ExitOK || !strings.Contains(h.stdout.String(), "Logged out") {
		t.Fatalf("logout: exit %d", code)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config still exists: %v", err)
	}
	if code := h.run("logout"); code != ExitOK || !strings.Contains(h.stdout.String(), "No saved API key") {
		t.Fatalf("second logout: exit %d", code)
	}
}

func TestLoginScopedKeyIsValid(t *testing.T) {
	h := newHarness(t)
	const scoped = "optml-sScopedKeyForLogin0123"
	h.srv.AddKey(scoped, testserver.Key{Scoped: true})
	h.stdin = strings.NewReader(scoped)
	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "folder-scoped key") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
	if f, _, _ := config.Load(config.Path(h.cfgDir)); f.APIKey != scoped {
		t.Fatal("scoped key not saved")
	}
}

func TestLoginFailures(t *testing.T) {
	cases := []struct {
		name  string
		stdin string
		setup func(*harness)
		want  int
	}{
		{"rejected key", "optml-unknown-key-000000", nil, ExitFailure},
		{"empty stdin", "  \n\n", nil, ExitUsage},
		{"key with spaces", "two words", nil, ExitUsage},
		{"server down", testserver.DefaultKey, func(h *harness) {
			for range 4 {
				h.srv.Inject(testserver.RouteFolders, testserver.Response{Status: 503, Body: `{}`})
			}
		}, ExitFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			h.stdin = strings.NewReader(tc.stdin)
			if code := h.run("login"); code != tc.want {
				t.Fatalf("exit %d, want %d: %s", code, tc.want, h.stderr.String())
			}
			if _, err := os.Stat(config.Path(h.cfgDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a key was saved")
			}
			if strings.Contains(h.stderr.String(), tc.stdin) && strings.TrimSpace(tc.stdin) != "" && tc.name != "key with spaces" {
				t.Fatalf("stderr echoes the key: %s", h.stderr.String())
			}
		})
	}
}

// TestLoginNeverEchoesAPositionalKey: "optimole login $KEY" is a mistake the
// error message must not turn into a leak.
func TestLoginNeverEchoesAPositionalKey(t *testing.T) {
	const key = "optml-positional-secret-0123456789"
	for _, args := range [][]string{{"login", key}, {"login", "--api-url", "https://x.example/api", key}, {"logout", key}, {"folders", key}} {
		h := newHarness(t)
		h.vars[config.EnvAPIKey] = testserver.DefaultKey
		if code := h.run(args...); code != ExitUsage {
			t.Fatalf("%v: exit %d", args[0], code)
		}
		if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, key) || strings.Contains(out, "positional-secret") {
			t.Fatalf("%v printed the key:\n%s", args[0], out)
		}
		if _, err := os.Stat(config.Path(h.cfgDir)); err == nil {
			t.Fatal("a key was saved")
		}
	}
}

// TestLoginCancelWhileReadingStdin: a pipe that never delivers a line must
// not keep Ctrl-C from working.
func TestLoginCancelWhileReadingStdin(t *testing.T) {
	h := newHarness(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	h.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan int, 1)
	go func() { done <- h.runCtx(ctx, "login") }()
	select {
	case code := <-done:
		if code != ExitInterrupted {
			t.Fatalf("exit %d, want %d", code, ExitInterrupted)
		}
	case <-time.After(5 * time.Second):
		pw.Close()
		t.Fatal("login ignored the cancellation while reading stdin")
	}
}

func TestLoginReadsOneBoundedLine(t *testing.T) {
	h := newHarness(t)
	h.stdin = strings.NewReader(strings.Repeat("k", 10000))
	if code := h.run("login"); code != ExitUsage || !strings.Contains(h.stderr.String(), "too long") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	h.stdin = strings.NewReader("\n\n" + testserver.DefaultKey + "\nsecond-line-ignored\n")
	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code := h.run("version"); code != ExitOK || h.stdout.String() != "optimole 1.2.3 (commit abc1234, built 2026-10-26)\n" {
		t.Fatalf("exit %d: %q", code, h.stdout.String())
	}
}

func TestUserAgent(t *testing.T) {
	cases := map[string]string{"": "optimole-cli/dev", "v1.4.0": "optimole-cli/1.4.0", "1.4.0": "optimole-cli/1.4.0"}
	for in, want := range cases {
		if got := userAgent(BuildInfo{Version: in}); got != want {
			t.Errorf("userAgent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	fs := newFlagSet("x", "", Env{Stderr: io.Discard})
	r := fs.Bool("r", false, "")
	folder := fs.String("folder", "", "")
	pos, err := parseArgs(fs, []string{"a", "-r", "b", "--folder", "F", "--", "-c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	if !*r || *folder != "F" || strings.Join(pos, ",") != "a,b,-c,d" {
		t.Fatalf("r=%v folder=%q pos=%v", *r, *folder, pos)
	}
}

// countdownCtx reports cancellation after its Err method has been asked n
// times, to cancel in the middle of a walk.
type countdownCtx struct {
	context.Context
	mu   sync.Mutex
	n    int
	done chan struct{}
}

func newCountdownCtx(n int) *countdownCtx {
	return &countdownCtx{Context: context.Background(), n: n, done: make(chan struct{})}
}

func (c *countdownCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	if c.n == 0 {
		close(c.done)
		return context.Canceled
	}
	return nil
}

func (c *countdownCtx) Done() <-chan struct{} { return c.done }

func TestDryRunCancellation(t *testing.T) {
	files := map[string]string{}
	for i := range 50 {
		files[fmt.Sprintf("d%02d/f.jpg", i)] = "x"
	}
	root := tree(t, files)

	t.Run("before planning", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if code := h.runCtx(ctx, "upload", "--dry-run", "--json", "-r", root); code != ExitInterrupted {
			t.Fatalf("exit %d", code)
		}
		lines := h.jsonLines()
		if len(lines) != 1 || lines[0]["summary"].(map[string]any)["interrupted"] != true {
			t.Fatalf("output = %v", lines)
		}
	})
	t.Run("during the walk", func(t *testing.T) {
		h := newHarness(t)
		if code := h.runCtx(newCountdownCtx(20), "upload", "--dry-run", "-r", root); code != ExitInterrupted {
			t.Fatalf("exit %d", code)
		}
		if out := h.stdout.String(); !strings.Contains(out, "Interrupted.") || strings.Count(out, "planned ") == 50 {
			t.Fatalf("stdout:\n%s", out)
		}
	})
	t.Run("while listing", func(t *testing.T) {
		h := newHarness(t)
		// The walk checks once per entry (about 100), the plan loop once per
		// file (50); the rest of the budget runs out while printing.
		if code := h.runCtx(newCountdownCtx(160), "upload", "--dry-run", "-r", root); code != ExitInterrupted {
			t.Fatalf("exit %d", code)
		}
		out := h.stdout.String()
		if n := strings.Count(out, "planned "); n == 0 || n == 50 || !strings.Contains(out, "Interrupted.") {
			t.Fatalf("%d planned lines; stdout:\n%s", n, out)
		}
	})
}

// failingWriter fails every write after the first ok ones.
type failingWriter struct {
	mu sync.Mutex
	ok int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ok > 0 {
		f.ok--
		return len(p), nil
	}
	return 0, errors.New("no space left on device")
}

func TestOutputWriteErrors(t *testing.T) {
	root := tree(t, map[string]string{"a.jpg": "a", "b.jpg": "b", "c.jpg": "c", "d.jpg": "d"})
	runWith := func(t *testing.T, w io.Writer, args ...string) (*harness, int) {
		h := newHarness(t).withKey()
		h.srv.AddFolder("A")
		env := h.env()
		env.Stdout = w
		return h, Run(context.Background(), args, env)
	}
	t.Run("upload stops starting files", func(t *testing.T) {
		h, code := runWith(t, &failingWriter{}, "upload", "--json", "-c", "1", "-r", root)
		if code != ExitFailure || !strings.Contains(h.stderr.String(), "writing output: no space left on device") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if n := h.srv.Count(testserver.RouteCreate); n != 1 {
			t.Fatalf("creates = %d, want 1: uploads went on after the output failed", n)
		}
	})
	t.Run("human upload output", func(t *testing.T) {
		h, code := runWith(t, &failingWriter{ok: 1}, "upload", "-r", root)
		if code != ExitFailure || !strings.Contains(h.stderr.String(), "writing output") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("dry run", func(t *testing.T) {
		h, code := runWith(t, &failingWriter{}, "upload", "--dry-run", "-r", root)
		if code != ExitFailure || !strings.Contains(h.stderr.String(), "writing output") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	for _, args := range [][]string{{"folders"}, {"folders", "--json"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			h, code := runWith(t, &failingWriter{}, args...)
			if code != ExitFailure || !strings.Contains(h.stderr.String(), "writing output") {
				t.Fatalf("exit %d: %s", code, h.stderr.String())
			}
		})
	}
}
