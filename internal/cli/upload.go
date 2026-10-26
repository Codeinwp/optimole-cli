package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/Codeinwp/optimole-cli/internal/output"
	"github.com/Codeinwp/optimole-cli/internal/upload"
)

const uploadUsage = `Usage:
  optimole upload [flags] <path>...

Uploads files, and with -r the contents of directories, into the media
library. A directory's contents go into --folder (or the library root) and each
subdirectory becomes a subfolder below it:

  optimole upload -r ./photos --folder Campaigns
    ./photos/a.jpg       -> Campaigns
    ./photos/2024/b.jpg  -> Campaigns/2024

Flags:
  --folder PATH        Destination folder path from the library root, e.g. "Campaigns/2026"
  --folder-id N        Destination folder ID (instead of --folder; no subdirectories)
  --create-folders     Create missing folders (full-access keys only)
  -r, --recursive      Upload directory contents, mapping subdirectories to subfolders
  --include-hidden     Include dot-prefixed files and directories
  --exclude GLOB       Skip paths matching GLOB (repeatable); tried against the path
                       relative to the argument and against the base name
  -c, --concurrency N  Files uploaded in parallel, 1-16 (default 4)
  --dry-run            List what would go where, without contacting the server
  --json               Print one JSON object per file and a final summary object
` + commonUsage + `
Exit codes: 0 every file uploaded, 1 a file failed or nothing to upload,
2 usage or configuration error, 130 interrupted.
`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runUpload(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("upload", uploadUsage, env)
	var common commonFlags
	common.register(fs, true)
	var (
		folder, folderIDRaw                     string
		createFolders, recursive, includeHidden bool
		dryRun, jsonOut                         bool
		concurrency                             int
		excludes                                stringList
	)
	fs.StringVar(&folder, "folder", "", "")
	fs.StringVar(&folderIDRaw, "folder-id", "", "")
	fs.BoolVar(&createFolders, "create-folders", false, "")
	fs.BoolVar(&recursive, "r", false, "")
	fs.BoolVar(&recursive, "recursive", false, "")
	fs.BoolVar(&includeHidden, "include-hidden", false, "")
	fs.Var(&excludes, "exclude", "")
	fs.IntVar(&concurrency, "c", upload.DefaultConcurrency, "")
	fs.IntVar(&concurrency, "concurrency", upload.DefaultConcurrency, "")
	fs.BoolVar(&dryRun, "dry-run", false, "")
	fs.BoolVar(&jsonOut, "json", false, "")

	paths, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(paths) == 0 {
		return usageError(env, "upload", "no files or directories given")
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["folder"] && set["folder-id"] {
		return usageError(env, "upload", "--folder and --folder-id cannot be used together")
	}
	if concurrency < 1 || concurrency > upload.MaxConcurrency {
		return usageError(env, "upload", "--concurrency must be between 1 and %d", upload.MaxConcurrency)
	}
	opts := upload.PlanOptions{Recursive: recursive, IncludeHidden: includeHidden, Excludes: excludes}
	if set["folder-id"] {
		id, err := parseFolderID(folderIDRaw)
		if err != nil {
			return usageError(env, "upload", "--folder-id: %v", err)
		}
		if createFolders {
			return usageError(env, "upload", "--create-folders needs --folder (or -r with subdirectories), not --folder-id")
		}
		opts.FolderID = id
	}
	if opts.Folder, err = upload.NormalizeFolderPath(folder); err != nil {
		return usageError(env, "upload", "--folder: %v", err)
	}

	plan, err := upload.BuildPlan(paths, opts)
	if err != nil {
		var ue *upload.UsageError
		if errors.As(err, &ue) {
			return usageError(env, "upload", "%v", err)
		}
		fmt.Fprintf(env.Stderr, "optimole upload: %v\n", err)
		return ExitFailure
	}

	// The reporter redacts with the session's key once there is one.
	var sess *session
	rep := output.New(env.Stdout, jsonOut, func(s string) string {
		if sess != nil {
			return sess.redact.String(s)
		}
		return s
	})
	planFailed := false
	for _, it := range plan.Items {
		if it.Status == upload.StatusPlanned {
			continue
		}
		if it.Status == upload.StatusFailed {
			planFailed = true
		}
		rep.Result(upload.Result{Item: it, Status: it.Status, Err: errors.New(it.Reason)})
	}
	selected := plan.Selected()

	if dryRun {
		for _, it := range selected {
			rep.Planned(it)
		}
		rep.Finish(plan.Ignored, true, false)
		if len(selected) == 0 {
			fmt.Fprintln(env.Stderr, "optimole upload: nothing to upload")
			return ExitFailure
		}
		if planFailed {
			return ExitFailure
		}
		return ExitOK
	}
	if len(selected) == 0 {
		rep.Finish(plan.Ignored, false, false)
		fmt.Fprintln(env.Stderr, "optimole upload: nothing to upload")
		return ExitFailure
	}

	if sess, err = common.connect(env); err != nil {
		fmt.Fprintf(env.Stderr, "optimole upload: %v\n", err)
		return ExitUsage
	}

	runner := upload.Runner{
		Client:        sess.client,
		Concurrency:   concurrency,
		CreateFolders: createFolders,
		OnResult:      rep.Result,
	}
	outcome := runner.Run(ctx, selected)
	rep.Finish(plan.Ignored, false, outcome.Interrupted)

	switch {
	case outcome.Interrupted:
		return ExitInterrupted
	case outcome.Failed > 0 || planFailed:
		if isRejectedKey(outcome.Stopped) {
			fmt.Fprintf(env.Stderr, "optimole upload: the API key (from %s) was rejected\n", sess.settings.KeySource)
		}
		return ExitFailure
	}
	return ExitOK
}

func parseFolderID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a positive folder ID", raw)
	}
	return id, nil
}
