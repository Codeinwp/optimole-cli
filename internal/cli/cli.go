// Package cli implements the optimole command line: argument parsing,
// configuration and the subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/config"
	"github.com/Codeinwp/optimole-cli/internal/redact"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitFailure     = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// BuildInfo is set at build time through ldflags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// Env is everything the commands touch outside the process: streams,
// environment, the config directory and the network.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	// UserConfigDir returns the base config directory (os.UserConfigDir).
	UserConfigDir func() (string, error)
	Build         BuildInfo
	// Transport, when set, carries every HTTP request (tests use it to make
	// sure nothing leaves the machine).
	Transport http.RoundTripper
	// Sleep, when set, replaces the wait between retries.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultEnv is the real process environment.
func DefaultEnv(build BuildInfo) Env {
	return Env{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Getenv:        os.Getenv,
		UserConfigDir: os.UserConfigDir,
		Build:         build,
	}
}

const mainUsage = `optimole uploads local files into an Optimole media library.

Usage:
  optimole <command> [flags] [arguments]

Commands:
  upload     Upload files and directories into library folders
  folders    List the library's folders
  login      Save an API key (read from stdin)
  logout     Delete the saved API key
  version    Print the version

Run "optimole <command> -h" for a command's flags.
`

// Run executes the command line and returns the process exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, mainUsage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "upload":
		return runUpload(ctx, rest, env)
	case "folders":
		return runFolders(ctx, rest, env)
	case "login":
		return runLogin(ctx, rest, env)
	case "logout":
		return runLogout(rest, env)
	case "version", "--version":
		return runVersion(rest, env)
	case "help", "-h", "-help", "--help":
		if len(rest) > 0 && rest[0] != "help" {
			return Run(ctx, []string{rest[0], "-h"}, env)
		}
		fmt.Fprint(env.Stdout, mainUsage)
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "optimole: unknown command %q\n\n%s", cmd, mainUsage)
		return ExitUsage
	}
}

// newFlagSet returns a flag set that prints usage to stderr and returns
// errors instead of exiting.
func newFlagSet(name, usage string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprint(env.Stderr, usage) }
	return fs
}

// parseArgs parses flags placed anywhere among the arguments (Go's flag
// package stops at the first argument otherwise). "--" ends the flags.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		consumed := len(args) - len(rest)
		if consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// flagExit maps a flag parsing error to an exit code (help is not an error).
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	return ExitUsage
}

// commonFlags are the connection flags every networked command takes.
type commonFlags struct {
	apiKey string
	apiURL string
}

func (c *commonFlags) register(fs *flag.FlagSet, withKey bool) {
	if withKey {
		fs.StringVar(&c.apiKey, "api-key", "", "")
	}
	fs.StringVar(&c.apiURL, "api-url", "", "")
}

const commonUsage = `
Connection flags:
  --api-key KEY   API key (default: $OPTIMOLE_API_KEY, then the key saved by "optimole login")
  --api-url URL   API base URL (default: $OPTIMOLE_API_URL, then https://dashboard.optimole.com/api)
`

// session is a configured client plus the redactor for its key.
type session struct {
	client   *api.Client
	settings config.Settings
	redact   redact.Redactor
}

// configPath returns the config file path.
func configPath(env Env) (string, error) {
	if env.UserConfigDir == nil {
		return "", errors.New("no user config directory")
	}
	dir, err := env.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the config directory: %w", err)
	}
	return config.Path(dir), nil
}

// connect resolves the key and URL and builds a client. Errors are
// configuration errors (exit 2); they never contain the key.
func (c *commonFlags) connect(env Env) (*session, error) {
	path, pathErr := configPath(env)
	in := config.Inputs{FlagAPIKey: c.apiKey, FlagAPIURL: c.apiURL, Getenv: env.Getenv}
	if pathErr == nil {
		in.ConfigPath = path
	}
	settings, err := config.Resolve(in)
	envKey := ""
	if env.Getenv != nil {
		envKey = env.Getenv(config.EnvAPIKey)
	}
	red := redact.New(settings.APIKey, c.apiKey, envKey)
	if err != nil {
		if errors.Is(err, config.ErrNoAPIKey) && pathErr != nil {
			return nil, fmt.Errorf("%w (%v)", err, pathErr)
		}
		return nil, errors.New(red.Error(err))
	}
	if settings.LooseConfig {
		fmt.Fprintf(env.Stderr, "warning: %s can be read by other users; run: chmod 600 %q\n", path, path)
	}
	client, err := newClient(env, settings.APIURL, settings.APIKey)
	if err != nil {
		return nil, errors.New(red.Error(err))
	}
	return &session{client: client, settings: settings, redact: red}, nil
}

func newClient(env Env, baseURL, key string) (*api.Client, error) {
	return api.New(api.Options{
		BaseURL:   baseURL,
		APIKey:    key,
		UserAgent: userAgent(env.Build),
		Transport: env.Transport,
		Sleep:     env.Sleep,
	})
}

func userAgent(b BuildInfo) string {
	v := strings.TrimPrefix(b.Version, "v")
	if v == "" {
		v = "dev"
	}
	return "optimole-cli/" + v
}

// usageError prints a usage problem and returns ExitUsage.
func usageError(env Env, cmd, format string, args ...any) int {
	fmt.Fprintf(env.Stderr, "optimole %s: %s\n", cmd, fmt.Sprintf(format, args...))
	fmt.Fprintf(env.Stderr, "Run \"optimole %s -h\" for help.\n", cmd)
	return ExitUsage
}

// isRejectedKey reports a 401 from the dashboard.
func isRejectedKey(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized
}
