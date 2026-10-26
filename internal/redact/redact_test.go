package redact

import (
	"errors"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := New("optml-s+secret/key", "", "ab")
	cases := map[string]string{
		"key optml-s+secret/key leaked":       "key [REDACTED] leaked",
		"query ?k=optml-s%2Bsecret%2Fkey":     "query ?k=[REDACTED]",
		"path /optml-s+secret%2Fkey/x":        "path /[REDACTED]/x",
		"short values like ab are left alone": "short values like ab are left alone",
	}
	for in, want := range cases {
		if got := r.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
	if got := r.Error(errors.New("bad optml-s+secret/key")); got != "bad [REDACTED]" {
		t.Errorf("Error = %q", got)
	}
	if got := r.Error(nil); got != "" {
		t.Errorf("Error(nil) = %q", got)
	}
	var zero Redactor
	if got := zero.String("anything"); got != "anything" {
		t.Errorf("zero Redactor changed text: %q", got)
	}
}
