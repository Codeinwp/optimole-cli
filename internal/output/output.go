// Package output prints upload results as human-readable lines or as JSON
// lines, and keeps the counts for the final summary.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

// Summary counts the files of one run.
type Summary struct {
	Total    int `json:"total"`
	Uploaded int `json:"uploaded"`
	Planned  int `json:"planned,omitempty"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	// NotAttempted counts failed files that were never sent because the run
	// stopped early; they are included in Failed.
	NotAttempted int `json:"not_attempted,omitempty"`
	// Ignored counts hidden and excluded entries, which are not listed.
	Ignored     int  `json:"ignored"`
	DryRun      bool `json:"dry_run,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
}

// ErrWriter passes writes through and remembers the first error, so callers
// can print freely and check once.
type ErrWriter struct {
	w   io.Writer
	err error
}

// NewErrWriter wraps w.
func NewErrWriter(w io.Writer) *ErrWriter { return &ErrWriter{w: w} }

// Write writes p unless an earlier write failed.
func (e *ErrWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		e.err = err
	}
	return n, err
}

// Err returns the first write error, if any.
func (e *ErrWriter) Err() error { return e.err }

// Reporter prints results. It is not safe for concurrent use; the runner
// calls it from one goroutine.
type Reporter struct {
	w      *ErrWriter
	json   bool
	enc    *json.Encoder
	redact func(string) string
	sum    Summary
}

// New returns a Reporter writing to w. redact is applied to every error
// message (nil means none).
func New(w io.Writer, jsonLines bool, redact func(string) string) *Reporter {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	ew := NewErrWriter(w)
	enc := json.NewEncoder(ew)
	enc.SetEscapeHTML(false)
	return &Reporter{w: ew, json: jsonLines, enc: enc, redact: redact}
}

// Err returns the first error writing the output, if any. Once set, nothing
// more is written.
func (r *Reporter) Err() error { return r.w.Err() }

// FileLine is the JSON object printed for each file.
type FileLine struct {
	Path     string `json:"path"`
	Folder   string `json:"folder"`
	FolderID int64  `json:"folder_id,omitempty"`
	Status   string `json:"status"`
	Size     int64  `json:"size,omitempty"`
	Key      string `json:"key,omitempty"`
	URL      string `json:"url,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

// Result reports one file's outcome (uploaded, skipped or failed).
func (r *Reporter) Result(res upload.Result) {
	r.sum.Total++
	switch res.Status {
	case upload.StatusUploaded:
		r.sum.Uploaded++
	case upload.StatusSkipped:
		r.sum.Skipped++
	default:
		r.sum.Failed++
		if res.NotAttempted {
			r.sum.NotAttempted++
		}
	}
	msg := ""
	if res.Err != nil {
		msg = r.redact(res.Err.Error())
	}

	if r.json {
		line := r.line(res.Item, res.Status)
		line.Key, line.URL, line.Error = res.Key, res.URL, msg
		if res.FolderID > 0 {
			line.FolderID = res.FolderID
		}
		var apiErr *api.Error
		if errors.As(res.Err, &apiErr) {
			line.Code = apiErr.Code
		}
		r.enc.Encode(line)
		return
	}

	switch res.Status {
	case upload.StatusUploaded:
		fmt.Fprintf(r.w, "uploaded  %s -> %s  %s\n", res.Item.DisplayPath(), res.Item.Dest, res.URL)
	case upload.StatusSkipped:
		fmt.Fprintf(r.w, "skipped   %s: %s\n", res.Item.DisplayPath(), msg)
	default:
		if res.NotAttempted {
			// Listed only in JSON; the summary gives the count.
			return
		}
		if res.Item.Status == upload.StatusFailed {
			// Failed while planning (unreadable directory).
			fmt.Fprintf(r.w, "failed    %s: %s\n", res.Item.DisplayPath(), msg)
			return
		}
		fmt.Fprintf(r.w, "failed    %s -> %s: %s\n", res.Item.DisplayPath(), res.Item.Dest, msg)
	}
}

// Planned reports a file a dry run would upload.
func (r *Reporter) Planned(it upload.Item) {
	r.sum.Total++
	r.sum.Planned++
	if r.json {
		r.enc.Encode(r.line(it, upload.StatusPlanned))
		return
	}
	fmt.Fprintf(r.w, "planned   %s -> %s (%s)\n", it.DisplayPath(), it.Dest, upload.HumanSize(it.Size))
}

func (r *Reporter) line(it upload.Item, status upload.Status) FileLine {
	l := FileLine{Path: it.DisplayPath(), Folder: it.Dest.Path, Status: string(status)}
	if it.Dest.ID > 0 {
		l.Folder = ""
		l.FolderID = it.Dest.ID
	}
	if status == upload.StatusPlanned {
		l.Size = it.Size
	}
	return l
}

// Summary returns the counts so far.
func (r *Reporter) Summary() Summary { return r.sum }

// Finish prints the summary and returns it.
func (r *Reporter) Finish(ignored int, dryRun, interrupted bool) Summary {
	r.sum.Ignored = ignored
	r.sum.DryRun = dryRun
	r.sum.Interrupted = interrupted
	s := r.sum
	if r.json {
		r.enc.Encode(struct {
			Summary Summary `json:"summary"`
		}{s})
		return s
	}
	var b strings.Builder
	if dryRun {
		fmt.Fprintf(&b, "\nDry run: %d to upload, %d skipped", s.Planned, s.Skipped)
		if s.Failed > 0 {
			fmt.Fprintf(&b, ", %d unreadable", s.Failed)
		}
	} else {
		fmt.Fprintf(&b, "\n%d uploaded, %d skipped, %d failed", s.Uploaded, s.Skipped, s.Failed)
		if s.NotAttempted > 0 {
			fmt.Fprintf(&b, ", %d of them not attempted", s.NotAttempted)
		}
	}
	fmt.Fprintf(&b, " (%d %s", s.Total, plural(s.Total, "file", "files"))
	if s.Ignored > 0 {
		fmt.Fprintf(&b, "; %d hidden or excluded %s ignored", s.Ignored, plural(s.Ignored, "entry", "entries"))
	}
	b.WriteString(")")
	b.WriteString(".")
	if interrupted {
		b.WriteString(" Interrupted.")
	}
	fmt.Fprintln(r.w, b.String())
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
