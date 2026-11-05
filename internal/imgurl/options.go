package imgurl

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// cdnOptions are the options the CDN reads, in the order it applies them:
// its viewer-request function sorts a path's options into this order and
// drops any other "name:value" segment before the source. "ig" (only
// "ig:avif": leave AVIF out of the formats a browser can get) is read before
// that sort, so its place does not matter. "id" is not here: it starts the
// source.
var cdnOptions = []string{
	"cb", "c", "w", "h", "q", "rt", "rs", "g", "dpr", "f", "ig", "bl", "sh",
	"pix", "br", "co", "sa", "process", "m", "mh", "mw", "zoom", "el", "ex",
	"pd", "trim", "rot", "ar", "bg", "fq", "autoquality", "mb", "jpgo", "pngo",
	"pg", "vts", "fiu", "skp", "exp", "fn", "pr", "strip_metadata", "sm",
	"keep_copyright", "scp", "eth", "att", "wm", "wmu", "wmt", "wms", "st",
	"no-auto", "raw",
}

// IsOption reports whether the CDN reads an option of this name.
func IsOption(name string) bool {
	for _, n := range cdnOptions {
		if n == name {
			return true
		}
	}
	return false
}

// Check validates an option the way the CDN reads it. Options the CDN
// answers with an empty image for (bad w, h or q), silently drops (a cache
// buster without a dot) or resets (an unknown gravity) are refused.
func Check(name, value string) error {
	if !IsOption(name) {
		return fmt.Errorf("%q is not an option the CDN reads", name)
	}
	if value == "" {
		return errors.New("missing value")
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		return r == '/' || r == '?' || r == '#' || r == '%' || unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return errors.New("the value cannot contain /, ?, #, % or spaces")
	}
	var err error
	switch name {
	case "w", "h":
		_, err = Size(value)
	case "q":
		_, err = Quality(value)
	case "rt":
		_, err = ResizeType(value)
	case "g":
		err = checkGravityValue(value)
	case "dpr":
		_, err = DPR(value)
	case "cb":
		err = checkCacheBuster(value)
	}
	return err
}

// Size validates a width or height: "auto" or a number of pixels.
func Size(v string) (string, error) {
	if v == "auto" {
		return v, nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 1 || strings.HasPrefix(v, "+") || strings.HasPrefix(v, "0") {
		return "", errors.New(`must be "auto" or a number of pixels (1 or more)`)
	}
	return v, nil
}

// Quality validates a quality: auto, mauto, eco or 1-100.
func Quality(v string) (string, error) {
	switch v {
	case "auto", "mauto", "eco":
		return v, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 100 || strings.HasPrefix(v, "+") || strings.HasPrefix(v, "0") {
		return "", errors.New("must be auto, mauto, eco or a number from 1 to 100")
	}
	return v, nil
}

// resizeTypes are the resize types the dashboard offers. The CDN answers
// "crop" with a redirect to the original image.
var resizeTypes = []string{"fit", "fill", "fill-down", "force", "auto"}

// ResizeType validates a resize type.
func ResizeType(v string) (string, error) {
	for _, t := range resizeTypes {
		if v == t {
			return v, nil
		}
	}
	return "", fmt.Errorf("must be %s", list(resizeTypes))
}

// gravityWords maps the names the CLI takes to the CDN's gravity codes.
var gravityWords = []struct{ word, code string }{
	{"center", "ce"},
	{"top", "no"},
	{"bottom", "so"},
	{"left", "we"},
	{"right", "ea"},
	{"top-left", "nowe"},
	{"top-right", "noea"},
	{"bottom-left", "sowe"},
	{"bottom-right", "soea"},
	{"smart", "sm"},
}

// Gravity returns the gravity code for a name (top-left) or a code (nowe).
func Gravity(v string) (string, error) {
	v = strings.ToLower(v)
	for _, g := range gravityWords {
		if v == g.word || v == g.code {
			return g.code, nil
		}
	}
	words := make([]string, len(gravityWords))
	for i, g := range gravityWords {
		words[i] = g.word
	}
	return "", fmt.Errorf("must be %s", list(words))
}

// Focus returns the focus point gravity ("fp:X:Y") for "X:Y" or "X,Y", two
// fractions from 0 to 1 measured from the top left.
func Focus(v string) (string, error) {
	sep := ":"
	if strings.Contains(v, ",") {
		sep = ","
	}
	xs, ys, ok := strings.Cut(v, sep)
	x, errX := fraction(xs)
	y, errY := fraction(ys)
	if !ok || errX != nil || errY != nil {
		return "", errors.New("must be X:Y, two numbers from 0 to 1 (0:0 is the top left)")
	}
	return "fp:" + x + ":" + y, nil
}

func fraction(s string) (string, error) {
	f, err := finite(s)
	if err != nil || f < 0 || f > 1 {
		return "", errors.New("not a fraction")
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

// finite parses a number, refusing NaN and infinities.
func finite(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("not a number")
	}
	return f, nil
}

// checkGravityValue accepts a gravity code with optional X:Y offsets in
// pixels, "obj" (object detection), or a focus point "fp:X:Y". The CDN turns
// an unknown code into the center.
func checkGravityValue(v string) error {
	code, rest, hasRest := strings.Cut(v, ":")
	if code == "fp" {
		// The CDN reads the point only in this form: fp:X:Y.
		if strings.Contains(rest, ",") {
			return errors.New("must be fp:X:Y, with colons")
		}
		_, err := Focus(rest)
		return err
	}
	known := code == "obj"
	for _, g := range gravityWords {
		known = known || code == g.code
	}
	if !known {
		return errors.New("must be a gravity code (ce, no, so, ea, we, noea, nowe, soea, sowe, sm, obj), optionally with :X:Y offsets, or fp:X:Y")
	}
	if !hasRest {
		return nil
	}
	xs, ys, ok := strings.Cut(rest, ":")
	_, errX := finite(xs)
	_, errY := finite(ys)
	if !ok || errX != nil || errY != nil {
		return errors.New("gravity offsets must be two numbers: " + code + ":X:Y")
	}
	return nil
}

// formats are the formats the CLI can force. Each was checked against the
// CDN. AVIF is not among them: the CDN drops "f:avif" and picks the format
// from the browser's Accept header instead.
var formats = []string{"webp", "jpg", "png", "gif", "ico", "jxl", "heic"}

// ErrAVIF explains why AVIF cannot be forced.
var ErrAVIF = errors.New("AVIF cannot be forced: the CDN drops f:avif. With auto, browsers that accept AVIF get it")

// Format validates a format. It returns "" for auto, which means no "f:"
// option: the CDN then serves each browser AVIF or WebP when it accepts them.
func Format(v string) (string, error) {
	v = strings.ToLower(v)
	switch v {
	case "auto":
		return "", nil
	case "jpeg":
		return "jpg", nil
	case "avif":
		return "", ErrAVIF
	}
	for _, f := range formats {
		if v == f {
			return v, nil
		}
	}
	return "", fmt.Errorf("must be %s", list(append([]string{"auto"}, formats...)))
}

// DPR validates a pixel density from 1 to 5.
func DPR(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 5 || strings.HasPrefix(v, "+") || strings.HasPrefix(v, "0") {
		return "", errors.New("must be a whole number from 1 to 5")
	}
	return v, nil
}

// checkCacheBuster applies the CDN's rule: it reads the value up to a colon,
// and drops it unless it has at least 3 characters, a dot, and at least 1
// more character (cache tokens from the dashboard look like "aB3d.1f").
func checkCacheBuster(v string) error {
	parts := strings.Split(v, ".")
	if strings.Contains(v, ":") || len(parts) < 2 || len(parts[0]) < 3 || len(parts[1]) < 1 {
		return errors.New(`must look like "abc.1" (3 or more characters, a dot, then more); the CDN ignores any other value`)
	}
	return nil
}

// list joins two or more values as "a, b or c".
func list(values []string) string {
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}
