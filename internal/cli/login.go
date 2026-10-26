package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/redact"
	"github.com/Codeinwp/optimole-cli/internal/termecho"
)

const loginUsage = `Usage:
  optimole login [--api-url URL]

Reads an API key from stdin, checks it against the API and saves it for later
commands (in the user config directory, readable only by you). Type or paste
the key at the prompt, or pipe it in:

  optimole login < key.txt
  printf '%s' "$KEY" | optimole login

Folder-scoped keys work too; they can upload into the folders they were
granted but cannot list folders.

Flags:
  --api-url URL   API base URL used to check the key
                  (default: $OPTIMOLE_API_URL, then https://dashboard.optimole.com/api)
`

const logoutUsage = `Usage:
  optimole logout

Deletes the API key saved by "optimole login".
`

// maxKeyInput caps how much of stdin is read for a key.
const maxKeyInput = 4096

func runLogin(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("login", loginUsage, env)
	var common commonFlags
	common.register(fs, false)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, "login", "unexpected argument %q (the key is read from stdin, never from the command line)", rest[0])
	}
	path, err := configPath(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole login: %v\n", err)
		return ExitUsage
	}

	key, err := readKey(ctx, env)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(env.Stderr, "optimole login: interrupted")
			return ExitInterrupted
		}
		fmt.Fprintf(env.Stderr, "optimole login: %v\n", err)
		return ExitUsage
	}
	red := redact.New(key)

	baseURL, _ := config.ResolveURL(config.Inputs{FlagAPIURL: common.apiURL, Getenv: env.Getenv})
	client, err := newClient(env, baseURL, key)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole login: %s\n", red.Error(err))
		return ExitUsage
	}

	// GET /folders is cheap and authenticated. A 403 still proves the key:
	// folder-scoped keys (and accounts without an active plan) get one.
	note := ""
	if _, err := client.ListFolders(ctx, api.FolderQuery{PerPage: 1}); err != nil {
		var apiErr *api.Error
		switch {
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(env.Stderr, "optimole login: interrupted")
			return ExitInterrupted
		case isRejectedKey(err):
			fmt.Fprintln(env.Stderr, "optimole login: the API rejected this key; nothing was saved")
			return ExitFailure
		case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden:
			if isScopedKey(key) {
				note = "This is a folder-scoped key: it can upload into its granted folders with --folder, but cannot list folders."
			} else {
				note = "The key is valid, but the API refused to list folders: " + red.String(apiErr.Message)
			}
		default:
			fmt.Fprintf(env.Stderr, "optimole login: could not check the key: %s\nNothing was saved.\n", red.Error(err))
			return ExitFailure
		}
	}

	if err := config.Save(path, config.File{APIKey: key}); err != nil {
		fmt.Fprintf(env.Stderr, "optimole login: %s\n", red.Error(err))
		return ExitFailure
	}
	fmt.Fprintf(env.Stdout, "Logged in. The API key was saved to %s\n", path)
	if note != "" {
		fmt.Fprintln(env.Stdout, note)
	}
	if env.Getenv != nil && strings.TrimSpace(env.Getenv(config.EnvAPIKey)) != "" {
		fmt.Fprintf(env.Stderr, "note: %s is set and takes precedence over the saved key\n", config.EnvAPIKey)
	}
	return ExitOK
}

// readKey reads the key from stdin. On a terminal it prompts and hides the
// input when the platform allows; otherwise it warns that input is visible.
func readKey(ctx context.Context, env Env) (string, error) {
	var line string
	f, isFile := env.Stdin.(*os.File)
	if isFile && termecho.IsTerminal(f) {
		restore, err := termecho.Disable(f)
		if err != nil {
			fmt.Fprintln(env.Stderr, "warning: input cannot be hidden on this terminal; the key will be visible as you type.")
		}
		fmt.Fprint(env.Stderr, "Optimole API key: ")
		line, err = readLine(ctx, f)
		if restore != nil {
			restore()
			fmt.Fprintln(env.Stderr)
		}
		if err != nil {
			return "", err
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(env.Stdin, maxKeyInput))
		if err != nil {
			return "", fmt.Errorf("reading the key from stdin: %w", err)
		}
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				line = l
				break
			}
		}
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no API key on stdin")
	}
	if strings.IndexFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("the API key must be a single word without spaces")
	}
	return key, nil
}

// readLine reads one line from a terminal, giving up when ctx is done (the
// read itself cannot be interrupted, so it is left behind).
func readLine(ctx context.Context, f *os.File) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReaderSize(f, maxKeyInput).ReadString('\n')
		if errors.Is(err, io.EOF) && line != "" {
			err = nil
		}
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("reading the key: %w", r.err)
		}
		return r.line, nil
	}
}

func runLogout(args []string, env Env) int {
	fs := newFlagSet("logout", logoutUsage, env)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(rest) > 0 {
		return usageError(env, "logout", "unexpected argument %q", rest[0])
	}
	path, err := configPath(env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole logout: %v\n", err)
		return ExitUsage
	}
	removed, err := config.Remove(path)
	if err != nil {
		fmt.Fprintf(env.Stderr, "optimole logout: %v\n", err)
		return ExitFailure
	}
	if removed {
		fmt.Fprintf(env.Stdout, "Logged out. Deleted %s\n", path)
	} else {
		fmt.Fprintln(env.Stdout, "No saved API key.")
	}
	if env.Getenv != nil && strings.TrimSpace(env.Getenv(config.EnvAPIKey)) != "" {
		fmt.Fprintf(env.Stderr, "note: %s is still set in this environment\n", config.EnvAPIKey)
	}
	return ExitOK
}

const versionUsage = `Usage:
  optimole version
`

func runVersion(args []string, env Env) int {
	fs := newFlagSet("version", versionUsage, env)
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	b := env.Build
	fmt.Fprintf(env.Stdout, "optimole %s (commit %s, built %s)\n", orDefault(b.Version, "dev"), orDefault(b.Commit, "unknown"), orDefault(b.Date, "unknown"))
	return ExitOK
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
