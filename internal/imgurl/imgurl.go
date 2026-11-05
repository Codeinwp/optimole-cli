// Package imgurl reads and rewrites Optimole delivery URLs:
//
//	https://HOST/OPTION/OPTION/.../SOURCE
//
// SOURCE is "id:HASH/ORIGIN/FILE" for library assets, or the origin URL
// itself ("https://example.com/a.jpg") for images the CDN fetches from a
// site. OPTION segments are "name:value" (w:800, q:auto, rt:fill).
//
// The package reads a path the way the CDN's viewer-request function does:
// only the first copy of an option counts, and the CDN applies options in a
// fixed order whatever order the path lists them in. String writes them in
// that order, so equal options always give the same URL.
package imgurl

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrNotOptimole is returned for URLs that are not Optimole delivery URLs.
var ErrNotOptimole = errors.New("not an Optimole URL")

// Kind tells images from other files.
type Kind int

const (
	// Image is an image the CDN can transform.
	Image Kind = iota
	// File is any other file: a library file ("/files/" in the URL) or a
	// CSS or JS asset ("f:css", "f:js"). Image options do not apply to it.
	File
)

// Option is one "name:value" path segment. Value keeps any further colons
// ("fp:0.5:0.2" for g).
type Option struct {
	Name  string
	Value string
}

// String returns the path segment.
func (o Option) String() string { return o.Name + ":" + o.Value }

// URL is an Optimole delivery URL split into the parts the CDN reads.
type URL struct {
	// prefix is the scheme and host, e.g. "https://abc.i.optimole.com".
	prefix string
	// options are the path's options in path order. A name can repeat: Get,
	// Set and Options use its first copy, the one the CDN applies, and
	// Unset removes every copy.
	options []Option
	// extra are the other segments before the source, kept as they are:
	// "files" marks a file URL, and the CDN ignores the rest.
	extra []string
	// source is the rest of the path, from "id:" or the origin's scheme.
	source string
	// suffix is the query and fragment, kept as they are.
	suffix string
}

// defaultImageOptions are the options of an image URL from the dashboard.
var defaultImageOptions = []Option{{"w", "auto"}, {"h", "auto"}, {"q", "auto"}}

// Parse splits an Optimole URL. The host must be an Optimole one, or (for
// custom domains) the path must hold an "id:" source or CDN options, or start
// with the origin URL.
func Parse(raw string) (*URL, error) {
	s := strings.TrimSpace(raw)
	// A URL never holds spaces or control characters; a newline would also
	// turn one printed URL into two.
	if strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, fmt.Errorf("%w: it contains spaces or control characters", ErrNotOptimole)
	}
	scheme, rest, ok := cutScheme(s)
	if !ok {
		return nil, fmt.Errorf("%w: it must start with https://", ErrNotOptimole)
	}
	host, path, _ := strings.Cut(rest, "/")
	if host == "" || strings.ContainsAny(host, "@?#\\") {
		return nil, fmt.Errorf("%w: invalid host %q", ErrNotOptimole, host)
	}
	path, suffix := cutSuffix(path)
	// The CDN reads an escaped colon only when the path has no plain one.
	if !strings.Contains(path, ":") {
		path = strings.NewReplacer("%3A", ":", "%3a", ":").Replace(path)
	}
	segs := strings.Split(path, "/")
	start := sourceStart(segs)
	if start < 0 {
		return nil, fmt.Errorf("%w: the path has no id: or origin URL", ErrNotOptimole)
	}
	u := &URL{prefix: scheme + "://" + host, source: strings.Join(segs[start:], "/"), suffix: suffix}
	for _, seg := range segs[:start] {
		if seg == "" {
			continue
		}
		name, value, isOption := strings.Cut(seg, ":")
		if isOption && IsOption(name) {
			u.options = append(u.options, Option{name, value})
			continue
		}
		u.extra = append(u.extra, seg)
	}
	if !isOptimoleHost(host) && start > 0 && len(u.options) == 0 && u.ID() == "" {
		return nil, fmt.Errorf("%w: %s is not an Optimole host and the path has no CDN options", ErrNotOptimole, host)
	}
	return u, nil
}

// cutScheme splits "https://rest" (any case) and returns the scheme in
// lower case.
func cutScheme(s string) (scheme, rest string, ok bool) {
	for _, sc := range []string{"https", "http"} {
		prefix := sc + "://"
		if len(s) > len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
			return sc, s[len(prefix):], true
		}
	}
	return "", "", false
}

// cutSuffix splits the path from its query and fragment.
func cutSuffix(path string) (string, string) {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		return path[:i], path[i:]
	}
	return path, ""
}

// sourceStart returns the index of the first "id:HASH" segment, or of the
// "https:" that starts an origin URL ("https:", "", host), or -1.
func sourceStart(segs []string) int {
	for i, seg := range segs {
		if len(seg) > len("id:") && strings.HasPrefix(seg, "id:") {
			return i
		}
		if (seg == "https:" || seg == "http:") && i+2 < len(segs) && segs[i+1] == "" && segs[i+2] != "" {
			return i
		}
	}
	return -1
}

func isOptimoleHost(host string) bool {
	h := strings.ToLower(host)
	return strings.HasSuffix(h, ".i.optimole.com") || strings.HasSuffix(h, ".optml.cloud")
}

// ID returns the asset hash of an "id:" URL, or "".
func (u *URL) ID() string {
	rest, ok := strings.CutPrefix(u.source, "id:")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// Validate reports a URL the CDN would misread. The CDN looks for options in
// the whole path, the origin's own path included, so a segment there such as
// "w:9" counts as an option unless the URL already sets that option before
// the source.
func (u *URL) Validate() error {
	segs := strings.Split(u.source, "/")
	for _, seg := range segs[1:] {
		name, _, ok := strings.Cut(seg, ":")
		if !ok || !(IsOption(name) || name == "id") {
			continue
		}
		if _, set := u.Get(name); set || (name == "id" && u.ID() != "") {
			continue
		}
		return fmt.Errorf("the origin path has %q, which the CDN reads as an option, so it would misread this URL", seg)
	}
	return nil
}

// Kind tells whether the URL is an image the CDN can transform.
func (u *URL) Kind() Kind {
	for _, e := range u.extra {
		if e == "files" {
			return File
		}
	}
	if f, ok := u.Get("f"); ok && (f == "css" || f == "js") {
		return File
	}
	return Image
}

// Get returns the value of an option.
func (u *URL) Get(name string) (string, bool) {
	for _, o := range u.options {
		if o.Name == name {
			return o.Value, true
		}
	}
	return "", false
}

// Set adds an option or replaces its value.
func (u *URL) Set(name, value string) {
	for i := range u.options {
		if u.options[i].Name == name {
			u.options[i].Value = value
			return
		}
	}
	u.options = append(u.options, Option{name, value})
}

// Unset removes an option.
func (u *URL) Unset(name string) {
	kept := u.options[:0]
	for _, o := range u.options {
		if o.Name != name {
			kept = append(kept, o)
		}
	}
	u.options = kept
}

// Reset replaces the options with those of a dashboard image URL
// (w:auto/h:auto/q:auto).
func (u *URL) Reset() {
	u.options = append([]Option(nil), defaultImageOptions...)
}

// Options returns one option per name, in the order String writes them.
func (u *URL) Options() []Option {
	sorted := make([]Option, 0, len(u.options))
	for _, name := range cdnOptions {
		if v, ok := u.Get(name); ok {
			sorted = append(sorted, Option{name, v})
		}
	}
	return sorted
}

// String returns the URL with its options in the CDN's order, followed by
// the other segments and the source.
func (u *URL) String() string {
	var b strings.Builder
	b.WriteString(u.prefix)
	for _, o := range u.Options() {
		b.WriteByte('/')
		b.WriteString(o.String())
	}
	for _, e := range u.extra {
		b.WriteByte('/')
		b.WriteString(e)
	}
	b.WriteByte('/')
	b.WriteString(u.source)
	b.WriteString(u.suffix)
	return b.String()
}
