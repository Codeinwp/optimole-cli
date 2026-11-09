package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/output"
	"github.com/Codeinwp/optimole-cli/internal/redact"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

const deleteUsage = `Usage:
  optimole delete --keys FILE|- [flags]
  optimole delete --folder PATH|--folder-id N [flags]
  optimole delete --site DOMAIN [flags]

Deletes images from the media library for good: the CDN stops serving them,
so pages that still use them show broken images. Name the images with exactly
one of:

  --keys FILE|-    The asset keys in FILE (- reads stdin), one per line; blank
                   lines are ignored. They go 100 per request; a request that
                   fails stops the run, and the images before it stay deleted.
  --folder PATH    Every image in this folder and its subfolders, e.g.
                   "Campaigns/2026" (see "optimole folders"). For a website's
                   folder, also the website's images that are in no folder.
  --folder-id N    The same, naming the folder by its ID.
  --site DOMAIN    Every image of this website of the account, in any folder
                   (an internationalized domain in its xn-- form).

Folders and websites are kept. A folder or a website is emptied in the
background: the command returns at once with how many images go, and they
leave the library within a few minutes.

First the command says what it will delete and asks you to type the folder's
path (its name, for a path too long to type), the website's domain, or
"delete" for keys, then Enter. --yes skips the question. Without a terminal to
ask on (in scripts, or with --keys -), it deletes only with --yes. Deleting
needs a full-access API key.

Flags:
  --yes    Delete without asking
  --json   Print the result as one JSON object
` + commonUsage + `
Exit codes: 0 deleted (a folder or a website: being deleted), 1 the folder
was not found, the API refused or failed, or the typed answer did not match,
2 usage or configuration error (such as no --yes without a terminal), 130
interrupted.
`

// maxAnswer is the longest answer the question asks to type: a terminal line
// holds 1024 bytes on macOS. A longer folder path is confirmed by its name.
const maxAnswer = 1000

// deleteTarget is what "optimole delete" deletes: keys, a folder or a website.
type deleteTarget struct {
	keys     []string // lower case, each once
	keysFrom string   // the --keys file, "-" for stdin
	folderID int64
	folder   string // the folder's path
	website  bool   // the folder is a website's
	// site is the --site value, then its host in lower case: what is sent.
	// The server strips one "www." from it, into domain, as the prompt does.
	// Sending domain would have the server strip a second "www.", and sending
	// the whole URL could pass its 255-character limit.
	site   string
	domain string
}

// keysLine is the JSON of a deletion by keys.
type keysLine struct {
	Scope     string `json:"scope"`
	Requested int    `json:"requested"`
	Deleted   int    `json:"deleted"`
	// MaybeDeleted counts the keys of a failed request whose outcome is
	// unknown: they may be deleted too.
	MaybeDeleted int    `json:"maybe_deleted,omitempty"`
	Error        string `json:"error,omitempty"`
}

// backgroundLine is the JSON of a folder or a website being emptied.
type backgroundLine struct {
	Scope    string `json:"scope"`
	FolderID int64  `json:"folder_id,omitempty"`
	Folder   string `json:"folder,omitempty"`
	Domain   string `json:"domain,omitempty"`
	// Deleting is how many images go; absent when the server did not say.
	Deleting *int64 `json:"deleting,omitempty"`
}

func runDelete(ctx context.Context, args []string, env Env) int {
	const name = "delete"
	fs := newFlagSet(name, deleteUsage, env)
	var common commonFlags
	common.register(fs, true)
	fs.SetOutput(flagErrorWriter{w: env.Stderr})
	var t deleteTarget
	var folderPath, folderIDRaw string
	var yes, jsonOut bool
	fs.StringVar(&t.keysFrom, "keys", "", "")
	fs.StringVar(&folderPath, "folder", "", "")
	fs.StringVar(&folderIDRaw, "folder-id", "", "")
	fs.StringVar(&t.site, "site", "", "")
	fs.BoolVar(&yes, "yes", false, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, name, "takes no arguments; name the images with --keys, --folder, --folder-id or --site")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	named := 0
	for _, f := range []string{"keys", "folder", "folder-id", "site"} {
		if set[f] {
			named++
		}
	}
	if named != 1 {
		return usageError(env, name, "name the images to delete with exactly one of --keys, --folder, --folder-id and --site")
	}
	// Flag values are echoed in usage errors: a key given by mistake must
	// not be.
	red := common.resolve(env).redact
	switch {
	case set["keys"]:
		if strings.TrimSpace(t.keysFrom) == "" {
			return usageError(env, name, "--keys needs a file, or - to read stdin")
		}
		if t.keysFrom == "-" && env.interactive() {
			return usageError(env, name, "--keys - reads the keys from stdin: pipe them in, or name a file")
		}
	case set["folder"]:
		if folderPath, err = upload.NormalizeFolderPath(folderPath); err != nil {
			return usageError(env, name, "--folder: %s", shown(red, err.Error()))
		}
		if folderPath == "" {
			return usageError(env, name, "--folder must name a folder; the library root is not one")
		}
	case set["folder-id"]:
		if t.folderID, err = parseFolderID(folderIDRaw); err != nil {
			return usageError(env, name, "--folder-id: %s", shown(red, err.Error()))
		}
	default:
		var host string
		if host, err = siteHost(t.site); err != nil {
			return usageError(env, name, "--site: %s %v", strconv.Quote(red.String(t.site)), err)
		}
		t.site, t.domain = host, strings.TrimPrefix(host, "www.")
	}
	if !yes && !env.interactive() {
		return usageError(env, name, "stdin is not a terminal, so the deletion cannot be confirmed; add --yes to delete without asking")
	}
	if set["keys"] {
		if code := readDeleteKeys(ctx, env, red, &t); code != ExitOK {
			return code
		}
	}

	sess, err := common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole %s: %v\n", name, err)
		return ExitUsage
	}
	if set["folder"] || set["folder-id"] {
		if code := findFolder(ctx, env, sess, &t, folderPath); code != ExitOK {
			return code
		}
	}
	if !yes {
		if code := confirmDelete(ctx, env, sess.redact, &t); code != ExitOK {
			return code
		}
	}
	out := output.NewErrWriter(env.Stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	var code int
	if t.keys != nil {
		code = deleteKeys(ctx, env, sess, out, enc, &t, jsonOut)
	} else {
		code = deleteInBackground(ctx, env, sess, out, enc, &t, jsonOut)
	}
	if err := out.Err(); err != nil && code == ExitOK {
		return outputFailed(env, err)
	}
	return code
}

// readDeleteKeys reads the --keys input: one asset key per line, blank lines
// and surrounding spaces ignored. The keys are kept in lower case, each once.
// red hides the known API keys in errors, which name the file.
func readDeleteKeys(ctx context.Context, env Env, red redact.Redactor, t *deleteTarget) int {
	const name = "delete"
	interrupted := func() int {
		fmt.Fprintf(env.Stderr, "optimole %s: interrupted; nothing was deleted\n", name)
		return ExitInterrupted
	}
	var r io.Reader = env.Stdin
	src := "stdin"
	if t.keysFrom != "-" {
		f, err := openContext(ctx, t.keysFrom)
		if ctx.Err() != nil {
			return interrupted()
		}
		if err != nil {
			return usageError(env, name, "--keys: %s", shown(red, err.Error()))
		}
		defer f.Close()
		r, src = f, shown(red, t.keysFrom)
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines, readErr := readLines(readCtx, r, src)
	seen := map[string]bool{}
	for n := 1; ; n++ {
		// A read cannot be interrupted: Ctrl-C leaves it behind.
		var line string
		var more bool
		select {
		case line, more = <-lines:
		case <-ctx.Done():
		}
		if !more {
			break
		}
		k := strings.TrimSpace(line)
		if k == "" {
			continue
		}
		// The line is not echoed: it could be anything, an API key too.
		if !api.IsAssetKey(k) {
			return usageError(env, name, "--keys: line %d of %s is not an asset key (32 hexadecimal characters)", n, src)
		}
		k = strings.ToLower(k)
		if !seen[k] {
			seen[k] = true
			t.keys = append(t.keys, k)
		}
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	if err := readErr(); err != nil {
		return usageError(env, name, "--keys: %s", shown(red, err.Error()))
	}
	if len(t.keys) == 0 {
		return usageError(env, name, "--keys: no asset keys in %s", src)
	}
	return ExitOK
}

// openContext opens a file for reading and gives up when ctx is done: opening
// a FIFO waits for a writer. The open left behind ends with the process.
func openContext(ctx context.Context, name string) (*os.File, error) {
	type opened struct {
		f   *os.File
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		f, err := os.Open(name)
		ch <- opened{f, err}
	}()
	select {
	case o := <-ch:
		return o.f, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// findFolder looks the folder up, by ID or by path, in the folder list: for
// its path, shown and typed to confirm, and to refuse a folder that is not
// there, or a path two folders share, before anything is deleted. A path is
// matched only against a whole list, so a duplicate cannot hide in the part
// that did not come back.
func findFolder(ctx context.Context, env Env, sess *session, t *deleteTarget, want string) int {
	folders, complete, err := listAllFolders(ctx, sess.client, "")
	if err != nil {
		return deleteError(env, sess, err)
	}
	paths := folderPaths(folders)
	found := map[int64]api.Folder{}
	for _, f := range folders {
		if (t.folderID > 0 && f.ID == t.folderID) || (want != "" && samePath(paths[f.ID], want)) {
			found[f.ID] = f
		}
	}
	what := fmt.Sprintf("the ID %d", t.folderID)
	if want != "" {
		what = "the path " + strconv.Quote(sess.redact.String(want))
	}
	switch {
	case !complete && (want != "" || len(found) == 0):
		fmt.Fprintf(env.Stderr, "optimole delete: the folder list came back incomplete (%d folders), so the folder with %s cannot be told apart for sure; nothing was deleted\n", len(folders), what)
		return ExitFailure
	case len(found) == 0:
		fmt.Fprintf(env.Stderr, "optimole delete: no folder has %s (\"optimole folders\" lists them)\n", what)
		return ExitFailure
	case len(found) > 1:
		ids := make([]string, 0, len(found))
		for id := range found {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		sort.Strings(ids)
		fmt.Fprintf(env.Stderr, "optimole delete: %d folders have %s (IDs %s); name one with --folder-id\n", len(found), what, strings.Join(ids, ", "))
		return ExitFailure
	}
	for id, f := range found {
		t.folderID, t.folder, t.website = id, paths[id], f.IsDefault
	}
	return ExitOK
}

// confirmDelete says what is about to go and asks for the folder's path, the
// website's domain or "delete" to be typed, then Enter; any other answer, or
// none, aborts. red hides the known API keys in what is shown: an answer that
// holds one cannot be typed from the prompt, which leaves --yes.
func confirmDelete(ctx context.Context, env Env, red redact.Redactor, t *deleteTarget) int {
	var what, answer string
	switch {
	case t.keys != nil:
		what = fmt.Sprintf("%s (the keys in %s)", images(int64(len(t.keys))), shown(red, t.keysFrom))
		answer = "delete"
	case t.domain != "":
		what = fmt.Sprintf("every image of the website %s, in any folder", shown(red, t.domain))
		answer = t.domain
	default:
		what = fmt.Sprintf("every image in the folder %q (ID %d) and its subfolders", red.String(t.folder), t.folderID)
		if t.website {
			what += fmt.Sprintf(", and the images of the website %s that are in no folder", shown(red, t.folder))
		}
		what += "; the folders are kept"
		answer = t.folder
		if len(answer) > maxAnswer {
			answer = path.Base(t.folder)
		}
	}
	fmt.Fprintf(env.Stderr, "About to delete %s. This cannot be undone: the CDN stops serving the images.\nType %q to confirm: ", what, red.String(answer))
	line, err := readOneLine(ctx, env.Stdin, false, "the answer")
	complete := strings.HasSuffix(line, "\n")
	if !complete {
		// Ctrl-C or Ctrl-D left the cursor after the prompt.
		fmt.Fprintln(env.Stderr)
	}
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "optimole delete: interrupted; nothing was deleted")
		return ExitInterrupted
	case err != nil:
		fmt.Fprintf(env.Stderr, "optimole delete: %s; nothing was deleted\n", shown(red, err.Error()))
		return ExitFailure
	case !complete || strings.TrimSpace(line) != answer:
		// An answer that ends without Enter (Ctrl-D, or a line too long to
		// be read whole) is no answer.
		fmt.Fprintln(env.Stderr, "optimole delete: the answer did not match; nothing was deleted")
		return ExitFailure
	}
	return ExitOK
}

// deleteKeys deletes t.keys, api.MaxDeleteKeys per request, and stops at the
// first request that fails.
func deleteKeys(ctx context.Context, env Env, sess *session, out io.Writer, enc *json.Encoder, t *deleteTarget, jsonOut bool) int {
	total, deleted, maybe := len(t.keys), 0, 0
	var err error
	for deleted < total {
		if err = ctx.Err(); err != nil {
			// Stopped between requests: the next one was never sent.
			break
		}
		end := min(deleted+api.MaxDeleteKeys, total)
		if err = sess.client.DeleteAssets(ctx, t.keys[deleted:end]); err != nil {
			if !untouched(err) {
				maybe = end - deleted
			}
			break
		}
		deleted = end
	}
	if jsonOut {
		enc.Encode(keysLine{Scope: "keys", Requested: total, Deleted: deleted, MaybeDeleted: maybe, Error: sess.redact.Error(err)})
	}
	if err == nil {
		if !jsonOut {
			fmt.Fprintf(out, "Deleted %s.\n", images(int64(deleted)))
		}
		return ExitOK
	}
	code := deleteError(env, sess, err)
	other := total - deleted - maybe
	switch {
	case maybe > 0:
		msg := fmt.Sprintf("Deleted %d of %s before it stopped; %d more may be deleted too (the request failed)", deleted, images(int64(total)), maybe)
		if deleted == 0 {
			msg = fmt.Sprintf("%d of %s may be deleted (the request failed)", maybe, images(int64(total)))
		}
		if other > 0 {
			msg += fmt.Sprintf(", the other %d were not", other)
		}
		fmt.Fprintln(env.Stderr, msg+".")
	case deleted > 0:
		fmt.Fprintf(env.Stderr, "Deleted %d of %s before it stopped; the other %d were not deleted.\n", deleted, images(int64(total)), other)
	default:
		fmt.Fprintln(env.Stderr, "Nothing was deleted.")
	}
	// A deleted key counts as invalid if named again, and refuses its whole
	// request: say which to leave out.
	if deleted > 0 {
		fmt.Fprintf(env.Stderr, "To finish, run the command again without the first %d keys: a deleted key counts as invalid if named again.\n", deleted)
	}
	if maybe > 0 {
		fmt.Fprintln(env.Stderr, "If the next run lists keys as invalid, the request that failed deleted them; leave them out.")
	}
	return code
}

// deleteInBackground asks the server to empty the folder or the website.
func deleteInBackground(ctx context.Context, env Env, sess *session, out io.Writer, enc *json.Encoder, t *deleteTarget, jsonOut bool) int {
	var n api.FlexInt
	var err error
	var line backgroundLine
	var where string
	if t.domain != "" {
		n, err = sess.client.DeleteSiteAssets(ctx, t.site)
		line = backgroundLine{Scope: "site", Domain: sess.redact.String(t.domain)}
		where = "the website " + shown(sess.redact, t.domain)
	} else {
		n, err = sess.client.DeleteFolderAssets(ctx, t.folderID)
		line = backgroundLine{Scope: "folder", FolderID: t.folderID, Folder: sess.redact.String(t.folder)}
		where = "the folder " + strconv.Quote(sess.redact.String(t.folder))
	}
	if err != nil {
		code := deleteError(env, sess, err)
		if !untouched(err) {
			fmt.Fprintln(env.Stderr, "The deletion may have started anyway: check the library in a few minutes before running the command again.")
		}
		return code
	}
	if jsonOut {
		if n.Valid {
			line.Deleting = &n.Value
		}
		enc.Encode(line)
		return ExitOK
	}
	switch {
	case !n.Valid:
		fmt.Fprintln(out, "Deleting the images in the background. They leave the library within a few minutes.")
	case n.Value == 0:
		fmt.Fprintf(out, "Nothing to delete: %s has no images.\n", where)
	case n.Value == 1:
		fmt.Fprintln(out, "Deleting 1 image in the background. It leaves the library within a few minutes.")
	default:
		fmt.Fprintf(out, "Deleting %d images in the background. They leave the library within a few minutes.\n", n.Value)
	}
	return ExitOK
}

// untouched reports a failed delete request that proves nothing was deleted:
// a 4xx (the dashboard checks the key, the scope and the body before it
// deletes anything), or no connection at all. Delete requests are repeated
// only after such failures (see api.DeleteAssets), so earlier attempts did
// nothing either. After a lost answer, a 5xx, a 2xx that does not read or
// Ctrl-C mid-request, the request may have deleted.
func untouched(err error) bool {
	var apiErr *api.Error
	var netErr *api.NetworkError
	switch {
	case errors.As(err, &apiErr):
		return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500
	case errors.As(err, &netErr):
		return !netErr.MaybeCommitted
	}
	return false
}

// deleteError prints a failed delete call and returns its exit code.
func deleteError(env Env, sess *session, err error) int {
	msg := shown(sess.redact, err.Error())
	var apiErr *api.Error
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "optimole delete: interrupted")
		return ExitInterrupted
	case isRejectedKey(err):
		fmt.Fprintf(env.Stderr, "optimole delete: %s\nThe API key (from %s) was rejected.\n", msg, sess.settings.KeySource)
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && isScopedKey(sess.settings.APIKey):
		fmt.Fprintf(env.Stderr, "optimole delete: %s\nDeleting images needs a full-access API key; folder-scoped keys cannot delete.\n", msg)
	default:
		fmt.Fprintf(env.Stderr, "optimole delete: %s\n", msg)
	}
	return ExitFailure
}

// siteHost reads the host of a URL or of a bare domain, in lower case, the
// way the dashboard does before it strips one "www.". Only ASCII hosts are
// taken: the dashboard maps the others with IDNA, which the standard library
// lacks, so the domain shown could differ from the one deleted.
func siteHost(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	} else if !hasHTTPScheme(s) {
		return "", errors.New("is not a domain")
	}
	u, err := url.Parse(s)
	if err != nil || strings.TrimPrefix(u.Hostname(), "www.") == "" {
		return "", errors.New("is not a domain")
	}
	host := u.Hostname()
	if strings.ContainsFunc(host, func(r rune) bool { return r >= utf8.RuneSelf }) {
		return "", errors.New("is not in ASCII; give the domain in its xn-- form")
	}
	return strings.ToLower(host), nil
}

// samePath reports whether a listed folder path reads as want, a normalized
// path: a folder whose name holds a slash ("2026 / Drafts") reads the same as
// the nested folders it looks like, and counts as a match, so the two are
// told apart by ID.
func samePath(listed, want string) bool {
	if n, err := upload.NormalizeFolderPath(listed); err == nil {
		listed = n
	}
	return strings.EqualFold(listed, want)
}

// images says "1 image" or "N images".
func images(n int64) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}

// shown readies text for the terminal: the known API keys hidden, and quoted
// when it holds control characters, which a terminal would otherwise act on.
func shown(red redact.Redactor, s string) string {
	s = red.String(s)
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}
