package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

const (
	urlHash  = "42cf6621995747f69a30d32f6a6c4f82"
	urlImage = "https://mlabc.i.optimole.com/w:auto/h:auto/q:auto/id:" + urlHash + "/directUpload/photo.jpg"
)

// libURL is the URL the fake dashboard gives an image asset.
func libURL(key, name string) string {
	return "https://app.i.optimole.com/w:auto/h:auto/q:auto/id:" + key + "/directUpload/" + name
}

func TestURLChangesURLsWithoutTheAPI(t *testing.T) {
	h := newHarness(t) // no API key: URLs need none
	tests := []struct {
		args []string
		want string
	}{
		{[]string{urlImage, "--width", "800", "--format", "webp"},
			"https://mlabc.i.optimole.com/w:800/h:auto/q:auto/f:webp/id:" + urlHash + "/directUpload/photo.jpg"},
		{[]string{"--resize", "fill", "--width", "400", "--height", "300", "--gravity", "top-left", urlImage},
			"https://mlabc.i.optimole.com/w:400/h:300/q:auto/rt:fill/g:nowe/id:" + urlHash + "/directUpload/photo.jpg"},
		{[]string{urlImage, "--focus", "0.25:0.75", "--quality", "60", "--dpr", "2"},
			"https://mlabc.i.optimole.com/w:auto/h:auto/q:60/g:fp:0.25:0.75/dpr:2/id:" + urlHash + "/directUpload/photo.jpg"},
		{[]string{urlImage, "--download", "--set", "bl:5", "--set", "cb:aB3d.1f"},
			"https://mlabc.i.optimole.com/cb:aB3d.1f/w:auto/h:auto/q:auto/bl:5/att:1/id:" + urlHash + "/directUpload/photo.jpg"},
		// An option already there is replaced, never repeated.
		{[]string{"https://mlabc.i.optimole.com/w:300/h:200/q:60/f:webp/bl:5/id:" + urlHash + "/directUpload/a.jpg", "--width", "100"},
			"https://mlabc.i.optimole.com/w:100/h:200/q:60/f:webp/bl:5/id:" + urlHash + "/directUpload/a.jpg"},
		{[]string{"https://mlabc.i.optimole.com/w:300/h:200/q:60/f:webp/bl:5/id:" + urlHash + "/directUpload/a.jpg", "--format", "auto", "--unset", "bl"},
			"https://mlabc.i.optimole.com/w:300/h:200/q:60/id:" + urlHash + "/directUpload/a.jpg"},
		{[]string{"https://mlabc.i.optimole.com/w:300/h:200/q:60/f:webp/bl:5/id:" + urlHash + "/directUpload/a.jpg", "--reset", "--width", "50"},
			"https://mlabc.i.optimole.com/w:50/h:auto/q:auto/id:" + urlHash + "/directUpload/a.jpg"},
		// No flags: the URL as the CDN reads it.
		{[]string{urlImage}, urlImage},
	}
	for _, tt := range tests {
		if code := h.run(append([]string{"url"}, tt.args...)...); code != ExitOK {
			t.Fatalf("url %v: exit %d, stderr %s", tt.args, code, h.stderr.String())
		}
		if got := strings.TrimSpace(h.stdout.String()); got != tt.want {
			t.Errorf("url %v:\n got %s\nwant %s", tt.args, got, tt.want)
		}
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("%d API calls for URL inputs", n)
	}
}

func TestURLLooksUpKeys(t *testing.T) {
	h := newHarness(t).withKey()
	key := h.srv.AddAsset(testserver.Asset{Filename: "photo.jpg", ContentType: "image/jpeg", Size: 5, Width: 1200, Height: 800})
	if code := h.run("url", strings.ToUpper(key), "--width", "300"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	want := "https://app.i.optimole.com/w:300/h:auto/q:auto/id:" + key + "/directUpload/photo.jpg"
	if got := strings.TrimSpace(h.stdout.String()); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if code := h.run("url", key, "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	lines := h.jsonLines()
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	got := lines[0]
	if got["input"] != key || got["url"] != libURL(key, "photo.jpg") || got["key"] != key ||
		got["mime_type"] != "image/jpeg" || got["original_width"] != 1200.0 || got["original_height"] != 800.0 {
		t.Fatalf("line = %v", got)
	}
}

func TestURLReadsStdinInOrderWhileLookingUpInParallel(t *testing.T) {
	h := newHarness(t).withKey()
	slow := h.srv.AddAsset(testserver.Asset{Filename: "slow.jpg", ContentType: "image/jpeg"})
	fast := h.srv.AddAsset(testserver.Asset{Filename: "fast.jpg", ContentType: "image/jpeg"})
	// The first lookup answers only once the second one has arrived: the
	// lookups run together, and the output still follows the input.
	arrived := make(chan struct{})
	var once sync.Once
	h.srv.Hook = func(route testserver.Route, r *http.Request) {
		if route != testserver.RouteAsset {
			return
		}
		if strings.HasSuffix(r.URL.Path, slow) {
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Error("the lookups did not run in parallel")
			}
			return
		}
		once.Do(func() { close(arrived) })
	}
	h.stdin = strings.NewReader(strings.Join([]string{
		slow,
		"",
		urlImage,
		`{"path":"a.jpg","folder":"","status":"uploaded","key":"` + urlHash + `","url":"` + urlImage + `"}`,
		`{"path":"b.psd","folder":"","status":"skipped","error":"type not allowed"}`,
		fast,
		"nonsense",
		`{"summary":{"total":2,"uploaded":1,"skipped":1,"failed":0,"ignored":0}}`,
	}, "\n"))
	code := h.run("url", "--width", "64")
	if code != ExitFailure {
		t.Fatalf("exit %d, want 1 (two bad lines): %s", code, h.stderr.String())
	}
	w64 := func(u string) string { return strings.Replace(u, "/w:auto/", "/w:64/", 1) }
	want := strings.Join([]string{w64(libURL(slow, "slow.jpg")), w64(urlImage), w64(urlImage), w64(libURL(fast, "fast.jpg"))}, "\n")
	if got := strings.TrimSpace(h.stdout.String()); got != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", got, want)
	}
	stderr := h.stderr.String()
	for _, msg := range []string{
		`optimole url: b.psd: the JSON line has no "url" (status skipped)`,
		"optimole url: nonsense: not an asset key",
	} {
		if !strings.Contains(stderr, msg) {
			t.Errorf("stderr lacks %q:\n%s", msg, stderr)
		}
	}
}

func TestURLReadsUploadOutput(t *testing.T) {
	h := newHarness(t).withKey()
	root := tree(t, map[string]string{"a.jpg": "aaa", "b.png": "bbb"})
	if code := h.run("upload", "-r", root, "--json"); code != ExitOK {
		t.Fatalf("upload: exit %d: %s", code, h.stderr.String())
	}
	// The upload's summary line is skipped, not reported as a bad line.
	h.stdin = strings.NewReader(h.stdout.String())
	if code := h.run("url", "--width", "100"); code != ExitOK || h.stderr.Len() != 0 {
		t.Fatalf("url: exit %d, stderr %q", code, h.stderr.String())
	}
	got := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if len(got) != 2 || !strings.Contains(got[0]+got[1], "/w:100/h:auto/q:auto/id:") {
		t.Fatalf("stdout = %q", got)
	}
	if n := h.srv.Count(testserver.RouteAsset); n != 0 {
		t.Fatalf("%d lookups: the upload lines carry their URLs", n)
	}
}

func TestURLPipesIntoItself(t *testing.T) {
	h := newHarness(t).withKey()
	key := h.srv.AddAsset(testserver.Asset{Filename: "photo.jpg", ContentType: "image/jpeg", Width: 640, Height: 480})
	if code := h.run("url", key, "--width", "320", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	h.stdin = strings.NewReader(h.stdout.String())
	if code := h.run("url", "--format", "webp", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	got := h.jsonLines()[0]
	want := "https://app.i.optimole.com/w:320/h:auto/q:auto/f:webp/id:" + key + "/directUpload/photo.jpg"
	// The asset's details travel along the pipe.
	if got["url"] != want || got["key"] != key || got["mime_type"] != "image/jpeg" || got["original_width"] != 640.0 {
		t.Fatalf("line = %v", got)
	}
	if n := h.srv.Count(testserver.RouteAsset); n != 1 {
		t.Fatalf("%d lookups, want 1: the second command reads the URL", n)
	}
}

func TestURLFiles(t *testing.T) {
	h := newHarness(t).withKey()
	key := h.srv.AddAsset(testserver.Asset{Filename: "notes.pdf", ContentType: "application/pdf"})
	fileURL := "https://app.i.optimole.com/files/id:" + key + "/directUpload/notes.pdf"
	if code := h.run("url", key); code != ExitOK || strings.TrimSpace(h.stdout.String()) != fileURL {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, h.stdout.String(), h.stderr.String())
	}
	if code := h.run("url", key, "--width", "100", "--json"); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := h.jsonLines()[0]; got["code"] != "not_an_image" || got["url"] != nil {
		t.Fatalf("line = %v", got)
	}
}

func TestURLLookupFailures(t *testing.T) {
	h := newHarness(t).withKey()
	good := h.srv.AddAsset(testserver.Asset{Filename: "a.jpg", ContentType: "image/jpeg"})
	pending := h.srv.AddAsset(testserver.Asset{Filename: "big.zip", ContentType: "application/zip", NoURL: true})
	unknown := strings.Repeat("ab", 16)
	code := h.run("url", unknown, pending, good, "--json")
	if code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	lines := h.jsonLines()
	if len(lines) != 3 || lines[0]["code"] != "not_found" || lines[1]["code"] != "no_url" || lines[2]["url"] != libURL(good, "a.jpg") {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0]["error"].(string), "no such asset, or this API key cannot see it") {
		t.Fatalf("error = %v", lines[0]["error"])
	}
}

func TestURLStopsOnARejectedKey(t *testing.T) {
	h := newHarness(t)
	h.vars[config.EnvAPIKey] = "optml-not-a-key"
	code := h.run("url", urlImage, strings.Repeat("ab", 16), strings.Repeat("cd", 16), strings.Repeat("ef", 16))
	if code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := strings.TrimSpace(h.stdout.String()); got != urlImage {
		t.Fatalf("stdout %q: the URL before the keys is printed, nothing after", got)
	}
	if n := strings.Count(h.stderr.String(), "was rejected"); n != 1 {
		t.Fatalf("the rejection is reported %d times:\n%s", n, h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "optml-not-a-key") {
		t.Fatal("the key is in the output")
	}
}

func TestURLNeedsAnAPIKeyOnlyForKeys(t *testing.T) {
	h := newHarness(t)
	code := h.run("url", urlImage, urlHash)
	if code != ExitUsage {
		t.Fatalf("exit %d, want 2", code)
	}
	if got := strings.TrimSpace(h.stdout.String()); got != urlImage {
		t.Fatalf("stdout %q", got)
	}
	if !strings.Contains(h.stderr.String(), "API key") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
}

func TestURLUsageErrors(t *testing.T) {
	h := newHarness(t).withKey()
	for _, args := range [][]string{
		{"--format", "avif"},
		{"--format", "best"},
		{"--set", "cb:abc123"},
		{"--width", "10", "--set", "w:20"},
		{"--gravity", "top", "--focus", "0.5:0.5"},
		{"--format", "webp", "--unset", "f"},
		{"--unset", "id"},
		{"--unset", "width"},
		{"--set", "id:" + urlHash},
		{"--set", "nocolon"},
		{"--set", "bl:1/2"},
		{"--set", "g:fp:0,1"},
		{"--set", "cb:x:abc.d"},
		{"--focus", "NaN:0"},
		{"--width", "0"},
		{"--width", ""},
		{"--resize", "crop"},
		{"--quality", "101"},
		{"--focus", "2:2"},
		{"-c", "0"},
		{"-c", "17"},
	} {
		code := h.run(append([]string{"url", urlImage}, args...)...)
		if code != ExitUsage || h.stdout.Len() != 0 {
			t.Errorf("url %v: exit %d, stdout %q; want 2 and nothing printed", args, code, h.stdout.String())
		}
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("%d API calls for usage errors", n)
	}
}

func TestURLInterrupted(t *testing.T) {
	h := newHarness(t).withKey()
	key := h.srv.AddAsset(testserver.Asset{Filename: "a.jpg", ContentType: "image/jpeg"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.srv.Hook = func(route testserver.Route, _ *http.Request) {
		if route == testserver.RouteAsset {
			cancel()
		}
	}
	if code := h.runCtx(ctx, "url", key); code != ExitInterrupted {
		t.Fatalf("exit %d, want 130", code)
	}
	if !strings.Contains(h.stderr.String(), "interrupted") || h.stdout.Len() != 0 {
		t.Fatalf("stdout %q, stderr %q", h.stdout.String(), h.stderr.String())
	}
}

func TestURLRefusesAnOverlongLine(t *testing.T) {
	h := newHarness(t)
	h.stdin = strings.NewReader(urlImage + "\n" + strings.Repeat("x", maxURLLine+1) + "\n")
	if code := h.run("url"); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.TrimSpace(h.stdout.String()) != urlImage || !strings.Contains(h.stderr.String(), "longer than") {
		t.Fatalf("stdout %q, stderr %q", h.stdout.String(), h.stderr.String())
	}
}

func TestURLHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run("help"); code != ExitOK || !strings.Contains(h.stdout.String(), "  url ") {
		t.Fatalf("main help does not list url: %s", h.stdout.String())
	}
	if code := h.run("url", "-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "--format F") {
		t.Fatalf("exit %d, help: %s", code, h.stderr.String())
	}
}

func TestURLInterruptedWhileStdinIsIdle(t *testing.T) {
	h := newHarness(t)
	pr, pw := io.Pipe()
	defer pw.Close()
	h.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := make(chan int, 1)
	go func() { done <- h.runCtx(ctx, "url") }()
	select {
	case code := <-done:
		if code != ExitInterrupted {
			t.Fatalf("exit %d, want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not stop a run waiting for stdin")
	}
}

func TestURLLimitsParallelLookups(t *testing.T) {
	for _, limit := range []int{1, 2} {
		h := newHarness(t).withKey()
		var keys []string
		for i := 0; i < 6; i++ {
			keys = append(keys, h.srv.AddAsset(testserver.Asset{Filename: fmt.Sprintf("%d.jpg", i), ContentType: "image/jpeg"}))
		}
		var mu sync.Mutex
		inside, most := 0, 0
		// Each lookup waits until limit lookups are inside, so a limit that
		// lets more through shows up as more at once.
		h.srv.Hook = func(route testserver.Route, _ *http.Request) {
			if route != testserver.RouteAsset {
				return
			}
			mu.Lock()
			inside++
			most = max(most, inside)
			mu.Unlock()
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				mu.Lock()
				n := inside
				mu.Unlock()
				if n >= limit {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
		}
		if code := h.run(append([]string{"url", "-c", strconv.Itoa(limit)}, keys...)...); code != ExitOK {
			t.Fatalf("-c %d: exit %d: %s", limit, code, h.stderr.String())
		}
		if most != limit {
			t.Fatalf("-c %d: at most %d lookups at once, want %d", limit, most, limit)
		}
	}
}

func TestURLReportsOutputFailures(t *testing.T) {
	h := newHarness(t)
	env := h.env()
	env.Stdout = &failingWriter{}
	if code := Run(context.Background(), []string{"url", urlImage, urlImage}, env); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "writing output: no space left on device") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
}

func TestURLRefusesNewlinesInURLs(t *testing.T) {
	h := newHarness(t)
	injected := urlImage + "\nhttps://evil.example/x.jpg"
	h.stdin = strings.NewReader(`{"url":"` + strings.ReplaceAll(injected, "\n", `\n`) + `"}` + "\n")
	if code := h.run("url"); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if h.stdout.Len() != 0 || strings.Contains(h.stderr.String(), "\nhttps://evil") {
		t.Fatalf("stdout %q, stderr %q: the newline went through", h.stdout.String(), h.stderr.String())
	}
	if code := h.run("url", injected); code != ExitFailure || h.stdout.Len() != 0 {
		t.Fatalf("argument: exit %d, stdout %q", code, h.stdout.String())
	}
}

func TestURLDoesNotEchoAPIKeys(t *testing.T) {
	h := newHarness(t)
	secret := "my.saved.secret.key"
	h.vars[config.EnvAPIKey] = secret
	if code := h.run("url", secret, "https://example.com/x/"+secret, "--json"); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, secret) || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("the configured key is printed:\n%s", out)
	}
	if code := h.run("url", secret, "https://example.com/x/"+secret); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, secret) || strings.Count(out, "[REDACTED]") != 2 {
		t.Fatalf("the configured key is printed:\n%s", out)
	}
	scoped := "optml-s" + strings.Repeat("Ab3", 19)
	if code := h.run("url", scoped); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if strings.Contains(h.stderr.String(), scoped) || !strings.Contains(h.stderr.String(), "looks like an API key") {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
}

func TestURLRefusesURLsTheCDNWouldMisread(t *testing.T) {
	h := newHarness(t)
	raw := "https://mlabc.i.optimole.com/w:300/https://example.com/w:9/a.jpg"
	if code := h.run("url", raw, "--width", "100"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if code := h.run("url", raw, "--unset", "w", "--json"); code != ExitFailure {
		t.Fatalf("exit %d, want 1", code)
	}
	if got := h.jsonLines()[0]; got["code"] != "invalid_input" || !strings.Contains(got["error"].(string), `"w:9"`) {
		t.Fatalf("line = %v", got)
	}
}

func TestURLRedactsKeysInEveryMessage(t *testing.T) {
	h := newHarness(t)
	secret := "my.saved.secret.key"
	h.vars[config.EnvAPIKey] = secret
	// In a usage error.
	if code := h.run("url", urlImage, "--width", secret); code != ExitUsage {
		t.Fatalf("exit %d, want 2", code)
	}
	if strings.Contains(h.stderr.String(), secret) || !strings.Contains(h.stderr.String(), "[REDACTED]") {
		t.Fatalf("usage error: %s", h.stderr.String())
	}
	// In the flag package's own errors, which never show the bad value,
	// whatever key it is.
	other := "flag.only.secret.key"
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"-c", secret}, "invalid value for flag -c"},
		{[]string{"--json=" + secret}, "invalid boolean value for -json"},
		{[]string{"--api-key", other, "-c", other}, "invalid value for flag -c"},
		{[]string{"--width", "--", "--api-key", other, "-c", other}, "invalid value for flag -c"},
		{[]string{"--api-key", other, "-c", secret, "--api-key", "x"}, "invalid value for flag -c"},
		{[]string{"---api-key=" + secret}, "bad flag syntax"},
		{[]string{"--optml-s" + strings.Repeat("Ab3", 19)}, "what was typed is not shown"},
		{[]string{"---api-key=\n" + secret}, "bad flag syntax\n"},
		{[]string{"--api-key " + secret}, "what was typed is not shown"},
		{[]string{"--widht", "300"}, "flag provided but not defined: -widht"},
	} {
		if code := h.run(append([]string{"url", urlImage}, tt.args...)...); code != ExitUsage {
			t.Fatalf("%v: exit %d, want 2", tt.args, code)
		}
		if got := h.stderr.String(); strings.Contains(got, secret) || strings.Contains(got, other) || !strings.Contains(got, tt.want) {
			t.Fatalf("%v: %s", tt.args, got)
		}
	}
	// In an error's own text, in both output modes.
	withCredentials := "https://" + secret + "@mlabc.i.optimole.com/id:" + urlHash + "/a.jpg"
	for _, args := range [][]string{{"url", withCredentials}, {"url", withCredentials, "--json"}} {
		if code := h.run(args...); code != ExitFailure {
			t.Fatalf("%v: exit %d, want 1", args, code)
		}
		if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, secret) || !strings.Contains(out, "invalid host") {
			t.Fatalf("%v printed the key:\n%s", args, out)
		}
	}
	// In a long line, which is shortened only after the key is hidden.
	long := strings.Repeat("x", 110) + " " + secret // cut at 120 characters, inside the key
	for _, args := range [][]string{{"url", long}, {"url", long, "--json"}} {
		if code := h.run(args...); code != ExitFailure {
			t.Fatalf("exit %d, want 1", code)
		}
		if out := h.stdout.String() + h.stderr.String(); strings.Contains(out, secret[:6]) {
			t.Fatalf("%v printed part of the key:\n%s", args[2:], out)
		}
	}
}
