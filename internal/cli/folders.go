package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/output"
)

const foldersUsage = `Usage:
  optimole folders [--parent root|ID] [--json]

Lists the library's active folders. Without --parent it lists every folder
with its full path (what --folder expects); with --parent it lists the direct
children of that folder ("root" for top-level folders).

Folder-scoped API keys cannot list folders; "optimole upload --folder PATH"
still works with them for the folders they were granted.

Flags:
  --parent root|ID   Only list the children of this folder
  --json             Print one JSON object per folder
` + commonUsage

const (
	foldersPerPage = 100
	maxFolderPages = 1000
)

var parentPattern = regexp.MustCompile(`^(root|[1-9][0-9]*)$`)

type folderLine struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	ParentID *int64 `json:"parent_id"`
	Path     string `json:"path,omitempty"`
}

func runFolders(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("folders", foldersUsage, env)
	var common commonFlags
	common.register(fs, true)
	var parent string
	var jsonOut bool
	fs.StringVar(&parent, "parent", "", "")
	fs.BoolVar(&jsonOut, "json", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, "folders", "takes no arguments")
	}
	parent = strings.ToLower(strings.TrimSpace(parent))
	if parent != "" && !parentPattern.MatchString(parent) {
		return usageError(env, "folders", "--parent must be \"root\" or a folder ID")
	}
	sess, err := common.connect(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole folders: %v\n", err)
		return ExitUsage
	}

	var folders []api.Folder
	q := api.FolderQuery{ParentID: parent, PerPage: foldersPerPage}
	seen := map[string]bool{}
	for page := 0; page < maxFolderPages; page++ {
		res, err := sess.client.ListFolders(ctx, q)
		if err != nil {
			return foldersError(env, sess, err)
		}
		folders = append(folders, res.Data...)
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

	lines := make([]folderLine, 0, len(folders))
	paths := map[int64]string{}
	if parent == "" {
		paths = folderPaths(folders)
	}
	for _, f := range folders {
		l := folderLine{ID: f.ID, Name: f.Name, Path: paths[f.ID]}
		if f.ParentID.Valid {
			id := f.ParentID.Value
			l.ParentID = &id
		}
		lines = append(lines, l)
	}
	sort.SliceStable(lines, func(i, j int) bool {
		a, b := lines[i].Path, lines[j].Path
		if a == "" && b == "" {
			a, b = lines[i].Name, lines[j].Name
		}
		return strings.ToLower(a) < strings.ToLower(b)
	})

	out := output.NewErrWriter(env.Stdout)
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetEscapeHTML(false)
		for _, l := range lines {
			enc.Encode(l)
		}
	} else if len(lines) == 0 {
		fmt.Fprintln(out, "No folders.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if parent == "" {
			fmt.Fprintln(tw, "ID\tPATH")
			for _, l := range lines {
				fmt.Fprintf(tw, "%d\t%s\n", l.ID, l.Path)
			}
		} else {
			fmt.Fprintln(tw, "ID\tNAME")
			for _, l := range lines {
				fmt.Fprintf(tw, "%d\t%s\n", l.ID, l.Name)
			}
		}
		tw.Flush()
	}
	if err := out.Err(); err != nil {
		return outputFailed(env, err)
	}
	return ExitOK
}

func foldersError(env Env, sess *session, err error) int {
	msg := sess.redact.Error(err)
	var apiErr *api.Error
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(env.Stderr, "optimole folders: interrupted")
		return ExitInterrupted
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden:
		fmt.Fprintf(env.Stderr, "optimole folders: %s\n", msg)
		if isScopedKey(sess.settings.APIKey) {
			fmt.Fprintln(env.Stderr, "This is a folder-scoped API key, and those cannot list folders. "+
				"\"optimole upload --folder PATH\" still works for the folders it was granted.")
		} else {
			fmt.Fprintln(env.Stderr, "This key cannot list folders. If it is a folder-scoped key, "+
				"\"optimole upload --folder PATH\" still works for the folders it was granted.")
		}
	case isRejectedKey(err):
		fmt.Fprintf(env.Stderr, "optimole folders: %s\nThe API key (from %s) was rejected.\n", msg, sess.settings.KeySource)
	default:
		fmt.Fprintf(env.Stderr, "optimole folders: %s\n", msg)
	}
	return ExitFailure
}

// isScopedKey recognizes folder-scoped keys by the dashboard's prefix.
func isScopedKey(key string) bool {
	return strings.HasPrefix(key, "optml-s")
}

// folderPaths builds each folder's full path from the parent links. A parent
// missing from the list shows as "…".
func folderPaths(folders []api.Folder) map[int64]string {
	byID := make(map[int64]api.Folder, len(folders))
	for _, f := range folders {
		byID[f.ID] = f
	}
	paths := make(map[int64]string, len(folders))
	var resolve func(id int64, depth int) string
	resolve = func(id int64, depth int) string {
		if p, ok := paths[id]; ok {
			return p
		}
		f, ok := byID[id]
		if !ok {
			return "…"
		}
		p := f.Name
		if f.ParentID.Valid && f.ParentID.Value > 0 {
			if depth > 64 {
				p = "…/" + p
			} else {
				p = resolve(f.ParentID.Value, depth+1) + "/" + p
			}
		}
		paths[id] = p
		return p
	}
	for _, f := range folders {
		resolve(f.ID, 0)
	}
	return paths
}
