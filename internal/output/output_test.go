package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

func sampleResults() []upload.Result {
	limitErr := &api.Error{Op: api.OpCreateAsset, StatusCode: 403, Code: api.CodeUploadLimitReached, Message: "Limit reached for KEY-SECRET-1"}
	return []upload.Result{
		{Item: upload.Item{Path: "photos/a.jpg", Dest: upload.Destination{Path: "Campaigns"}}, Status: upload.StatusUploaded, Key: "0123456789abcdef0123456789abcdef", URL: "https://app.i.optimole.com/a.jpg", FolderID: 12},
		{Item: upload.Item{Path: "photos/b.bmp", Status: upload.StatusSkipped}, Status: upload.StatusSkipped, Err: errors.New("unsupported file type .bmp")},
		{Item: upload.Item{Path: "photos/c.jpg", Dest: upload.Destination{ID: 7}}, Status: upload.StatusFailed, Err: limitErr},
		{Item: upload.Item{Path: "photos/d.jpg"}, Status: upload.StatusFailed, Err: errors.New("not attempted"), NotAttempted: true},
	}
}

func redactor(s string) string { return strings.ReplaceAll(s, "KEY-SECRET-1", "[REDACTED]") }

func TestJSONLines(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, true, redactor)
	for _, res := range sampleResults() {
		r.Result(res)
	}
	r.Finish(3, false, false)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines:\n%s", len(lines), buf.String())
	}
	var first FileLine
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	want := FileLine{Path: "photos/a.jpg", Folder: "Campaigns", FolderID: 12, Status: "uploaded", Key: "0123456789abcdef0123456789abcdef", URL: "https://app.i.optimole.com/a.jpg"}
	if first != want {
		t.Fatalf("line 1 = %+v", first)
	}
	var third FileLine
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatal(err)
	}
	if third.Status != "failed" || third.Code != api.CodeUploadLimitReached || third.FolderID != 7 || third.Folder != "" ||
		strings.Contains(third.Error, "KEY-SECRET-1") || !strings.Contains(third.Error, "[REDACTED]") {
		t.Fatalf("line 3 = %+v", third)
	}
	var sum struct {
		Summary Summary `json:"summary"`
	}
	if err := json.Unmarshal([]byte(lines[4]), &sum); err != nil {
		t.Fatal(err)
	}
	if (sum.Summary != Summary{Total: 4, Uploaded: 1, Skipped: 1, Failed: 2, NotAttempted: 1, Ignored: 3}) {
		t.Fatalf("summary = %+v", sum.Summary)
	}
	// Every file line has the required keys.
	for _, l := range lines[:4] {
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		for _, k := range []string{"path", "folder", "status"} {
			if _, ok := m[k]; !ok {
				t.Errorf("%s lacks %q", l, k)
			}
		}
	}
}

func TestHumanLines(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false, redactor)
	for _, res := range sampleResults() {
		r.Result(res)
	}
	s := r.Finish(1, false, true)
	out := buf.String()
	for _, want := range []string{
		"uploaded  photos/a.jpg -> Campaigns  https://app.i.optimole.com/a.jpg\n",
		"skipped   photos/b.bmp: unsupported file type .bmp\n",
		"failed    photos/c.jpg -> folder #7: create asset: Limit reached for [REDACTED] (HTTP 403; upload_limit_reached)\n",
		"\n1 uploaded, 1 skipped, 2 failed, 1 of them not attempted (4 files; 1 hidden or excluded entry ignored). Interrupted.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "photos/d.jpg") {
		t.Errorf("not-attempted file listed in human output:\n%s", out)
	}
	if !s.Interrupted || s.Failed != 2 {
		t.Errorf("summary = %+v", s)
	}
}

func TestDryRun(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, false, nil)
	r.Planned(upload.Item{Path: "a.jpg", Size: 1536, Dest: upload.Destination{}})
	r.Finish(0, true, false)
	want := "planned   a.jpg -> (library root) (1.5 KB)\n\nDry run: 1 to upload, 0 skipped (1 file).\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}

	buf.Reset()
	r = New(&buf, true, nil)
	r.Planned(upload.Item{Path: "a.jpg", Size: 1536, Dest: upload.Destination{Path: "X"}})
	r.Finish(0, true, false)
	want = "{\"path\":\"a.jpg\",\"folder\":\"X\",\"status\":\"planned\",\"size\":1536}\n{\"summary\":{\"total\":1,\"uploaded\":0,\"planned\":1,\"skipped\":0,\"failed\":0,\"ignored\":0,\"dry_run\":true}}\n"
	if buf.String() != want {
		t.Fatalf("got %q\nwant %q", buf.String(), want)
	}
}

type brokenWriter struct{ writes int }

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.writes++
	return 0, errors.New("broken pipe")
}

func TestWriteErrorsAreKept(t *testing.T) {
	for _, jsonLines := range []bool{false, true} {
		w := &brokenWriter{}
		r := New(w, jsonLines, nil)
		for _, res := range sampleResults() {
			r.Result(res)
		}
		r.Finish(0, false, false)
		if r.Err() == nil || r.Err().Error() != "broken pipe" {
			t.Fatalf("json=%v: Err = %v", jsonLines, r.Err())
		}
		if w.writes != 1 {
			t.Fatalf("json=%v: %d writes after the first failure, want none", jsonLines, w.writes-1)
		}
	}
}
