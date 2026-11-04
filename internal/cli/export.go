package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/export"
	"github.com/Codeinwp/optimole-cli/internal/output"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

const exportUsage = `Usage:
  optimole export <command> [flags] [arguments]

Exports the original files of the whole library (each whitelisted domain, plus
"directUpload" for the files uploaded straight to the library) as ZIP archives
of about 1 GB each, and downloads them. Exports need a full-access API key.

Commands:
  start          Start an export, wait for it and download its archives
  download [ID]  Download an export's archives (default: the latest export),
                 waiting while it runs and resuming an earlier download
  status [ID]    Show an export's domains and archives (default: the latest)
  list           List the account's exports, newest first
  delete ID      Delete a finished export and its archives

An account keeps at most 5 exports and runs one at a time. Finished exports are
deleted automatically 10 days after they finish.

Run "optimole export <command> -h" for a command's flags.
`

const downloadFlagsUsage = `  -o, --output DIR     Folder the export folder goes into (default: the current folder)
  --domain NAME        Only download this domain's archives (repeatable)
  -c, --concurrency N  Archives downloaded in parallel, 1-8 (default 2)
  --timeout DURATION   Give up after this long, e.g. 90m or 2h (default: no limit)
  --json               Print one JSON object per event and a final summary object
`

const downloadNotes = `
Ctrl-C and --timeout stop only the CLI, never the export on the server, and
"optimole export download ID" picks up where the run stopped.

Exit codes: 0 the export completed and every selected archive is downloaded,
1 the export failed or was cancelled, an archive failed, or the timeout passed,
2 usage or configuration error, 130 interrupted.
`

const exportStartUsage = `Usage:
  optimole export start [flags]

Starts an export of the whole library, follows it, and downloads each archive
as soon as it is ready, to DIR/optimole-export-ID/DOMAIN/images-N.zip, with an
export.json describing them. If an export is already running, follows that
one instead.

Flags:
` + downloadFlagsUsage + `  --no-wait            Start the export and print its ID, without waiting
` + commonUsage + downloadNotes

const exportDownloadUsage = `Usage:
  optimole export download [ID|latest] [flags]

Downloads the archives of an export (default: the latest) to
DIR/optimole-export-ID/DOMAIN/images-N.zip, with an export.json describing
them. While the export runs, each archive is downloaded as soon as it is ready.
Archives already downloaded are skipped and partial ones resumed.

Flags:
` + downloadFlagsUsage + commonUsage + downloadNotes

const exportStatusUsage = `Usage:
  optimole export status [ID|latest] [--json]

Shows an export's status and, for each domain, its status and finished
archives (default: the latest export).

Flags:
  --json   Print the export as one JSON object (without download URLs)
` + commonUsage

const exportListUsage = `Usage:
  optimole export list [--json]

Lists the account's exports, newest first.

Flags:
  --json   Print one JSON object per export
` + commonUsage

const exportDeleteUsage = `Usage:
  optimole export delete ID

Deletes a finished export and its archives on the server, which frees one of
the account's 5 export slots. Downloaded files are not touched.
` + commonUsage

const (
	// exportLimit is how many exports the dashboard lets an account keep.
	exportLimit = 5
	// exportsPerPage is more than an account keeps, so one page lists them.
	exportsPerPage = 20
	maxExportPages = 100
)

func runExport(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, exportUsage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "start":
		return runExportStart(ctx, rest, env)
	case "download":
		return runExportDownload(ctx, rest, env)
	case "status":
		return runExportStatus(ctx, rest, env)
	case "list":
		return runExportList(ctx, rest, env)
	case "delete":
		return runExportDelete(ctx, rest, env)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(env.Stdout, exportUsage)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "optimole export: unknown command %q\n\n%s", cmd, exportUsage)
		return ExitUsage
	}
}

// downloadFlags are the flags of "export start" and "export download".
type downloadFlags struct {
	common      commonFlags
	output      string
	domains     stringList
	concurrency int
	timeout     time.Duration
	jsonOut     bool
}

func (d *downloadFlags) register(fs *flag.FlagSet) {
	d.common.register(fs, true)
	fs.StringVar(&d.output, "o", ".", "")
	fs.StringVar(&d.output, "output", ".", "")
	fs.Var(&d.domains, "domain", "")
	fs.IntVar(&d.concurrency, "c", export.DefaultConcurrency, "")
	fs.IntVar(&d.concurrency, "concurrency", export.DefaultConcurrency, "")
	fs.DurationVar(&d.timeout, "timeout", 0, "")
	fs.BoolVar(&d.jsonOut, "json", false, "")
}

// check validates the flags; it returns a usage problem or "".
func (d *downloadFlags) check() string {
	switch {
	case d.concurrency < 1 || d.concurrency > export.MaxConcurrency:
		return fmt.Sprintf("--concurrency must be between 1 and %d", export.MaxConcurrency)
	case d.timeout < 0:
		return "--timeout must not be negative"
	case strings.TrimSpace(d.output) == "":
		return "--output must not be empty"
	}
	for _, dom := range d.domains {
		if strings.TrimSpace(dom) == "" {
			return "--domain must not be empty"
		}
	}
	return ""
}

func runExportStart(ctx context.Context, args []string, env Env) int {
	const name = "export start"
	fs := newFlagSet(name, exportStartUsage, env)
	var df downloadFlags
	df.register(fs)
	var noWait bool
	fs.BoolVar(&noWait, "no-wait", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, name, "takes no arguments")
	}
	if msg := df.check(); msg != "" {
		return usageError(env, name, "%s", msg)
	}
	sess, err := df.common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	runCtx, cancel := withTimeout(ctx, df.timeout)
	defer cancel()

	id, created, code := startExport(runCtx, env, sess)
	if code != ExitOK {
		return code
	}
	rep := output.NewExportReporter(env.Stdout, df.jsonOut, id, sess.redact.String)
	rep.Started(created)
	if noWait {
		if rep.Err() != nil {
			return outputFailed(env, rep.Err())
		}
		return ExitOK
	}
	return downloadExport(ctx, runCtx, env, sess, rep, name, id, &df)
}

// startExport creates an export, or finds the one already running. It
// returns the export ID, whether this call created it, and an exit code.
func startExport(ctx context.Context, env Env, sess *session) (int64, bool, int) {
	const name = "export start"
	exp, err := sess.client.CreateExport(ctx)
	if err == nil {
		return exp.ID, true, ExitOK
	}
	var apiErr *api.Error
	var netErr *api.NetworkError
	switch {
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
		if apiErr.Code == api.CodeExportInProgress && apiErr.ExportID.Valid {
			return apiErr.ExportID.Value, false, ExitOK
		}
		// Servers without error codes answer both conflicts with a bare 409:
		// the list says which one it is. Below the limit the conflict can
		// only be the newest export, running then (it may have finished
		// since).
		exports, listErr := listAllExports(ctx, sess.client)
		if listErr != nil {
			return 0, false, exportError(env, sess, name, firstCancel(ctx, err))
		}
		if apiErr.Code != api.CodeExportLimitReached && len(exports) > 0 && (!api.IsFinalStatus(exports[0].Status) || len(exports) < exportLimit) {
			return exports[0].ID, false, ExitOK
		}
		fmt.Fprintf(env.Stderr, "optimole %s: %s\n", name, sess.redact.Error(err))
		if apiErr.Code == api.CodeExportLimitReached || len(exports) >= exportLimit {
			printLimitHint(env, exports)
		}
		return 0, false, ExitFailure
	case errors.As(err, &netErr) && netErr.MaybeCommitted:
		// The export may have started: follow it if one runs.
		exports, listErr := listAllExports(ctx, sess.client)
		if listErr == nil && len(exports) > 0 && !api.IsFinalStatus(exports[0].Status) {
			return exports[0].ID, false, ExitOK
		}
	}
	return 0, false, exportError(env, sess, name, firstCancel(ctx, err))
}

// shellQuote writes a path for a command to paste: as is when it holds only
// characters no shell treats specially, else in single quotes, or on Windows
// in double quotes (a path cannot contain them), where a backslash run before
// the closing quote is doubled so it does not escape the quote. A backslash
// needs quotes outside Windows. No one Windows form suits both cmd.exe and
// PowerShell: both still expand "%" or "$" inside double quotes, which a
// folder name rarely holds.
func shellQuote(s string) string {
	windows := runtime.GOOS == "windows"
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool {
		if r == '\\' {
			return !windows
		}
		return !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) || strings.ContainsRune("._-/:+,", r))
	}) < 0
	switch {
	case plain:
		return s
	case windows:
		trailing := len(s) - len(strings.TrimRight(s, `\`))
		return `"` + s + strings.Repeat(`\`, trailing) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firstCancel returns the context's error when it ended (so Ctrl-C during
// a fallback lookup still exits 130), else err.
func firstCancel(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func printLimitHint(env Env, exports []api.Export) {
	hint := `Delete one with "optimole export delete ID"; finished exports are also deleted 10 days after they finish.`
	for i := len(exports) - 1; i >= 0; i-- {
		if api.IsFinalStatus(exports[i].Status) {
			e := exports[i]
			hint += fmt.Sprintf(" Oldest: export %d (%s, %s).", e.ID, e.Status, displayTime(e.CreatedAt))
			break
		}
	}
	fmt.Fprintln(env.Stderr, hint)
}

func runExportDownload(ctx context.Context, args []string, env Env) int {
	const name = "export download"
	fs := newFlagSet(name, exportDownloadUsage, env)
	var df downloadFlags
	df.register(fs)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 1 {
		return usageError(env, name, "takes at most one export ID")
	}
	if msg := df.check(); msg != "" {
		return usageError(env, name, "%s", msg)
	}
	ref := "latest"
	if len(rest) == 1 {
		ref = rest[0]
	}
	if _, err := parseExportRef(ref); err != nil {
		return usageError(env, name, "%v", err)
	}
	sess, err := df.common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	runCtx, cancel := withTimeout(ctx, df.timeout)
	defer cancel()
	id, code := resolveExport(runCtx, env, sess, name, ref)
	if code != ExitOK {
		return code
	}
	rep := output.NewExportReporter(env.Stdout, df.jsonOut, id, sess.redact.String)
	return downloadExport(ctx, runCtx, env, sess, rep, name, id, &df)
}

// downloadExport runs the download of export id. ctx is the command's own
// context (Ctrl-C), runCtx the one --timeout bounds.
func downloadExport(ctx, runCtx context.Context, env Env, sess *session, rep *output.ExportReporter, name string, id int64, df *downloadFlags) int {
	dir := filepath.Join(df.output, fmt.Sprintf("optimole-export-%d", id))
	out := export.Run(runCtx, export.Options{
		Client:      sess.client,
		ExportID:    id,
		Dir:         dir,
		Domains:     df.domains,
		Concurrency: df.concurrency,
		Sleep:       env.Sleep,
		OnEvent:     rep.Event,
	})
	var unknown *export.UnknownDomainError
	if errors.As(out.Err, &unknown) {
		return usageError(env, name, "--domain: %v", unknown)
	}
	interrupted := ctx.Err() != nil
	timedOut := !interrupted && errors.Is(out.Err, context.DeadlineExceeded)
	if out.Export != nil {
		rep.Finish(out, dir, interrupted, timedOut)
	}
	if interrupted || timedOut {
		resume := fmt.Sprintf("optimole export download %d", id)
		if df.output != "." {
			resume += " -o " + shellQuote(df.output)
		}
		if out.Export != nil && api.IsFinalStatus(out.Export.Status) {
			fmt.Fprintf(env.Stderr, "Resume with: %s\n", resume)
		} else {
			fmt.Fprintf(env.Stderr, "Export %d continues on the server. Resume with: %s\n", id, resume)
		}
	}
	switch {
	case interrupted:
		return ExitInterrupted
	case timedOut:
		fmt.Fprintf(env.Stderr, "optimole %s: timed out after %s\n", name, df.timeout)
		return ExitFailure
	case out.Err != nil:
		return exportError(env, sess, name, out.Err)
	case rep.Err() != nil:
		return outputFailed(env, rep.Err())
	case out.Failed > 0 || out.Export.Status != api.ExportCompleted:
		return ExitFailure
	}
	return ExitOK
}

func runExportStatus(ctx context.Context, args []string, env Env) int {
	const name = "export status"
	fs := newFlagSet(name, exportStatusUsage, env)
	var common commonFlags
	common.register(fs, true)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 1 {
		return usageError(env, name, "takes at most one export ID")
	}
	ref := "latest"
	if len(rest) == 1 {
		ref = rest[0]
	}
	if _, err := parseExportRef(ref); err != nil {
		return usageError(env, name, "%v", err)
	}
	sess, err := common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	id, code := resolveExport(ctx, env, sess, name, ref)
	if code != ExitOK {
		return code
	}
	exp, err := sess.client.GetExport(ctx, id)
	if err != nil {
		return exportError(env, sess, name, err)
	}

	out := output.NewErrWriter(env.Stdout)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		enc.Encode(statusJSON(exp))
	} else {
		fmt.Fprintf(out, "Export %d: %s, started %s\n", exp.ID, exp.Status, displayTime(exp.CreatedAt))
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "DOMAIN\tSTATUS\tARCHIVES\tSIZE")
		for _, d := range exp.Domains {
			var total int64
			for _, p := range d.Parts {
				total += p.Size.Value
			}
			size := "-"
			if len(d.Parts) > 0 {
				size = upload.HumanSize(total)
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", d.Domain, d.Status, len(d.Parts), size)
		}
		tw.Flush()
	}
	if err := out.Err(); err != nil {
		return outputFailed(env, err)
	}
	return ExitOK
}

// exportStatusLine is the JSON of "export status": the export without URLs.
type exportStatusLine struct {
	ID        int64              `json:"id"`
	Status    string             `json:"status"`
	CreatedAt string             `json:"created_at"`
	Domains   []domainStatusLine `json:"domains"`
}

type domainStatusLine struct {
	Domain   string              `json:"domain"`
	Status   string              `json:"status"`
	Archives []archiveStatusLine `json:"archives"`
}

type archiveStatusLine struct {
	PartNumber int64 `json:"part_number"`
	Size       int64 `json:"size"`
}

func statusJSON(exp *api.Export) exportStatusLine {
	l := exportStatusLine{ID: exp.ID, Status: exp.Status, CreatedAt: exp.CreatedAt, Domains: []domainStatusLine{}}
	for _, d := range exp.Domains {
		dl := domainStatusLine{Domain: d.Domain, Status: d.Status, Archives: []archiveStatusLine{}}
		for _, p := range d.Parts {
			dl.Archives = append(dl.Archives, archiveStatusLine{PartNumber: p.Number.Value, Size: p.Size.Value})
		}
		l.Domains = append(l.Domains, dl)
	}
	return l
}

func runExportList(ctx context.Context, args []string, env Env) int {
	const name = "export list"
	fs := newFlagSet(name, exportListUsage, env)
	var common commonFlags
	common.register(fs, true)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, name, "takes no arguments")
	}
	sess, err := common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	exports, err := listAllExports(ctx, sess.client)
	if err != nil {
		return exportError(env, sess, name, err)
	}

	out := output.NewErrWriter(env.Stdout)
	switch {
	case jsonOut:
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		for _, e := range exports {
			names := make([]string, 0, len(e.Domains))
			for _, d := range e.Domains {
				names = append(names, d.Domain)
			}
			enc.Encode(struct {
				ID        int64    `json:"id"`
				Status    string   `json:"status"`
				CreatedAt string   `json:"created_at"`
				Domains   []string `json:"domains"`
			}{e.ID, e.Status, e.CreatedAt, names})
		}
	case len(exports) == 0:
		fmt.Fprintln(out, `No exports. Start one with "optimole export start".`)
	default:
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tCREATED\tDOMAINS")
		for _, e := range exports {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%d\n", e.ID, e.Status, displayTime(e.CreatedAt), len(e.Domains))
		}
		tw.Flush()
	}
	if err := out.Err(); err != nil {
		return outputFailed(env, err)
	}
	return ExitOK
}

func runExportDelete(ctx context.Context, args []string, env Env) int {
	const name = "export delete"
	fs := newFlagSet(name, exportDeleteUsage, env)
	var common commonFlags
	common.register(fs, true)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) != 1 {
		return usageError(env, name, "takes one export ID")
	}
	id, err := parseExportRef(rest[0])
	if err != nil || id == 0 {
		return usageError(env, name, "%q is not an export ID", rest[0])
	}
	sess, err := common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	if err := sess.client.DeleteExport(ctx, id); err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			fmt.Fprintf(env.Stderr, "optimole %s: %s\nOnly finished exports can be deleted; this one is still queued or running.\n", name, sess.redact.Error(err))
			return ExitFailure
		}
		return exportError(env, sess, name, err)
	}
	out := output.NewErrWriter(env.Stdout)
	fmt.Fprintf(out, "Deleted export %d.\n", id)
	if err := out.Err(); err != nil {
		return outputFailed(env, err)
	}
	return ExitOK
}

// parseExportRef reads an export ID; "latest" gives 0.
func parseExportRef(ref string) (int64, error) {
	if strings.EqualFold(ref, "latest") {
		return 0, nil
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not an export ID (or \"latest\")", ref)
	}
	return id, nil
}

// resolveExport turns an export reference into an ID, looking up the latest
// export when asked.
func resolveExport(ctx context.Context, env Env, sess *session, name, ref string) (int64, int) {
	id, _ := parseExportRef(ref)
	if id > 0 {
		return id, ExitOK
	}
	page, err := sess.client.ListExports(ctx, api.ExportQuery{PerPage: 1})
	if err != nil {
		return 0, exportError(env, sess, name, err)
	}
	if len(page.Data) == 0 {
		fmt.Fprintf(env.Stderr, "optimole %s: there are no exports yet; start one with \"optimole export start\"\n", name)
		return 0, ExitFailure
	}
	return page.Data[0].ID, ExitOK
}

// listAllExports returns every export of the account, newest first.
func listAllExports(ctx context.Context, client *api.Client) ([]api.Export, error) {
	var all []api.Export
	q := api.ExportQuery{PerPage: exportsPerPage}
	seen := map[string]bool{}
	for page := 0; page < maxExportPages; page++ {
		res, err := client.ListExports(ctx, q)
		if err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		next := ""
		if res.Meta.NextCursor != nil {
			next = *res.Meta.NextCursor
		}
		if !res.Meta.HasMore || next == "" || seen[next] {
			break
		}
		seen[next] = true
		q.Cursor = next
	}
	return all, nil
}

// exportError prints a failed export call and returns its exit code.
func exportError(env Env, sess *session, name string, err error) int {
	msg := sess.redact.Error(err)
	var apiErr *api.Error
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(env.Stderr, "optimole %s: interrupted\n", name)
		return ExitInterrupted
	case isRejectedKey(err):
		fmt.Fprintf(env.Stderr, "optimole %s: %s\nThe API key (from %s) was rejected.\n", name, msg, sess.settings.KeySource)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && isScopedKey(sess.settings.APIKey):
		fmt.Fprintf(env.Stderr, "optimole %s: %s\nExports need a full-access API key; folder-scoped keys cannot export.\n", name, msg)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound && apiErr.Op == api.OpGetExport:
		fmt.Fprintf(env.Stderr, "optimole %s: %s\nNo such export: finished exports are deleted 10 days after they finish (\"optimole export list\" shows the others).\n", name, msg)
	default:
		fmt.Fprintf(env.Stderr, "optimole %s: %s\n", name, msg)
	}
	return ExitFailure
}

// withTimeout bounds ctx by d when d is set.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// displayTime shows an API timestamp in UTC to the minute, or as given when
// it does not parse.
func displayTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		if ts == "" {
			return "-"
		}
		return ts
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
