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
	"sort"
	"strconv"
	"strings"
	"unicode"

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
  --site DOMAIN    Every image of this website of the account, in any folder.

Folders and websites are kept. A folder or a website is emptied in the
background: the command returns at once with how many images go, and they
leave the library within a few minutes.

First the command says what it will delete and asks you to type the folder's
path, the website's domain, or "delete" for keys. --yes skips the question.
Without a terminal to ask on (in scripts, or with --keys -), it deletes only
with --yes. Deleting needs a full-access API key.

Flags:
  --yes    Delete without asking
  --json   Print the result as one JSON object
` + commonUsage + `
Exit codes: 0 deleted (a folder or a website: being deleted), 1 the folder
was not found, the API refused or failed, or the typed answer did not match,
2 usage or configuration error (such as no --yes without a terminal), 130
interrupted.
`

// deleteTarget is what "optimole delete" deletes: keys, a folder or a website.
type deleteTarget struct {
	keys     []string // lower case, each once
	keysFrom string   // the --keys file, "-" for stdin
	folderID int64
	folder   string // the folder's path
	website  bool   // the folder is a website's
	domain   string
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
	var folderPath, folderIDRaw, site string
	var yes, jsonOut bool
	fs.StringVar(&t.keysFrom, "keys", "", "")
	fs.StringVar(&folderPath, "folder", "", "")
	fs.StringVar(&folderIDRaw, "folder-id", "", "")
	fs.StringVar(&site, "site", "", "")
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
			return usageError(env, name, "--folder: %s", red.String(err.Error()))
		}
		if folderPath == "" {
			return usageError(env, name, "--folder must name a folder; the library root is not one")
		}
	case set["folder-id"]:
		if t.folderID, err = parseFolderID(folderIDRaw); err != nil {
			return usageError(env, name, "--folder-id: %s", red.String(err.Error()))
		}
	default:
		if t.domain = siteDomain(site); t.domain == "" {
			return usageError(env, name, "--site: %s is not a domain", strconv.Quote(red.String(site)))
		}
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
		if code := confirmDelete(ctx, env, &t); code != ExitOK {
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
	var r io.Reader = env.Stdin
	src := "stdin"
	if t.keysFrom != "-" {
		f, err := os.Open(t.keysFrom)
		if err != nil {
			return usageError(env, name, "--keys: %s", red.Error(err))
		}
		defer f.Close()
		r, src = f, shownText(red.String(t.keysFrom))
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
		fmt.Fprintf(env.Stderr, "optimole %s: interrupted; nothing was deleted\n", name)
		return ExitInterrupted
	}
	if err := readErr(); err != nil {
		return usageError(env, name, "--keys: %s", red.Error(err))
	}
	if len(t.keys) == 0 {
		return usageError(env, name, "--keys: no asset keys in %s", src)
	}
	return ExitOK
}

// findFolder looks the folder up, by ID or by path, in the folder list: for
// its path, shown and typed to confirm, and to refuse a folder that is not
// there, or a path two folders share, before anything is deleted.
func findFolder(ctx context.Context, env Env, sess *session, t *deleteTarget, path string) int {
	folders, err := listAllFolders(ctx, sess.client, "")
	if err != nil {
		return deleteError(env, sess, err)
	}
	paths := folderPaths(folders)
	found := map[int64]api.Folder{}
	for _, f := range folders {
		if (t.folderID > 0 && f.ID == t.folderID) || (path != "" && strings.EqualFold(paths[f.ID], path)) {
			found[f.ID] = f
		}
	}
	if len(found) == 0 {
		what := fmt.Sprintf("the ID %d", t.folderID)
		if path != "" {
			what = "the path " + strconv.Quote(path)
		}
		fmt.Fprintf(env.Stderr, "optimole delete: no folder has %s (\"optimole folders\" lists them)\n", what)
		return ExitFailure
	}
	if len(found) > 1 {
		ids := make([]string, 0, len(found))
		for id := range found {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
		sort.Strings(ids)
		fmt.Fprintf(env.Stderr, "optimole delete: %d folders have the path %q (IDs %s); name one with --folder-id\n", len(found), path, strings.Join(ids, ", "))
		return ExitFailure
	}
	for id, f := range found {
		t.folderID, t.folder, t.website = id, paths[id], f.IsDefault
	}
	return ExitOK
}

// confirmDelete says what is about to go and asks for the folder's path, the
// website's domain or "delete" to be typed; any other answer aborts.
func confirmDelete(ctx context.Context, env Env, t *deleteTarget) int {
	var what, answer string
	switch {
	case t.keys != nil:
		what = fmt.Sprintf("%s (the keys in %s)", images(int64(len(t.keys))), shownText(t.keysFrom))
		answer = "delete"
	case t.domain != "":
		what = fmt.Sprintf("every image of the website %s, in any folder", t.domain)
		answer = t.domain
	default:
		what = fmt.Sprintf("every image in the folder %q (ID %d) and its subfolders", t.folder, t.folderID)
		if t.website {
			what += fmt.Sprintf(", and the images of the website %s that are in no folder", shownText(t.folder))
		}
		what += "; the folders are kept"
		answer = t.folder
	}
	fmt.Fprintf(env.Stderr, "About to delete %s. This cannot be undone: the CDN stops serving the images.\nType %q to confirm: ", what, answer)
	line, err := readOneLine(ctx, env.Stdin, false, "the answer")
	if !strings.HasSuffix(line, "\n") {
		// Ctrl-C or Ctrl-D left the cursor after the prompt.
		fmt.Fprintln(env.Stderr)
	}
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "optimole delete: interrupted; nothing was deleted")
		return ExitInterrupted
	case err != nil:
		fmt.Fprintf(env.Stderr, "optimole delete: %v; nothing was deleted\n", err)
		return ExitFailure
	case strings.TrimSpace(line) != answer:
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
			if !refusedBeforeDeleting(err) {
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
	switch {
	case maybe > 0:
		msg := fmt.Sprintf("Deleted %d of %d images before it stopped; the next %d may be deleted too (their request failed)", deleted, total, maybe)
		if other := total - deleted - maybe; other > 0 {
			msg += fmt.Sprintf(", the other %d were not", other)
		}
		fmt.Fprintln(env.Stderr, msg+".")
	case deleted > 0:
		fmt.Fprintf(env.Stderr, "Deleted %d of %d images before it stopped; the other %d were not deleted.\n", deleted, total, total-deleted)
	default:
		fmt.Fprintln(env.Stderr, "Nothing was deleted.")
	}
	if deleted > 0 || maybe > 0 {
		fmt.Fprintln(env.Stderr, "Running the command again with the same keys is safe: deleted images can be named again.")
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
		n, err = sess.client.DeleteSiteAssets(ctx, t.domain)
		line = backgroundLine{Scope: "site", Domain: t.domain}
		where = "the website " + t.domain
	} else {
		n, err = sess.client.DeleteFolderAssets(ctx, t.folderID)
		line = backgroundLine{Scope: "folder", FolderID: t.folderID, Folder: t.folder}
		where = "the folder " + strconv.Quote(t.folder)
	}
	if err != nil {
		code := deleteError(env, sess, err)
		if !refusedBeforeDeleting(err) {
			// Not repeated (see api.DeleteFolderAssets), so it is unknown.
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

// refusedBeforeDeleting reports an answer that proves the request deleted
// nothing: the dashboard checks the key, the scope and the body (401, 403,
// 422, 429) before it deletes anything. After any other failure (no answer,
// a 5xx, Ctrl-C mid-request) it may have.
func refusedBeforeDeleting(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode < http.StatusInternalServerError
}

// deleteError prints a failed delete call and returns its exit code.
func deleteError(env Env, sess *session, err error) int {
	msg := sess.redact.Error(err)
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

// siteDomain reads a website's domain the way the dashboard does: the host of
// a URL or of a bare domain, in lower case and without "www.". It returns ""
// when there is none.
func siteDomain(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	} else if !hasHTTPScheme(s) {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// images says "1 image" or "N images".
func images(n int64) string {
	if n == 1 {
		return "1 image"
	}
	return fmt.Sprintf("%d images", n)
}

// shownText quotes s when it holds control characters, which a terminal
// would otherwise act on.
func shownText(s string) string {
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}
