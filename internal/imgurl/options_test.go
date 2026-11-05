package imgurl

import (
	"errors"
	"testing"
)

func TestTypedValues(t *testing.T) {
	type check func(string) (string, error)
	tests := []struct {
		name string
		fn   check
		good map[string]string // input -> value written to the URL
		bad  []string
	}{
		{"Size", Size,
			map[string]string{"auto": "auto", "1": "1", "800": "800"},
			[]string{"", "0", "-5", "+5", "05", "1.5", "abc", "AUTO", "99999999999"}},
		{"Quality", Quality,
			map[string]string{"auto": "auto", "mauto": "mauto", "eco": "eco", "1": "1", "100": "100"},
			[]string{"", "0", "101", "080", "+50", "best", "high"}},
		{"ResizeType", ResizeType,
			map[string]string{"fit": "fit", "fill": "fill", "fill-down": "fill-down", "force": "force", "auto": "auto"},
			[]string{"", "crop", "FILL", "cover"}},
		{"Gravity", Gravity,
			map[string]string{"center": "ce", "top": "no", "bottom": "so", "left": "we", "right": "ea",
				"top-left": "nowe", "top-right": "noea", "bottom-left": "sowe", "bottom-right": "soea",
				"smart": "sm", "Top-Left": "nowe", "nowe": "nowe", "ce": "ce"},
			[]string{"", "middle", "north", "fp", "obj"}},
		{"Focus", Focus,
			map[string]string{"0.5:0.2": "fp:0.5:0.2", "0,1": "fp:0:1", "1:0.25": "fp:1:0.25", ".5:.5": "fp:0.5:0.5"},
			[]string{"", "0.5", "1.5:0", "0:-0.1", "a:b", "0.5:0.5:0.5"}},
		{"Format", Format,
			map[string]string{"auto": "", "webp": "webp", "JPG": "jpg", "jpeg": "jpg", "png": "png", "gif": "gif",
				"ico": "ico", "jxl": "jxl", "heic": "heic"},
			[]string{"", "avif", "best", "css", "bmp"}},
		{"DPR", DPR,
			map[string]string{"1": "1", "2": "2", "5": "5"},
			[]string{"", "0", "6", "1.5", "02", "+2"}},
	}
	for _, tt := range tests {
		for in, want := range tt.good {
			if got, err := tt.fn(in); err != nil || got != want {
				t.Errorf("%s(%q) = %q, %v; want %q", tt.name, in, got, err, want)
			}
		}
		for _, in := range tt.bad {
			if got, err := tt.fn(in); err == nil {
				t.Errorf("%s(%q) = %q, want an error", tt.name, in, got)
			}
		}
	}
}

func TestFormatExplainsAVIF(t *testing.T) {
	if _, err := Format("AVIF"); !errors.Is(err, ErrAVIF) {
		t.Fatalf("Format(AVIF) error = %v, want ErrAVIF", err)
	}
}

func TestCheck(t *testing.T) {
	good := [][2]string{
		{"w", "300"}, {"h", "auto"}, {"q", "eco"}, {"rt", "fill-down"}, {"dpr", "2"},
		{"g", "sm"}, {"g", "obj"}, {"g", "ce:10:-5"}, {"g", "fp:0.5:0.5"},
		{"cb", "aB3d.1f"}, {"cb", "abc.d.e"},
		{"bl", "5"}, {"c", "200:100:ce"}, {"bg", "255:255:255"}, {"f", "best"}, {"f", "avif"}, {"ig", "avif"},
		{"att", "1"}, {"no-auto", "1"},
	}
	for _, g := range good {
		if err := Check(g[0], g[1]); err != nil {
			t.Errorf("Check(%q, %q) = %v, want nil", g[0], g[1], err)
		}
	}
	bad := [][2]string{
		{"id", hash}, {"width", "300"}, {"W", "300"}, {"", "x"},
		{"bl", ""}, {"bl", "5/6"}, {"bl", "5 6"}, {"bl", "a?b"}, {"bl", "a#b"}, {"bl", "50%"}, {"bl", "a\x00"},
		{"w", "0"}, {"h", "x"}, {"q", "200"}, {"rt", "crop"}, {"dpr", "9"},
		{"g", "middle"}, {"g", "fp"}, {"g", "fp:2:0"}, {"g", "ce:1"}, {"g", "ce:a:b"},
		{"cb", "abc123xyz789"}, {"cb", "ab.c"}, {"cb", "abc."}, {"cb", ".abc"},
	}
	for _, b := range bad {
		if err := Check(b[0], b[1]); err == nil {
			t.Errorf("Check(%q, %q) = nil, want an error", b[0], b[1])
		}
	}
}

func TestEveryOptionHasOnePlace(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range cdnOptions {
		if seen[n] {
			t.Errorf("%q is listed twice", n)
		}
		seen[n] = true
	}
	if IsOption("id") {
		t.Error(`"id" starts the source and must not be an option`)
	}
}
