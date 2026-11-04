package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/export"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

// ExportReporter prints the events of an export download as lines or JSON
// lines. It is not safe for concurrent use; export.Run reports from one
// goroutine.
type ExportReporter struct {
	w      *ErrWriter
	json   bool
	enc    *json.Encoder
	redact func(string) string
	id     int64
}

// NewExportReporter returns a reporter for export id writing to w. redact is
// applied to every error message (nil means none).
func NewExportReporter(w io.Writer, jsonLines bool, id int64, redact func(string) string) *ExportReporter {
	if redact == nil {
		redact = func(s string) string { return s }
	}
	ew := NewErrWriter(w)
	enc := json.NewEncoder(ew)
	enc.SetEscapeHTML(false)
	return &ExportReporter{w: ew, json: jsonLines, enc: enc, redact: redact, id: id}
}

// Err returns the first error writing the output, if any.
func (r *ExportReporter) Err() error { return r.w.Err() }

// ExportLine is the JSON object printed for each event.
type ExportLine struct {
	Event    string `json:"event"`
	ID       int64  `json:"id,omitempty"`
	Created  *bool  `json:"created,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Part     int64  `json:"part,omitempty"`
	Path     string `json:"path,omitempty"`
	Status   string `json:"status,omitempty"`
	Archives *int   `json:"archives,omitempty"`
	Size     *int64 `json:"size,omitempty"`
	Seconds  *int64 `json:"seconds,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

// ExportSummary is the final JSON object.
type ExportSummary struct {
	ID            int64    `json:"id"`
	Status        string   `json:"status"`
	Dir           string   `json:"dir"`
	Downloaded    int      `json:"downloaded"`
	Present       int      `json:"present"`
	Failed        int      `json:"failed"`
	Bytes         int64    `json:"bytes"`
	FailedDomains []string `json:"failed_domains"`
	Interrupted   bool     `json:"interrupted"`
	TimedOut      bool     `json:"timed_out"`
}

// Started reports the export the command follows: created by it, or the
// one already running.
func (r *ExportReporter) Started(created bool) {
	if r.json {
		r.enc.Encode(ExportLine{Event: "started", ID: r.id, Created: &created})
		return
	}
	if created {
		fmt.Fprintf(r.w, "Started export %d.\n", r.id)
	} else {
		fmt.Fprintf(r.w, "Export %d is already running; following it.\n", r.id)
	}
}

// Event reports one step of export.Run.
func (r *ExportReporter) Event(e export.Event) {
	if r.json {
		r.enc.Encode(r.line(e))
		return
	}
	switch e.Kind {
	case export.ExportStatus:
		fmt.Fprintf(r.w, "Export %d: %s\n", r.id, e.Status)
	case export.DomainStatus:
		fmt.Fprintf(r.w, "  %s: %s\n", e.Domain, domainDetail(e))
	case export.PartStarted:
		fmt.Fprintf(r.w, "downloading %s%s\n", e.Path, sizeNote(e.Size, ""))
	case export.PartDone:
		switch e.Result {
		case export.Downloaded:
			fmt.Fprintf(r.w, "downloaded  %s%s\n", e.Path, sizeNote(e.Size, " in "+e.Elapsed.Round(time.Second).String()))
		case export.Present:
			fmt.Fprintf(r.w, "skipped     %s: already downloaded\n", e.Path)
		default:
			fmt.Fprintf(r.w, "failed      %s: %s\n", e.Path, r.errText(e.Err))
		}
	}
}

func (r *ExportReporter) line(e export.Event) ExportLine {
	switch e.Kind {
	case export.ExportStatus:
		return ExportLine{Event: "export", ID: r.id, Status: e.Status}
	case export.DomainStatus:
		n := e.Parts
		return ExportLine{Event: "domain", Domain: e.Domain, Status: e.Status, Archives: &n}
	}
	l := ExportLine{Event: "archive", Domain: e.Domain, Part: e.Part, Path: e.Path, Status: string(e.Result)}
	if e.Kind == export.PartStarted {
		l.Event, l.Status = "downloading", ""
	}
	if e.Size >= 0 {
		size := e.Size
		l.Size = &size
	}
	if e.Result == export.Downloaded {
		secs := int64(e.Elapsed.Round(time.Second) / time.Second)
		l.Seconds = &secs
	}
	if e.Err != nil {
		l.Error = r.errText(e.Err)
		var se *api.StorageError
		if errors.As(e.Err, &se) {
			l.Code = se.Code
		}
	}
	return l
}

// Finish prints the summary of a run into dir.
func (r *ExportReporter) Finish(out export.Outcome, dir string, interrupted, timedOut bool) {
	s := ExportSummary{
		ID: r.id, Dir: dir, Downloaded: out.Downloaded, Present: out.Present, Failed: out.Failed,
		Bytes: out.Bytes, FailedDomains: []string{}, Interrupted: interrupted, TimedOut: timedOut,
	}
	if out.Export != nil {
		s.Status = out.Export.Status
		for _, d := range out.Export.Domains {
			if d.Status == api.ExportFailed || d.Status == api.ExportCancelled {
				s.FailedDomains = append(s.FailedDomains, d.Domain)
			}
		}
	}
	if r.json {
		r.enc.Encode(struct {
			Summary ExportSummary `json:"summary"`
		}{s})
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nExport %d", r.id)
	if s.Status != "" {
		fmt.Fprintf(&b, " %s", s.Status)
	}
	if len(s.FailedDomains) > 0 {
		fmt.Fprintf(&b, " (failed: %s)", strings.Join(s.FailedDomains, ", "))
	}
	fmt.Fprintf(&b, ": %d downloaded, %d already there, %d failed (%s written) in %s.", s.Downloaded, s.Present, s.Failed, upload.HumanSize(s.Bytes), dir)
	if interrupted {
		b.WriteString(" Interrupted.")
	}
	if timedOut {
		b.WriteString(" Timed out.")
	}
	fmt.Fprintln(r.w, b.String())
}

func (r *ExportReporter) errText(err error) string {
	if err == nil {
		return ""
	}
	return r.redact(err.Error())
}

func domainDetail(e export.Event) string {
	if !api.IsFinalStatus(e.Status) {
		return e.Status
	}
	switch {
	case e.Parts == 0 && e.Status == api.ExportCompleted:
		return e.Status + ", no files"
	case e.Parts == 0:
		return e.Status
	case e.Parts == 1:
		return e.Status + ", 1 archive"
	}
	return fmt.Sprintf("%s, %d archives", e.Status, e.Parts)
}

func sizeNote(size int64, extra string) string {
	if size < 0 {
		if extra == "" {
			return ""
		}
		return " (" + strings.TrimPrefix(extra, " ") + ")"
	}
	return " (" + upload.HumanSize(size) + extra + ")"
}
