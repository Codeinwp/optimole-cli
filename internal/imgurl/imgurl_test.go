package imgurl

import (
	"errors"
	"reflect"
	"testing"
)

const (
	hash     = "42cf6621995747f69a30d32f6a6c4f82"
	libImage = "https://mlabc.i.optimole.com/w:auto/h:auto/q:auto/id:" + hash + "/directUpload/photo.jpg"
	libFile  = "https://mlabc.i.optimole.com/files/id:" + hash + "/directUpload/notes.pdf"
)

func mustParse(t *testing.T, raw string) *URL {
	t.Helper()
	u, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q): %v", raw, err)
	}
	return u
}

func TestParseKeepsDashboardURLs(t *testing.T) {
	for _, raw := range []string{
		libImage,
		libFile,
		"https://mlabc.i.optimole.com/w:auto/h:auto/q:auto/id:" + hash + "/https://example.com/a.jpg",
		"https://mlabc.b.optml.cloud/w:auto/h:auto/q:auto/id:" + hash + "/directUpload/photo.jpg",
	} {
		if got := mustParse(t, raw).String(); got != raw {
			t.Errorf("String() = %q, want the input %q", got, raw)
		}
	}
}

func TestParseReadsPartsLikeTheCDN(t *testing.T) {
	tests := []struct {
		name, raw string
		options   []Option
		id        string
		kind      Kind
		out       string
	}{
		{
			name:    "library image",
			raw:     libImage,
			options: []Option{{"w", "auto"}, {"h", "auto"}, {"q", "auto"}},
			id:      hash,
			kind:    Image,
			out:     libImage,
		},
		{
			name: "library file",
			raw:  libFile,
			id:   hash,
			kind: File,
			out:  libFile,
		},
		{
			name:    "WordPress plugin URL: options sorted into the CDN's order",
			raw:     "https://mlabc.i.optimole.com/cb:aB3d.1f/w:800/h:600/f:best/ig:avif/q:mauto/rt:fill/g:ce/https://example.com/wp-content/uploads/a.jpg",
			options: []Option{{"cb", "aB3d.1f"}, {"w", "800"}, {"h", "600"}, {"q", "mauto"}, {"rt", "fill"}, {"g", "ce"}, {"f", "best"}, {"ig", "avif"}},
			kind:    Image,
			out:     "https://mlabc.i.optimole.com/cb:aB3d.1f/w:800/h:600/q:mauto/rt:fill/g:ce/f:best/ig:avif/https://example.com/wp-content/uploads/a.jpg",
		},
		{
			name:    "a repeated option: the first copy counts, as on the CDN",
			raw:     "https://mlabc.i.optimole.com/w:800/w:400/h:auto/id:" + hash + "/directUpload/a.jpg",
			options: []Option{{"w", "800"}, {"h", "auto"}},
			id:      hash,
			kind:    Image,
			out:     "https://mlabc.i.optimole.com/w:800/h:auto/id:" + hash + "/directUpload/a.jpg",
		},
		{
			name:    "segments the CDN does not read are kept after the options",
			raw:     "https://mlabc.i.optimole.com/wdth:300/w:200/extra/id:" + hash + "/directUpload/a.jpg",
			options: []Option{{"w", "200"}},
			id:      hash,
			kind:    Image,
			out:     "https://mlabc.i.optimole.com/w:200/wdth:300/extra/id:" + hash + "/directUpload/a.jpg",
		},
		{
			name:    "escaped colons are read when the path has no plain one",
			raw:     "https://mlabc.i.optimole.com/w%3A300/id%3A" + hash + "/directUpload/a.jpg",
			options: []Option{{"w", "300"}},
			id:      hash,
			kind:    Image,
			out:     "https://mlabc.i.optimole.com/w:300/id:" + hash + "/directUpload/a.jpg",
		},
		{
			name:    "query and fragment are kept; the origin's own path is untouched",
			raw:     "https://mlabc.i.optimole.com/w:300/https://example.com/w:9/a.jpg?ver=2#top",
			options: []Option{{"w", "300"}},
			kind:    Image,
			out:     "https://mlabc.i.optimole.com/w:300/https://example.com/w:9/a.jpg?ver=2#top",
		},
		{
			name:    "a CSS asset is a file",
			raw:     "https://mlabc.i.optimole.com/cb:aB3d.1f/f:css/q:mauto/m:0/https://example.com/style.css",
			options: []Option{{"cb", "aB3d.1f"}, {"q", "mauto"}, {"f", "css"}, {"m", "0"}},
			kind:    File,
			out:     "https://mlabc.i.optimole.com/cb:aB3d.1f/q:mauto/f:css/m:0/https://example.com/style.css",
		},
		{
			name:    "a custom domain with an id: source",
			raw:     "https://images.example.com/w:auto/h:auto/q:auto/id:" + hash + "/directUpload/a.jpg",
			options: []Option{{"w", "auto"}, {"h", "auto"}, {"q", "auto"}},
			id:      hash,
			kind:    Image,
			out:     "https://images.example.com/w:auto/h:auto/q:auto/id:" + hash + "/directUpload/a.jpg",
		},
		{
			name:    "a custom domain with CDN options and an origin URL",
			raw:     "https://cdn.example.com/w:640/https://example.com/a.jpg",
			options: []Option{{"w", "640"}},
			kind:    Image,
			out:     "https://cdn.example.com/w:640/https://example.com/a.jpg",
		},
		{
			name:    "before an origin URL, segments the CDN removes are dropped",
			raw:     "https://cdn.example.com/w:640/extra/files/https://example.com/a.jpg",
			options: []Option{{"w", "640"}},
			kind:    File,
			out:     "https://cdn.example.com/w:640/files/https://example.com/a.jpg",
		},
		{
			name: "a custom domain whose path starts with the origin URL",
			raw:  "https://cdn.example.com/https://example.com/a.jpg",
			kind: Image,
			out:  "https://cdn.example.com/https://example.com/a.jpg",
		},
		{
			name: "upper-case scheme",
			raw:  "HTTPS://mlabc.i.optimole.com/id:" + hash + "/directUpload/a.jpg",
			id:   hash,
			kind: Image,
			out:  "https://mlabc.i.optimole.com/id:" + hash + "/directUpload/a.jpg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := mustParse(t, tt.raw)
			if got := u.Options(); len(got)+len(tt.options) > 0 && !reflect.DeepEqual(got, tt.options) {
				t.Errorf("Options() = %v, want %v", got, tt.options)
			}
			if u.ID() != tt.id {
				t.Errorf("ID() = %q, want %q", u.ID(), tt.id)
			}
			if u.Kind() != tt.kind {
				t.Errorf("Kind() = %v, want %v", u.Kind(), tt.kind)
			}
			if s := u.String(); s != tt.out {
				t.Errorf("String() = %q, want %q", s, tt.out)
			}
		})
	}
}

func TestParseRefusesOtherURLs(t *testing.T) {
	for _, raw := range []string{
		"",
		hash,
		"ftp://mlabc.i.optimole.com/id:" + hash + "/a.jpg",
		"https://",
		"https://user@mlabc.i.optimole.com/id:" + hash + "/a.jpg",
		"https://mlabc.i.optimole.com/w:300/a.jpg",
		"https://mlabc.i.optimole.com/w:300/https:/example.com/a.jpg",
		"https://mlabc.i.optimole.com/id:/directUpload/a.jpg",
		"https://web.archive.org/web/2020/https://example.com/a.jpg",
		"https://example.com/images/https://example.org/a.jpg",
		// Spaces and control characters: a newline would print two URLs.
		"https://mlabc.i.optimole.com/w:100/https://example.com/a.jpg\nhttps://evil.example/x",
		"https://mlabc.i.optimole.com/w:100/https://example.com/a b.jpg",
		"https://mlabc.i.optimole.com/w:100/https://example.com/a\tb.jpg",
		"https://mlabc.i.optimole.com/w:100/https://example.com/a\x1b[31m.jpg",
	} {
		if u, err := Parse(raw); !errors.Is(err, ErrNotOptimole) {
			t.Errorf("Parse(%q) = %v, %v; want ErrNotOptimole", raw, u, err)
		}
	}
}

func TestSetReplacesInPlace(t *testing.T) {
	u := mustParse(t, libImage)
	u.Set("w", "300")
	u.Set("f", "webp")
	want := "https://mlabc.i.optimole.com/w:300/h:auto/q:auto/f:webp/id:" + hash + "/directUpload/photo.jpg"
	if got := u.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	// Setting it again replaces the value, never adds a second copy (the CDN
	// would keep the first).
	u.Set("w", "200")
	if got, _ := u.Get("w"); got != "200" || len(u.Options()) != 4 {
		t.Fatalf("after a second Set: w=%q, options %v", got, u.Options())
	}
}

func TestUnsetAndReset(t *testing.T) {
	u := mustParse(t, "https://mlabc.i.optimole.com/w:300/h:200/q:60/rt:fill/f:webp/id:"+hash+"/directUpload/a.jpg")
	u.Unset("f")
	u.Unset("missing")
	if got, want := u.String(), "https://mlabc.i.optimole.com/w:300/h:200/q:60/rt:fill/id:"+hash+"/directUpload/a.jpg"; got != want {
		t.Fatalf("after Unset: %q, want %q", got, want)
	}
	u.Reset()
	if got, want := u.String(), "https://mlabc.i.optimole.com/w:auto/h:auto/q:auto/id:"+hash+"/directUpload/a.jpg"; got != want {
		t.Fatalf("after Reset: %q, want %q", got, want)
	}
	// Reset gives the URL its own options: changing them leaves the defaults
	// alone for the next URL.
	u.Set("w", "10")
	v := mustParse(t, libImage)
	v.Reset()
	if got, _ := v.Get("w"); got != "auto" {
		t.Fatalf("defaults changed by an earlier Set: w=%q", got)
	}
}

func TestValidateFindsOptionsTheCDNReadsInTheOriginPath(t *testing.T) {
	tests := []struct {
		raw string
		bad bool
	}{
		{"https://mlabc.i.optimole.com/w:300/https://example.com/w:9/a.jpg", false}, // w is set: the CDN ignores w:9
		{"https://mlabc.i.optimole.com/q:60/https://example.com/w:9/a.jpg", true},
		{"https://mlabc.i.optimole.com/w:300/id:" + hash + "/https://example.com/bl:9/a.jpg", true},
		{"https://mlabc.i.optimole.com/w:300/https://example.com/id:9/a.jpg", true},
		{"https://mlabc.i.optimole.com/w:300/id:" + hash + "/directUpload/id:x/a.jpg", false},
		{"https://mlabc.i.optimole.com/w:300/https://example.com:8443/x:9/a.jpg", false},
		// The CDN reads no "ig" value but avif, and deletes the first
		// "/ig:avif" and "/f:avif" in the path.
		{"https://mlabc.i.optimole.com/w:300/https://example.com/uploads/ig:other/a.jpg", false},
		{"https://mlabc.i.optimole.com/w:300/https://example.com/uploads/ig:avif/a.jpg", true},
		{"https://mlabc.i.optimole.com/w:300/ig:avif/https://example.com/uploads/ig:avif/a.jpg", false},
		{"https://mlabc.i.optimole.com/f:best/https://example.com/f:avif/a.jpg", true},
		{"https://mlabc.i.optimole.com/f:avif/https://example.com/f:avif/a.jpg", false},
		{libImage, false},
	}
	for _, tt := range tests {
		err := mustParse(t, tt.raw).Validate()
		if (err != nil) != tt.bad {
			t.Errorf("Validate(%q) = %v, want an error: %v", tt.raw, err, tt.bad)
		}
	}
	// A custom-domain URL left without options still parses back.
	c := mustParse(t, "https://cdn.example.com/w:640/extra/https://example.com/a.jpg")
	c.Unset("w")
	if got := mustParse(t, c.String()).String(); got != "https://cdn.example.com/https://example.com/a.jpg" {
		t.Fatalf("round trip = %q", got)
	}
	// Removing the option that shadowed it makes the URL one the CDN misreads.
	u := mustParse(t, "https://mlabc.i.optimole.com/w:300/https://example.com/w:9/a.jpg")
	u.Unset("w")
	if u.Validate() == nil {
		t.Fatal("Validate accepted a URL whose only w: is in the origin path")
	}
}

func TestStringReparsesToTheSameURL(t *testing.T) {
	for _, raw := range []string{
		libImage,
		"https://mlabc.i.optimole.com/q:60/zz/w:5/w:6/files/https://example.com/a.css?x=1",
		"https://cdn.example.com/g:fp:0.5:0.1/rt:fill/https://example.com/a.jpg",
	} {
		first := mustParse(t, raw).String()
		if second := mustParse(t, first).String(); second != first {
			t.Errorf("String() is not stable: %q then %q", first, second)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		libImage,
		libFile,
		"https://mlabc.i.optimole.com/cb:aB3d.1f/w:800/f:best/ig:avif/https://example.com/a.jpg?v=1",
		"https://cdn.example.com/w%3A1/id%3Ax/y",
		"https://x.optml.cloud//w:1//id:a/b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := Parse(raw)
		if err != nil {
			return
		}
		s := u.String()
		v, err := Parse(s)
		if err != nil {
			t.Fatalf("String() of %q is %q, which does not parse: %v", raw, s, err)
		}
		if v.String() != s {
			t.Fatalf("not stable: %q -> %q -> %q", raw, s, v.String())
		}
		if !reflect.DeepEqual(u.Options(), v.Options()) || u.ID() != v.ID() || u.Kind() != v.Kind() {
			t.Fatalf("reparsing %q changed it: %v %q %v -> %v %q %v", s, u.Options(), u.ID(), u.Kind(), v.Options(), v.ID(), v.Kind())
		}
		// An edited URL parses back with the edits.
		v.Set("w", "123")
		v.Unset("q")
		e, err := Parse(v.String())
		if err != nil {
			t.Fatalf("the edited %q does not parse: %v", v.String(), err)
		}
		if w, _ := e.Get("w"); w != "123" {
			t.Fatalf("edit lost: %q has w=%q", v.String(), w)
		}
		if _, ok := e.Get("q"); ok {
			t.Fatalf("edit lost: %q still has q", v.String())
		}
	})
}
