package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/testserver"
)

// keysFile writes lines to a file and returns its path.
func keysFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// addAssets puts n images in the fake library and returns their keys.
func addAssets(h *harness, n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = h.srv.AddAsset(testserver.Asset{Filename: fmt.Sprintf("%03d.jpg", i)})
	}
	return keys
}

// deleteRequests returns the DELETE /api/assets events.
func deleteRequests(h *harness) []testserver.Event {
	var out []testserver.Event
	for _, e := range h.srv.Events() {
		if e.Route == testserver.RouteDeleteAssets {
			out = append(out, e)
		}
	}
	return out
}

func TestDeleteKeys(t *testing.T) {
	h := newHarness(t).withKey()
	keys := addAssets(h, 3)
	// Blank lines and spaces are ignored, case does not matter, a repeated
	// key is sent once.
	file := keysFile(t, "", "  "+strings.ToUpper(keys[0])+"  ", "", keys[1], keys[0], "\t")
	if code := h.run("delete", "--keys", file, "--yes"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != "Deleted 2 images.\n" {
		t.Errorf("stdout = %q", got)
	}
	req := deleteRequests(h)
	if len(req) != 1 || strings.Join(req[0].Keys, ",") != keys[0]+","+keys[1] {
		t.Fatalf("requests = %+v", req)
	}
	if n := len(h.srv.Deleted()); n != 2 {
		t.Fatalf("%d deleted, want 2", n)
	}

	// From stdin, which then cannot answer a question: --yes.
	h.stdin = strings.NewReader(keys[2] + "\n")
	if code := h.run("delete", "--keys", "-", "--yes"); code != ExitOK || h.stdout.String() != "Deleted 1 image.\n" {
		t.Fatalf("exit %d: %q %s", code, h.stdout.String(), h.stderr.String())
	}
	if n := len(h.srv.Deleted()); n != 3 {
		t.Fatalf("%d deleted, want 3", n)
	}
}

func TestDeleteKeysInBatchesOf100(t *testing.T) {
	h := newHarness(t).withKey()
	keys := addAssets(h, 250)
	if code := h.run("delete", "--keys", keysFile(t, keys...), "--yes", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	lines := h.jsonLines()
	if len(lines) != 1 || lines[0]["scope"] != "keys" || lines[0]["requested"] != float64(250) || lines[0]["deleted"] != float64(250) {
		t.Fatalf("lines = %v", lines)
	}
	if _, ok := lines[0]["maybe_deleted"]; ok {
		t.Errorf("maybe_deleted on success: %v", lines[0])
	}
	var sizes []int
	for _, e := range deleteRequests(h) {
		sizes = append(sizes, len(e.Keys))
	}
	if fmt.Sprint(sizes) != "[100 100 50]" {
		t.Fatalf("request sizes = %v", sizes)
	}
	if n := len(h.srv.Deleted()); n != 250 {
		t.Fatalf("%d deleted", n)
	}
}

func TestDeleteKeysStopsAtTheFirstFailure(t *testing.T) {
	t.Run("a refused request", func(t *testing.T) {
		h := newHarness(t).withKey()
		keys := addAssets(h, 250)
		unknown := strings.Repeat("ab", 16)
		keys[150] = unknown
		if code := h.run("delete", "--keys", keysFile(t, keys...), "--yes"); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		msg := h.stderr.String()
		for _, want := range []string{
			"The following keys are invalid or unauthorized: " + strings.ToUpper(unknown) + " (HTTP 422)",
			"Deleted 100 of 250 images before it stopped; the other 150 were not deleted.",
			"To finish, run the command again without the first 100 keys: a deleted key counts as invalid if named again.",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("stderr lacks %q:\n%s", want, msg)
			}
		}
		if n := len(deleteRequests(h)); n != 2 {
			t.Errorf("%d requests, want 2: the run goes on after a failure", n)
		}
		if n := len(h.srv.Deleted()); n != 100 {
			t.Errorf("%d deleted, want 100", n)
		}
		if h.stdout.Len() != 0 {
			t.Errorf("stdout = %q", h.stdout.String())
		}
	})
	t.Run("an unknown outcome", func(t *testing.T) {
		for _, jsonOut := range []bool{false, true} {
			h := newHarness(t).withKey()
			keys := addAssets(h, 250)
			var calls atomic.Int32
			h.srv.Hook = func(route testserver.Route, _ *http.Request) {
				if route == testserver.RouteDeleteAssets && calls.Add(1) == 2 {
					h.srv.Inject(route, testserver.Response{Status: 500, Body: `{"message":"Server Error"}`})
				}
			}
			args := []string{"delete", "--keys", keysFile(t, keys...), "--yes"}
			if jsonOut {
				args = append(args, "--json")
			}
			if code := h.run(args...); code != ExitFailure {
				t.Fatalf("exit %d", code)
			}
			if want := "Deleted 100 of 250 images before it stopped; 100 more may be deleted too (the request failed), the other 50 were not."; !strings.Contains(h.stderr.String(), want) {
				t.Errorf("stderr lacks %q:\n%s", want, h.stderr.String())
			}
			if !jsonOut {
				continue
			}
			l := h.jsonLines()
			if len(l) != 1 || l[0]["deleted"] != float64(100) || l[0]["maybe_deleted"] != float64(100) || !strings.Contains(l[0]["error"].(string), "HTTP 500") {
				t.Errorf("lines = %v", l)
			}
		}
	})
	t.Run("the first request refused", func(t *testing.T) {
		h := newHarness(t).withKey()
		if code := h.run("delete", "--keys", keysFile(t, strings.Repeat("cd", 16)), "--yes"); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		if msg := h.stderr.String(); !strings.Contains(msg, "Nothing was deleted.") || strings.Contains(msg, "To finish") || strings.Contains(msg, "next run") {
			t.Errorf("stderr:\n%s", msg)
		}
	})
}

func TestDeleteFolder(t *testing.T) {
	h := newHarness(t).withKey()
	campaigns := h.srv.AddFolder("Campaigns")
	year := h.srv.AddFolder("Campaigns/2026")
	spring := h.srv.AddFolder("Campaigns/2026/Spring")
	h.srv.AddAsset(testserver.Asset{Filename: "a.jpg", FolderID: year})
	h.srv.AddAsset(testserver.Asset{Filename: "b.jpg", FolderID: spring})
	h.srv.AddAsset(testserver.Asset{Filename: "kept.jpg", FolderID: campaigns})
	h.srv.AddAsset(testserver.Asset{Filename: "root.jpg"})

	// Paths match case-insensitively, as the server reads them.
	if code := h.run("delete", "--folder", " /campaigns/2026/ ", "--yes"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != "Deleting 2 images in the background. They leave the library within a few minutes.\n" {
		t.Errorf("stdout = %q", got)
	}
	if req := deleteRequests(h); len(req) != 1 || req[0].FolderID != year || req[0].Keys != nil {
		t.Fatalf("requests = %+v", req)
	}
	if n := len(h.srv.Deleted()); n != 2 {
		t.Fatalf("%d deleted, want 2", n)
	}

	if code := h.run("delete", "--folder-id", fmt.Sprint(year), "--yes", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	l := h.jsonLines()
	if len(l) != 1 || l[0]["scope"] != "folder" || l[0]["folder_id"] != float64(year) || l[0]["folder"] != "Campaigns/2026" || l[0]["deleting"] != float64(0) {
		t.Fatalf("lines = %v", l)
	}
	if code := h.run("delete", "--folder-id", fmt.Sprint(year), "--yes"); code != ExitOK || h.stdout.String() != "Nothing to delete: the folder \"Campaigns/2026\" has no images.\n" {
		t.Fatalf("exit %d: %q", code, h.stdout.String())
	}
}

func TestDeleteFolderMustBeFoundFirst(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddFolder("Brand")
	for args, want := range map[[2]string]string{
		{"--folder", "Nope"}:    `no folder has the path "Nope"`,
		{"--folder-id", "999"}:  "no folder has the ID 999",
		{"--folder", "Brand/x"}: `no folder has the path "Brand/x"`,
	} {
		if code := h.run("delete", args[0], args[1], "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), want) {
			t.Errorf("%v: exit %d: %s", args, code, h.stderr.String())
		}
	}
	// Two folders with one path (a race can make them): neither is guessed.
	h.srv.Inject(testserver.RouteFolders, testserver.Response{Status: 200, Body: `{"data":[` +
		`{"id":5,"name":"Brand","parent_id":null,"status":"active","is_default":false},` +
		`{"id":6,"name":"brand","parent_id":null,"status":"active","is_default":false}],"meta":{"next_cursor":null,"has_more":false}}`})
	if code := h.run("delete", "--folder", "BRAND", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), `2 folders have the path "BRAND" (IDs 5, 6); name one with --folder-id`) {
		t.Errorf("exit %d: %s", code, h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 0 {
		t.Fatalf("%d delete requests", n)
	}
}

func TestDeleteWebsiteFolder(t *testing.T) {
	h := newHarness(t).withKey()
	folder := h.srv.AddSite("example.com")
	h.srv.AddAsset(testserver.Asset{Filename: "filed.jpg", FolderID: folder, Domain: "example.com"})
	h.srv.AddAsset(testserver.Asset{Filename: "unfiled.jpg", Domain: "example.com"})
	h.srv.AddAsset(testserver.Asset{Filename: "other.jpg", Domain: "other.com"})
	h.tty = true
	h.stdin = strings.NewReader("example.com\n")
	if code := h.run("delete", "--folder", "example.com"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if want := `About to delete every image in the folder "example.com" (ID ` + fmt.Sprint(folder) + `) and its subfolders, ` +
		`and the images of the website example.com that are in no folder; the folders are kept.`; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, h.stderr.String())
	}
	if !strings.HasPrefix(h.stdout.String(), "Deleting 2 images") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestDeleteSite(t *testing.T) {
	h := newHarness(t).withKey()
	folder := h.srv.AddFolder("Blog")
	h.srv.AddSite("example.com")
	h.srv.AddAsset(testserver.Asset{Filename: "a.jpg", Domain: "example.com"})
	h.srv.AddAsset(testserver.Asset{Filename: "b.jpg", Domain: "example.com", FolderID: folder})
	h.srv.AddAsset(testserver.Asset{Filename: "c.jpg", Domain: "other.com"})
	if code := h.run("delete", "--site", "https://WWW.Example.com/blog", "--yes", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	l := h.jsonLines()
	if len(l) != 1 || l[0]["scope"] != "site" || l[0]["domain"] != "example.com" || l[0]["deleting"] != float64(2) {
		t.Fatalf("lines = %v", l)
	}
	// The host goes, with its "www.": the server strips one, as the prompt did.
	if req := deleteRequests(h); len(req) != 1 || req[0].Domain != "www.example.com" {
		t.Fatalf("requests = %+v", req)
	}
	if n := len(h.srv.Events()); n != 1 {
		t.Errorf("%d requests, want only the delete", n)
	}

	if code := h.run("delete", "--site", "other.com", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), "The account has no website with this domain. (HTTP 422)") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

func TestDeleteAsksFirst(t *testing.T) {
	type tc struct {
		args   []string
		answer string
		prompt string
		ok     bool
	}
	h := newHarness(t).withKey()
	key := addAssets(h, 1)[0]
	folder := h.srv.AddFolder("Campaigns/2026")
	h.srv.AddSite("example.com")
	file := keysFile(t, key)
	for _, c := range []tc{
		{[]string{"--keys", file}, "yes\n", `About to delete 1 image (the keys in ` + file + `). This cannot be undone`, false},
		{[]string{"--keys", file}, "", `Type "delete" to confirm: `, false},
		{[]string{"--folder-id", fmt.Sprint(folder)}, "campaigns/2026\n", `Type "Campaigns/2026" to confirm: `, false},
		{[]string{"--site", "Example.com"}, "Example.com\n", `Type "example.com" to confirm: `, false},
		{[]string{"--keys", file}, " delete \n", `Type "delete" to confirm: `, true},
		{[]string{"--folder", "campaigns/2026"}, "Campaigns/2026\n", "every image in the folder \"Campaigns/2026\" (ID " + fmt.Sprint(folder) + ") and its subfolders; the folders are kept.", true},
		{[]string{"--site", "example.com"}, "example.com\n", "every image of the website example.com, in any folder.", true},
	} {
		before := len(deleteRequests(h))
		h.tty = true
		h.stdin = strings.NewReader(c.answer)
		code := h.run(append([]string{"delete"}, c.args...)...)
		if !strings.Contains(h.stderr.String(), c.prompt) {
			t.Errorf("%v: stderr lacks %q:\n%s", c.args, c.prompt, h.stderr.String())
		}
		sent := len(deleteRequests(h)) - before
		if c.ok && (code != ExitOK || sent != 1) {
			t.Errorf("%v answered %q: exit %d, %d requests", c.args, c.answer, code, sent)
		}
		if !c.ok && (code != ExitFailure || sent != 0 || !strings.Contains(h.stderr.String(), "the answer did not match; nothing was deleted")) {
			t.Errorf("%v answered %q: exit %d, %d requests: %s", c.args, c.answer, code, sent, h.stderr.String())
		}
	}
}

// TestDeleteInterruptedAtTheQuestion: Ctrl-C at the prompt returns at once,
// with nothing deleted.
func TestDeleteInterruptedAtTheQuestion(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddFolder("A")
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	h.tty = true
	h.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan int, 1)
	go func() { done <- h.runCtx(ctx, "delete", "--folder", "A") }()
	select {
	case code := <-done:
		if code != ExitInterrupted || !strings.Contains(h.stderr.String(), "interrupted; nothing was deleted") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		pw.Close()
		t.Fatal("the question ignored Ctrl-C")
	}
	if n := len(deleteRequests(h)); n != 0 {
		t.Fatalf("%d delete requests", n)
	}
}

// TestDeleteNeedsYesWithoutATerminal: nothing can be confirmed without a
// terminal, so nothing is deleted, or even looked up, without --yes.
func TestDeleteNeedsYesWithoutATerminal(t *testing.T) {
	h := newHarness(t).withKey()
	key := addAssets(h, 1)[0]
	h.srv.AddFolder("A")
	h.srv.AddSite("example.com")
	for _, args := range [][]string{
		{"--keys", keysFile(t, key)},
		{"--keys", "-"},
		{"--folder", "A"},
		{"--folder-id", "101"},
		{"--site", "example.com"},
	} {
		h.stdin = strings.NewReader(key + "\ndelete\n")
		code := h.run(append([]string{"delete"}, args...)...)
		if code != ExitUsage || !strings.Contains(h.stderr.String(), "add --yes to delete without asking") {
			t.Errorf("%v: exit %d: %s", args, code, h.stderr.String())
		}
	}
	// Keys typed at a terminal could not be told from the answer.
	h.tty = true
	if code := h.run("delete", "--keys", "-", "--yes"); code != ExitUsage || !strings.Contains(h.stderr.String(), "pipe them in, or name a file") {
		t.Errorf("--keys - on a terminal: exit %d: %s", code, h.stderr.String())
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("%d requests", n)
	}
	if n := len(h.srv.Deleted()); n != 0 {
		t.Fatalf("%d deleted", n)
	}
}

func TestDeleteUsageErrors(t *testing.T) {
	h := newHarness(t).withKey()
	key := addAssets(h, 1)[0]
	dir := t.TempDir()
	usage := map[string][]string{
		"nothing named":         {},
		"keys and site":         {"--keys", keysFile(t, key), "--site", "example.com"},
		"folder and folder-id":  {"--folder", "A", "--folder-id", "3"},
		"an argument":           {"--site", "example.com", "extra"},
		"bad folder ID":         {"--folder-id", "abc"},
		"folder ID 0":           {"--folder-id", "0"},
		"the root":              {"--folder", "/"},
		"empty folder name":     {"--folder", "a//b"},
		"empty site":            {"--site", ""},
		"not a web URL":         {"--site", "ftp://example.com"},
		"empty keys":            {"--keys", ""},
		"missing keys file":     {"--keys", filepath.Join(dir, "missing.txt")},
		"keys file is a folder": {"--keys", dir},
		"no keys in the file":   {"--keys", keysFile(t, "", "  ")},
		"a line that is no key": {"--keys", keysFile(t, key, "abc")},
		"bad boolean":           {"--keys", keysFile(t, key), "--yes=maybe"},
	}
	for name, args := range usage {
		code := h.run(append(append([]string{"delete"}, args...), "--yes")...)
		if code != ExitUsage {
			t.Errorf("%s: exit %d, want 2; stderr: %s", name, code, h.stderr.String())
		}
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("%d requests on usage errors", n)
	}

	// A bad line is not echoed: it could be anything, an API key too.
	const secret = "optml-pasted-api-key-by-mistake-0123456789"
	if code := h.run("delete", "--keys", keysFile(t, key, secret), "--yes"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if msg := h.stderr.String(); strings.Contains(msg, secret) || !strings.Contains(msg, "line 2 of") {
		t.Fatalf("stderr: %s", msg)
	}
}

func TestDeleteErrors(t *testing.T) {
	t.Run("rejected key", func(t *testing.T) {
		h := newHarness(t)
		h.vars[config.EnvAPIKey] = "optml-not-a-key-000000"
		if code := h.run("delete", "--keys", keysFile(t, strings.Repeat("ab", 16)), "--yes"); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		if msg := h.stderr.String(); !strings.Contains(msg, "The API key (from OPTIMOLE_API_KEY) was rejected.") || strings.Contains(msg, "optml-not-a-key") {
			t.Fatalf("stderr: %s", msg)
		}
	})
	t.Run("folder-scoped key", func(t *testing.T) {
		h := newHarness(t)
		const scoped = "optml-sScopedKeyForDeletes0123"
		h.srv.AddKey(scoped, testserver.Key{Scoped: true})
		h.vars[config.EnvAPIKey] = scoped
		key := addAssets(h, 1)[0]
		h.srv.AddFolder("A")
		// The keys are refused by DELETE, the folder already by its lookup.
		for _, args := range [][]string{{"--keys", keysFile(t, key)}, {"--folder", "A"}} {
			if code := h.run(append([]string{"delete", "--yes"}, args...)...); code != ExitFailure {
				t.Fatalf("%v: exit %d", args, code)
			}
			if msg := h.stderr.String(); !strings.Contains(msg, "Deleting images needs a full-access API key") || strings.Contains(msg, scoped) {
				t.Errorf("%v: stderr: %s", args, msg)
			}
		}
		if len(h.srv.Deleted()) != 0 {
			t.Fatal("a scoped key deleted")
		}
	})
	t.Run("other refusals", func(t *testing.T) {
		h := newHarness(t).withKey()
		folder := h.srv.AddFolder("A")
		h.srv.Inject(testserver.RouteDeleteAssets,
			testserver.Response{Status: 403, Body: `{"message":"Your plan is not active."}`},
			testserver.Response{Status: 422, Body: `{"message":"The selected folder is invalid","errors":{"folder":["The selected folder is invalid"]}}`})
		for _, want := range []string{"Your plan is not active. (HTTP 403)", "delete assets: The selected folder is invalid (HTTP 422)"} {
			if code := h.run("delete", "--folder-id", fmt.Sprint(folder), "--yes"); code != ExitFailure {
				t.Fatalf("exit %d", code)
			}
			if msg := h.stderr.String(); !strings.Contains(msg, want) || strings.Contains(msg, "full-access") || strings.Contains(msg, "may have started") {
				t.Errorf("stderr lacks %q (or says too much):\n%s", want, msg)
			}
		}
	})
	t.Run("lost answer", func(t *testing.T) {
		h := newHarness(t).withKey()
		h.srv.AddSite("example.com")
		h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{CommitThenClose: true})
		if code := h.run("delete", "--site", "example.com", "--yes"); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(h.stderr.String(), "The deletion may have started anyway") {
			t.Errorf("stderr: %s", h.stderr.String())
		}
		if n := len(deleteRequests(h)); n != 1 {
			t.Errorf("%d requests, want 1: emptying a website is never repeated blindly", n)
		}
	})
}

func TestDeleteRedactsTheKey(t *testing.T) {
	const key = "optml-very-secret-key-for-delete-42"
	echo := fmt.Sprintf(`{"message":"Key %s cannot do that.","errors":{"keys":["bad %s"]}}`, key, key)
	for _, jsonOut := range []bool{false, true} {
		h := newHarness(t)
		h.srv.AddKey(key, testserver.Key{})
		h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 422, Body: echo})
		args := []string{"delete", "--api-key", key, "--keys", keysFile(t, addAssets(h, 1)...), "--yes"}
		if jsonOut {
			args = append(args, "--json")
		}
		if code := h.run(args...); code != ExitFailure {
			t.Fatalf("exit %d", code)
		}
		all := h.stdout.String() + h.stderr.String()
		if strings.Contains(all, key) || !strings.Contains(all, "[REDACTED]") {
			t.Fatalf("json=%v output:\n%s", jsonOut, all)
		}
	}
	// A key given to --site, --folder-id or --keys by mistake is not echoed.
	h := newHarness(t)
	h.vars[config.EnvAPIKey] = key
	for _, flag := range []string{"--site", "--folder-id", "--keys"} {
		if code := h.run("delete", flag, "x/"+key+"://", "--yes"); code != ExitUsage || strings.Contains(h.stderr.String(), key) {
			t.Fatalf("%s: exit %d: %s", flag, code, h.stderr.String())
		}
	}
}

func TestDeleteHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run("delete", "-h"); code != ExitOK || !strings.Contains(h.stderr.String(), "--folder-id N") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if code := h.run("help"); code != ExitOK || !strings.Contains(h.stdout.String(), "delete     Delete images") {
		t.Fatalf("exit %d: %s", code, h.stdout.String())
	}
}

func TestDeleteReportsOutputFailures(t *testing.T) {
	h := newHarness(t).withKey()
	keys := addAssets(h, 1)
	env := h.env()
	env.Stdout = &failingWriter{}
	if code := Run(context.Background(), []string{"delete", "--keys", keysFile(t, keys...), "--yes"}, env); code != ExitFailure || !strings.Contains(h.stderr.String(), "writing output") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

// TestDeleteInterruptedWhileReadingKeys: a pipe that never ends must not keep
// Ctrl-C from working.
func TestDeleteInterruptedWhileReadingKeys(t *testing.T) {
	h := newHarness(t).withKey()
	key := addAssets(h, 1)[0]
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	go pw.Write([]byte(key + "\n"))
	h.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan int, 1)
	go func() { done <- h.runCtx(ctx, "delete", "--keys", "-", "--yes") }()
	select {
	case code := <-done:
		if code != ExitInterrupted || !strings.Contains(h.stderr.String(), "interrupted; nothing was deleted") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	case <-time.After(5 * time.Second):
		pw.Close()
		t.Fatal("reading the keys ignored Ctrl-C")
	}
	if n := len(h.srv.Events()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}

// cancelAfter cancels the run once the first answer to route arrives.
type cancelAfter struct {
	next   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodDelete {
		c.cancel()
	}
	return resp, err
}

// TestDeleteInterruptedBetweenRequests: Ctrl-C after a request finished stops
// before the next is sent, so the count is exact.
func TestDeleteInterruptedBetweenRequests(t *testing.T) {
	h := newHarness(t).withKey()
	keys := addAssets(h, 250)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := h.env()
	env.Transport = cancelAfter{next: env.Transport, cancel: cancel}
	if code := Run(ctx, []string{"delete", "--keys", keysFile(t, keys...), "--yes"}, env); code != ExitInterrupted {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if want := "Deleted 100 of 250 images before it stopped; the other 150 were not deleted."; !strings.Contains(h.stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// TestDeleteSiteStripsOneWWW: the server strips one "www.", as the prompt
// does, so "www.www.example.com" is the website www.example.com, never
// example.com.
func TestDeleteSiteStripsOneWWW(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddSite("example.com")
	h.srv.AddSite("www.example.com")
	h.srv.AddAsset(testserver.Asset{Filename: "a.jpg", Domain: "example.com"})
	h.srv.AddAsset(testserver.Asset{Filename: "b.jpg", Domain: "www.example.com"})
	h.tty = true
	h.stdin = strings.NewReader("www.example.com\n")
	if code := h.run("delete", "--site", "www.www.example.com"); code != ExitOK || !strings.Contains(h.stderr.String(), "every image of the website www.example.com,") {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	deleted := h.srv.Deleted()
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v", deleted)
	}
	for _, a := range h.srv.Assets() {
		if a.Deleted && a.Domain != "www.example.com" {
			t.Fatalf("deleted an image of %s", a.Domain)
		}
	}
}

// TestDeleteNeedsEnter: an answer that ends without Enter (Ctrl-D after
// typing it, or a line too long to read whole) deletes nothing.
func TestDeleteNeedsEnter(t *testing.T) {
	h := newHarness(t).withKey()
	file := keysFile(t, addAssets(h, 1)...)
	for _, answer := range []string{"delete", "delete" + strings.Repeat(" ", maxKeyInput)} {
		h.tty = true
		h.stdin = strings.NewReader(answer)
		if code := h.run("delete", "--keys", file); code != ExitFailure || !strings.Contains(h.stderr.String(), "the answer did not match; nothing was deleted") {
			t.Fatalf("answer of %d bytes: exit %d: %s", len(answer), code, h.stderr.String())
		}
	}
	if n := len(deleteRequests(h)); n != 0 {
		t.Fatalf("%d delete requests", n)
	}
}

// TestDeleteLongFolderPathAsksForTheName: a path longer than a terminal line
// cannot be typed, so its name is asked for instead.
func TestDeleteLongFolderPathAsksForTheName(t *testing.T) {
	h := newHarness(t).withKey()
	segs := make([]string, 17)
	for i := range segs {
		segs[i] = strings.Repeat(string(rune('a'+i)), 255)
	}
	long := strings.Join(segs, "/")
	folder := h.srv.AddFolder(long)
	h.tty = true
	h.stdin = strings.NewReader(segs[16] + "\n")
	if code := h.run("delete", "--folder-id", fmt.Sprint(folder)); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), fmt.Sprintf("Type %q to confirm: ", segs[16])) {
		t.Fatalf("stderr: %s", h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 1 {
		t.Fatalf("%d delete requests", n)
	}
}

// TestDeleteKeysAfterALostAnswer: a keys request whose answer was lost may
// have deleted. It is not repeated (the repeat would be refused for the keys
// it deleted), and the command says what to do next.
func TestDeleteKeysAfterALostAnswer(t *testing.T) {
	h := newHarness(t).withKey()
	keys := addAssets(h, 1)
	h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{CommitThenClose: true})
	if code := h.run("delete", "--keys", keysFile(t, keys...), "--yes", "--json"); code != ExitFailure {
		t.Fatalf("exit %d", code)
	}
	msg := h.stderr.String()
	for _, want := range []string{"1 of 1 image may be deleted (the request failed).", "If the next run lists keys as invalid, the request that failed deleted them"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stderr lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Nothing was deleted") || strings.Contains(msg, "To finish") {
		t.Errorf("stderr:\n%s", msg)
	}
	if n := len(deleteRequests(h)); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	if l := h.jsonLines(); len(l) != 1 || l[0]["deleted"] != float64(0) || l[0]["maybe_deleted"] != float64(1) {
		t.Errorf("lines = %v", l)
	}
	if n := len(h.srv.Deleted()); n != 1 {
		t.Fatalf("%d deleted: the first attempt did delete", n)
	}
}

// refusingTransport fails every request before it connects.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("dial tcp: connection refused")
}

func TestDeleteInBackgroundOutcomes(t *testing.T) {
	t.Run("an answer that does not read", func(t *testing.T) {
		// The server accepted it, so the deletion runs.
		h := newHarness(t).withKey()
		h.srv.AddSite("example.com")
		h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 202, Body: `{"assets":`})
		if code := h.run("delete", "--site", "example.com", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), "The deletion may have started anyway") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("never connected", func(t *testing.T) {
		h := newHarness(t).withKey()
		env := h.env()
		env.Transport = refusingTransport{}
		code := Run(context.Background(), []string{"delete", "--site", "example.com", "--yes"}, env)
		if code != ExitFailure || !strings.Contains(h.stderr.String(), "connection refused") || strings.Contains(h.stderr.String(), "may have started") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
	})
	t.Run("a 5xx", func(t *testing.T) {
		h := newHarness(t).withKey()
		h.srv.AddSite("example.com")
		h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 504, Body: `{"message":"Gateway Timeout"}`})
		if code := h.run("delete", "--site", "example.com", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), "The deletion may have started anyway") {
			t.Fatalf("exit %d: %s", code, h.stderr.String())
		}
		if n := len(deleteRequests(h)); n != 1 {
			t.Fatalf("%d requests: a 504 is not repeated", n)
		}
	})
}

// TestDeleteHidesKeysAndControlCharacters: whatever is printed (the folder
// asked for, the folder found, the prompt, the JSON, the server's message)
// shows no known API key and no raw control character.
func TestDeleteHidesKeysAndControlCharacters(t *testing.T) {
	const key = "optml-very-secret-key-for-delete-77"
	h := newHarness(t)
	h.vars[config.EnvAPIKey] = key
	h.srv.AddKey(key, testserver.Key{})
	folder := h.srv.AddFolder("Shared/" + key)
	check := func(what string) {
		t.Helper()
		if all := h.stdout.String() + h.stderr.String(); strings.Contains(all, key) || strings.Contains(all, "\x1b") {
			t.Errorf("%s printed a key or an escape:\n%q", what, all)
		}
	}
	h.run("delete", "--folder", key, "--yes")
	check("a folder not found")
	if !strings.Contains(h.stderr.String(), `no folder has the path "[REDACTED]"`) {
		t.Errorf("stderr: %s", h.stderr.String())
	}
	h.tty = true
	h.stdin = strings.NewReader("Shared/[REDACTED]\n")
	if code := h.run("delete", "--folder-id", fmt.Sprint(folder)); code != ExitFailure {
		t.Errorf("a redacted answer was taken: exit %d", code)
	}
	check("the prompt")
	h.tty = false
	if code := h.run("delete", "--folder-id", fmt.Sprint(folder), "--yes", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	check("the JSON")
	if l := h.jsonLines(); len(l) != 1 || l[0]["folder"] != "Shared/[REDACTED]" {
		t.Errorf("lines = %v", l)
	}
	h.run("delete", "--keys", filepath.Join(t.TempDir(), "\x1b[2Jmissing.txt"), "--yes")
	check("a keys file that does not open")
	h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 422, Body: `{"message":"Bad \u001b[2J folder"}`})
	h.run("delete", "--folder-id", fmt.Sprint(folder), "--yes")
	check("the server's message")
	if !strings.Contains(h.stderr.String(), `\x1b[2J`) {
		t.Errorf("the escape is not shown quoted: %q", h.stderr.String())
	}
}

// TestDeleteFolderNeedsTheWholeList: a path two folders share could hide in
// the part of the list that did not come back, so it is not guessed.
func TestDeleteFolderNeedsTheWholeList(t *testing.T) {
	h := newHarness(t).withKey()
	page := `{"data":[{"id":5,"name":"Brand","parent_id":null,"status":"active","is_default":false}],"meta":{"next_cursor":%s,"has_more":true}}`
	for _, next := range []string{`null`, `"same"`} {
		h.srv.Inject(testserver.RouteFolders, testserver.Response{Status: 200, Body: fmt.Sprintf(page, next)}, testserver.Response{Status: 200, Body: fmt.Sprintf(page, next)})
		if code := h.run("delete", "--folder", "Brand", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), "the folder list came back incomplete") {
			t.Fatalf("next cursor %s: exit %d: %s", next, code, h.stderr.String())
		}
	}
	// An ID is unique: found in what came back, it is the folder.
	h.srv.Inject(testserver.RouteFolders, testserver.Response{Status: 200, Body: fmt.Sprintf(page, `null`)})
	h.srv.Inject(testserver.RouteDeleteAssets, testserver.Response{Status: 202, Body: `{"assets":0}`})
	if code := h.run("delete", "--folder-id", "5", "--yes"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 1 {
		t.Fatalf("%d delete requests", n)
	}
}

// TestDeleteSiteSendsTheHost: only the host goes, so a long URL stays under
// the server's 255-character limit, and a domain the CLI cannot map as the
// server does (IDNA) is refused rather than shown wrong.
func TestDeleteSiteSendsTheHost(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.AddSite("example.com")
	if code := h.run("delete", "--site", "https://Example.com/"+strings.Repeat("p", 300), "--yes"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if req := deleteRequests(h); len(req) != 1 || req[0].Domain != "example.com" {
		t.Fatalf("requests = %+v", req)
	}
	for _, site := range []string{"İ.com", "bücher.de", "https://www.", "www."} {
		if code := h.run("delete", "--site", site, "--yes"); code != ExitUsage {
			t.Errorf("%s: exit %d: %s", site, code, h.stderr.String())
		}
	}
	if !strings.Contains(h.stderr.String(), `"www." is not a domain`) {
		t.Errorf("stderr: %s", h.stderr.String())
	}
	h.run("delete", "--site", "bücher.de", "--yes")
	if !strings.Contains(h.stderr.String(), "give the domain in its xn-- form") {
		t.Errorf("stderr: %s", h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 1 {
		t.Fatalf("%d delete requests", n)
	}
}

// TestDeleteFolderLookAlikePaths: a root folder named "2026 / Drafts" reads
// as the nested 2026/Drafts, so neither is guessed.
func TestDeleteFolderLookAlikePaths(t *testing.T) {
	h := newHarness(t).withKey()
	h.srv.Inject(testserver.RouteFolders, testserver.Response{Status: 200, Body: `{"data":[` +
		`{"id":5,"name":"2026 / Drafts","parent_id":null,"status":"active","is_default":false},` +
		`{"id":6,"name":"2026","parent_id":null,"status":"active","is_default":false},` +
		`{"id":7,"name":"Drafts","parent_id":6,"status":"active","is_default":false}],"meta":{"next_cursor":null,"has_more":false}}`})
	if code := h.run("delete", "--folder", "2026 / Drafts", "--yes"); code != ExitFailure || !strings.Contains(h.stderr.String(), `2 folders have the path "2026/Drafts" (IDs 5, 7)`) {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if n := len(deleteRequests(h)); n != 0 {
		t.Fatalf("%d delete requests", n)
	}
}
