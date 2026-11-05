package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/Codeinwp/optimole-cli/internal/api"
	"github.com/Codeinwp/optimole-cli/internal/imgurl"
	"github.com/Codeinwp/optimole-cli/internal/output"
	"github.com/Codeinwp/optimole-cli/internal/redact"
	"github.com/Codeinwp/optimole-cli/internal/termecho"
)

const urlUsage = `Usage:
  optimole url [flags] KEY|URL...
  COMMAND | optimole url [flags]

Prints the CDN URL of each asset key or Optimole URL, changed as the flags
ask. Without arguments it reads one key or URL per line from stdin, and also
the JSON lines of "optimole upload --json" and "optimole url --json", so
commands can be piped together:

  optimole url 42cf6621995747f69a30d32f6a6c4f82 --width 800 --format webp
  optimole upload -r ./photos --json | optimole url --width 1200
  optimole url KEY --width 800 | optimole url --quality 60

URLs are changed locally, with no API call. Keys are looked up with the API,
which needs an API key. An option already in the URL is replaced, because the
CDN uses only the first copy of an option; options the flags do not name are
kept.

Image flags:
  --width N|auto     Width in pixels
  --height N|auto    Height in pixels
  --resize TYPE      fit (the default: keeps the aspect ratio and fits the size),
                     fill (fills the size and cuts what sticks out), fill-down,
                     force (ignores the aspect ratio) or auto
  --gravity POS      The part fill keeps: center, top, bottom, left, right,
                     top-left, top-right, bottom-left, bottom-right or smart
  --focus X:Y        The point fill keeps: 0:0 is the top left, 1:1 the bottom right
  --quality Q        auto, mauto, eco or 1-100
  --format F         auto (each browser gets AVIF or WebP when it accepts them),
                     webp, jpg, png, gif, ico, jxl or heic
  --dpr N            Pixel density, 1-5
  --download         Serve the image as a download
  --set NAME:VALUE   Set another CDN option, e.g. --set bl:5 for a blur (repeatable)
  --unset NAME       Remove an option from the URL (repeatable)
  --reset            Start from w:auto/h:auto/q:auto, dropping the URL's options

Other flags:
  -c, --concurrency N  Keys looked up in parallel, 1-16 (default 4)
  --json               Print one JSON object per input

Files that are not images (their URLs have /files/) are printed as they are;
image flags are an error for them.

Exit codes: 0 every input printed, 1 an input failed (the others are still
printed) or the API key was rejected, 2 usage or configuration error, 130
interrupted. A rejected or missing API key stops the run at the first key.
` + commonUsage

const (
	urlDefaultConcurrency = 4
	urlMaxConcurrency     = 16
	// maxURLLine caps a line read from stdin.
	maxURLLine = 1 << 20
)

// keyLike matches a line that could be an API key given by mistake: it is
// never echoed.
var keyLike = regexp.MustCompile(`^[A-Za-z0-9_-]{20,}$`)

// urlEdits is what the flags change in every URL.
type urlEdits struct {
	reset bool
	set   []imgurl.Option
	unset []string
}

func (e urlEdits) empty() bool { return !e.reset && len(e.set) == 0 && len(e.unset) == 0 }

func (e urlEdits) apply(u *imgurl.URL) {
	if e.reset {
		u.Reset()
	}
	for _, o := range e.set {
		u.Set(o.Name, o.Value)
	}
	for _, n := range e.unset {
		u.Unset(n)
	}
}

type urlFlags struct {
	width, height, resize, gravity, focus, quality, format, dpr string
	download, reset                                             bool
	set, unset                                                  stringList
}

// typedOption maps an image flag to the option it sets.
type typedOption struct {
	flag   string
	option string
	value  *string
	check  func(string) (string, error)
}

// edits turns the flags given (visited) into the changes to apply. Two flags
// that change the same option are a usage error.
func (f *urlFlags) edits(visited map[string]bool) (urlEdits, error) {
	e := urlEdits{reset: f.reset}
	owner := map[string]string{} // option -> flag that changes it
	claim := func(option, flag string) error {
		if prev, ok := owner[option]; ok {
			return fmt.Errorf("%s and %s both change the %s option", prev, flag, option)
		}
		owner[option] = flag
		return nil
	}
	typed := []typedOption{
		{"--width", "w", &f.width, imgurl.Size},
		{"--height", "h", &f.height, imgurl.Size},
		{"--resize", "rt", &f.resize, imgurl.ResizeType},
		{"--gravity", "g", &f.gravity, imgurl.Gravity},
		{"--focus", "g", &f.focus, imgurl.Focus},
		{"--quality", "q", &f.quality, imgurl.Quality},
		{"--format", "f", &f.format, imgurl.Format},
		{"--dpr", "dpr", &f.dpr, imgurl.DPR},
	}
	for _, t := range typed {
		if !visited[strings.TrimPrefix(t.flag, "--")] {
			continue
		}
		if err := claim(t.option, t.flag); err != nil {
			return e, err
		}
		v, err := t.check(*t.value)
		if err != nil {
			return e, fmt.Errorf("%s %s: %v", t.flag, *t.value, err)
		}
		if v == "" { // --format auto
			e.unset = append(e.unset, t.option)
			continue
		}
		e.set = append(e.set, imgurl.Option{Name: t.option, Value: v})
	}
	if f.download {
		if err := claim("att", "--download"); err != nil {
			return e, err
		}
		e.set = append(e.set, imgurl.Option{Name: "att", Value: "1"})
	}
	for _, s := range f.set {
		name, value, ok := strings.Cut(s, ":")
		if !ok {
			return e, fmt.Errorf("--set %s: must be NAME:VALUE, e.g. bl:5", s)
		}
		if err := imgurl.Check(name, value); err != nil {
			return e, fmt.Errorf("--set %s: %v", s, err)
		}
		if err := claim(name, "--set "+s); err != nil {
			return e, err
		}
		e.set = append(e.set, imgurl.Option{Name: name, Value: value})
	}
	for _, name := range f.unset {
		if !imgurl.IsOption(name) {
			return e, fmt.Errorf("--unset %s: %q is not an option the CDN reads", name, name)
		}
		if err := claim(name, "--unset "+name); err != nil {
			return e, err
		}
		e.unset = append(e.unset, name)
	}
	return e, nil
}

func runURL(ctx context.Context, args []string, env Env) int {
	fs := newFlagSet("url", urlUsage, env)
	var common commonFlags
	common.register(fs, true)
	fs.SetOutput(flagErrorWriter{w: env.Stderr})
	var f urlFlags
	var jsonOut bool
	concurrency := urlDefaultConcurrency
	for _, s := range []struct {
		name string
		v    *string
	}{
		{"width", &f.width}, {"height", &f.height}, {"resize", &f.resize}, {"gravity", &f.gravity},
		{"focus", &f.focus}, {"quality", &f.quality}, {"format", &f.format}, {"dpr", &f.dpr},
	} {
		fs.StringVar(s.v, s.name, "", "")
	}
	fs.BoolVar(&f.download, "download", false, "")
	fs.BoolVar(&f.reset, "reset", false, "")
	fs.Var(&f.set, "set", "")
	fs.Var(&f.unset, "unset", "")
	fs.IntVar(&concurrency, "c", urlDefaultConcurrency, "")
	fs.IntVar(&concurrency, "concurrency", urlDefaultConcurrency, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	rest, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	// Flag values are echoed in usage errors: a key given by mistake must
	// not be.
	red := common.resolve(env).redact
	visited := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { visited[fl.Name] = true })
	edits, err := f.edits(visited)
	if err != nil {
		return usageError(env, "url", "%s", red.String(err.Error()))
	}
	if concurrency < 1 || concurrency > urlMaxConcurrency {
		return usageError(env, "url", "--concurrency must be between 1 and %d", urlMaxConcurrency)
	}

	inputs := func(context.Context) (<-chan string, func() error) {
		ch := make(chan string, len(rest))
		for _, a := range rest {
			ch <- a
		}
		close(ch)
		return ch, func() error { return nil }
	}
	if len(rest) == 0 || (len(rest) == 1 && rest[0] == "-") {
		if file, ok := env.Stdin.(*os.File); ok && termecho.IsTerminal(file) {
			return usageError(env, "url", "give asset keys or URLs, or pipe them in")
		}
		inputs = func(ctx context.Context) (<-chan string, func() error) { return readLines(ctx, env.Stdin) }
	}

	// The first key connects inside a lookup, whose warnings would otherwise
	// race the results written to stderr.
	env.Stderr = &lockedWriter{w: env.Stderr}
	run := &urlRun{env: env, common: &common, edits: edits, redact: red}
	return run.run(ctx, inputs, concurrency, jsonOut)
}

// The flag package's errors that repeat what was typed: a value
// (`invalid value "x" for flag -c`, `invalid boolean value "x" for -json`),
// a whole argument (`bad flag syntax: ---x`) and a flag name
// (`flag provided but not defined: -x`).
// The arguments, which can span lines, run to the end of the message.
var (
	flagValue   = regexp.MustCompile(`(invalid (?:boolean )?value) "(?:[^"\\]|\\.)*"`)
	flagSyntax  = regexp.MustCompile(`(?s)(bad flag syntax): .*`)
	flagUnknown = regexp.MustCompile(`(?s)flag provided but not defined: -(.*)`)
	// flagName is what a mistyped flag looks like; anything else (a value
	// pasted with it, a key) is not repeated.
	flagName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,18}$`)
)

// flagErrorWriter drops from the flag package's errors the values and
// arguments typed, keeping only a plain mistyped flag name: what was typed
// could be an API key given by mistake, from any source.
type flagErrorWriter struct{ w io.Writer }

func (f flagErrorWriter) Write(p []byte) (int, error) {
	s := flagValue.ReplaceAllString(string(p), "$1")
	s = flagSyntax.ReplaceAllString(s, "$1\n")
	s = flagUnknown.ReplaceAllStringFunc(s, func(m string) string {
		if name := strings.TrimSuffix(flagUnknown.FindStringSubmatch(m)[1], "\n"); !flagName.MatchString(name) {
			return "flag provided but not defined (what was typed is not shown)\n"
		}
		return m
	})
	if _, err := io.WriteString(f.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

// lockedWriter serializes writes from several goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// readLines streams r line by line. The returned function gives the read
// error, if any, once the channel is closed.
func readLines(ctx context.Context, r io.Reader) (<-chan string, func() error) {
	ch := make(chan string)
	var err error
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxURLLine)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		if e := sc.Err(); e != nil {
			if errors.Is(e, bufio.ErrTooLong) {
				e = fmt.Errorf("a line is longer than %d bytes", maxURLLine)
			}
			err = fmt.Errorf("reading stdin: %w", e)
		}
	}()
	// err is written before close(ch), and read only after it.
	return ch, func() error { return err }
}

// urlInput is one input line, sorted out.
type urlInput struct {
	shown string // what messages call it
	key   string // an asset key to look up
	url   string // a URL to change
	// Details a JSON line carried along.
	mimeType      string
	width, height int64
	err           error // neither a key nor a URL
	skip          bool  // blank, or an upload summary line
}

func classify(line string) urlInput {
	s := strings.TrimSpace(line)
	switch {
	case s == "":
		return urlInput{skip: true}
	case strings.HasPrefix(s, "{"):
		return classifyJSON(s)
	case api.IsAssetKey(s):
		return urlInput{shown: s, key: strings.ToLower(s)}
	case hasHTTPScheme(s):
		return urlInput{shown: s, url: s}
	case keyLike.MatchString(s):
		return urlInput{shown: "(hidden)", err: errors.New("not an asset key or an Optimole URL; it looks like an API key, so it is not shown")}
	}
	return urlInput{shown: s, err: errors.New("not an asset key (32 hexadecimal characters) or an Optimole URL")}
}

// classifyJSON reads a line of "optimole upload --json" or "optimole url --json".
func classifyJSON(s string) urlInput {
	var line struct {
		URL            string          `json:"url"`
		Path           string          `json:"path"`
		Status         string          `json:"status"`
		MimeType       string          `json:"mime_type"`
		OriginalWidth  int64           `json:"original_width"`
		OriginalHeight int64           `json:"original_height"`
		Summary        json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal([]byte(s), &line); err != nil {
		return urlInput{shown: s, err: errors.New("not valid JSON")}
	}
	if line.Summary != nil {
		return urlInput{skip: true}
	}
	if line.URL != "" {
		return urlInput{shown: line.URL, url: line.URL, mimeType: line.MimeType, width: line.OriginalWidth, height: line.OriginalHeight}
	}
	in := urlInput{shown: s}
	if line.Path != "" {
		in.shown = line.Path
	}
	msg := `the JSON line has no "url"`
	if line.Status != "" {
		msg += " (status " + line.Status + ")"
	}
	in.err = errors.New(msg)
	return in
}

func hasHTTPScheme(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

// shorten keeps messages about a long line readable.
func shorten(s string) string {
	const limit = 120
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

// urlResult is the outcome of one input.
type urlResult struct {
	in   urlInput
	url  string
	key  string
	err  error
	code string
	// fatal, when not zero, stops the run with this exit code after
	// printing fatalMsg: nothing more is printed, and the lookups still
	// running are cancelled. It is set when connecting fails (no API key, a
	// bad API URL) or the API key is rejected, which would fail every later
	// key the same way.
	fatal    int
	fatalMsg string
}

// urlRun holds what every input shares.
type urlRun struct {
	env    Env
	common *commonFlags
	edits  urlEdits
	redact redact.Redactor // hides the known API keys in what is printed

	once    sync.Once
	sess    *session
	sessErr error
}

// session connects on the first key, so URL-only runs need no API key.
func (r *urlRun) session() (*session, error) {
	r.once.Do(func() { r.sess, r.sessErr = r.common.connect(r.env) })
	return r.sess, r.sessErr
}

// run resolves the inputs, up to concurrency at a time, and prints the
// results in input order.
func (r *urlRun) run(ctx context.Context, inputs func(context.Context) (<-chan string, func() error), concurrency int, jsonOut bool) int {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines, readErr := inputs(runCtx)
	results := make(chan chan urlResult, concurrency)
	go func() {
		defer close(results)
		sem := make(chan struct{}, concurrency)
		for {
			var line string
			var more bool
			select {
			case line, more = <-lines:
			case <-runCtx.Done():
				return
			}
			if !more {
				return
			}
			in := classify(line)
			if in.skip {
				continue
			}
			ch := make(chan urlResult, 1)
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			select {
			case results <- ch:
			case <-runCtx.Done():
				return
			}
			go func() {
				defer func() { <-sem }()
				ch <- r.resolve(runCtx, in)
			}()
		}
	}()

	out := output.NewErrWriter(r.env.Stdout)
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	interrupted := func() int {
		fmt.Fprintln(r.env.Stderr, "optimole url: interrupted")
		return ExitInterrupted
	}
	failed := 0
	for {
		var ch chan urlResult
		var more bool
		select {
		case ch, more = <-results:
		case <-ctx.Done():
			return interrupted()
		}
		if !more {
			break
		}
		var res urlResult
		select {
		case res = <-ch:
		case <-ctx.Done():
			return interrupted()
		}
		if ctx.Err() != nil {
			return interrupted()
		}
		if res.fatal != 0 {
			cancel()
			fmt.Fprintln(r.env.Stderr, res.fatalMsg)
			return res.fatal
		}
		if res.err != nil {
			failed++
			if jsonOut {
				enc.Encode(urlLine{Input: shorten(r.redact.String(res.in.shown)), Error: r.redact.String(res.err.Error()), Code: res.code})
			} else {
				fmt.Fprintf(r.env.Stderr, "optimole url: %s: %s\n", r.printable(shorten(r.redact.String(res.in.shown))), r.printable(res.err.Error()))
			}
		} else if jsonOut {
			enc.Encode(urlLine{
				Input: r.redact.String(res.in.shown), URL: res.url, Key: res.key, MimeType: res.in.mimeType,
				OriginalWidth: res.in.width, OriginalHeight: res.in.height,
			})
		} else {
			fmt.Fprintln(out, res.url)
		}
		if err := out.Err(); err != nil {
			return outputFailed(r.env, err)
		}
	}
	if ctx.Err() != nil {
		return interrupted()
	}
	if err := readErr(); err != nil {
		fmt.Fprintf(r.env.Stderr, "optimole url: %v\n", err)
		return ExitFailure
	}
	if failed > 0 {
		return ExitFailure
	}
	return ExitOK
}

// printable hides the known API keys in s and quotes it when it holds
// control characters, which a terminal would otherwise act on.
func (r *urlRun) printable(s string) string {
	s = r.redact.String(s)
	if strings.ContainsFunc(s, unicode.IsControl) {
		return strconv.Quote(s)
	}
	return s
}

// urlLine is the JSON object printed for each input.
type urlLine struct {
	Input          string `json:"input"`
	URL            string `json:"url,omitempty"`
	Key            string `json:"key,omitempty"`
	MimeType       string `json:"mime_type,omitempty"`
	OriginalWidth  int64  `json:"original_width,omitempty"`
	OriginalHeight int64  `json:"original_height,omitempty"`
	Error          string `json:"error,omitempty"`
	Code           string `json:"code,omitempty"`
}

// Codes of the errors "optimole url" reports per input.
const (
	codeInvalidInput = "invalid_input"
	codeNotFound     = "not_found"
	codeNoURL        = "no_url"
	codeNotAnImage   = "not_an_image"
)

// resolve turns one input into its URL.
func (r *urlRun) resolve(ctx context.Context, in urlInput) urlResult {
	res := urlResult{in: in}
	if in.err != nil {
		res.err, res.code = in.err, codeInvalidInput
		return res
	}
	raw := in.url
	if in.key != "" {
		var ok bool
		if raw, ok = r.lookUp(ctx, &res); !ok {
			return res
		}
	}
	u, err := imgurl.Parse(raw)
	if err != nil {
		res.err, res.code = err, codeInvalidInput
		return res
	}
	if !r.edits.empty() && u.Kind() == imgurl.File {
		res.err, res.code = errors.New("not an image (a file URL): only image URLs can be changed"), codeNotAnImage
		return res
	}
	r.edits.apply(u)
	if err := u.Validate(); err != nil {
		res.err, res.code = err, codeInvalidInput
		return res
	}
	res.url = u.String()
	if res.key == "" && api.IsAssetKey(u.ID()) {
		res.key = strings.ToLower(u.ID())
	}
	return res
}

// lookUp fetches the URL of res.in.key. It fills res and returns false when
// the input failed or the run must stop.
func (r *urlRun) lookUp(ctx context.Context, res *urlResult) (string, bool) {
	key := res.in.key
	sess, err := r.session()
	if err != nil {
		res.fatal, res.fatalMsg = ExitUsage, fmt.Sprintf("optimole url: %v", err)
		return "", false
	}
	a, err := sess.client.GetAsset(ctx, key)
	var apiErr *api.Error
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		// Ctrl-C, or the run stopping after a fatal error: either way the
		// result is never printed.
		res.fatal, res.fatalMsg = ExitInterrupted, "optimole url: interrupted"
		return "", false
	case isRejectedKey(err):
		res.fatal = ExitFailure
		res.fatalMsg = fmt.Sprintf("optimole url: %s\nThe API key (from %s) was rejected.", sess.redact.Error(err), sess.settings.KeySource)
		return "", false
	case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusNotFound):
		res.err, res.code = errors.New("no such asset, or this API key cannot see it"), codeNotFound
		return "", false
	default:
		res.err = errors.New(sess.redact.Error(err))
		if errors.As(err, &apiErr) {
			res.code = apiErr.Code
		}
		return "", false
	}
	res.key = key
	if res.in.mimeType == "" {
		res.in.mimeType = a.MimeType
	}
	if res.in.width == 0 && a.Width.Valid {
		res.in.width = a.Width.Value
	}
	if res.in.height == 0 && a.Height.Valid {
		res.in.height = a.Height.Value
	}
	if a.URL == "" {
		res.err, res.code = errors.New("the asset has no CDN URL yet"), codeNoURL
		return "", false
	}
	return a.URL, true
}
