// Package redact removes secrets, such as the API key, from text before it is
// printed.
package redact

import (
	"net/url"
	"strings"
)

// Placeholder replaces every secret found in the text.
const Placeholder = "[REDACTED]"

// minSecretLength keeps very short values from blanking out ordinary words.
const minSecretLength = 4

// Redactor replaces known secrets with Placeholder. The zero value redacts
// nothing.
type Redactor struct {
	secrets []string
}

// New returns a Redactor for the given secrets. Empty and very short values are
// ignored. Each secret is also redacted in its URL-encoded forms.
func New(secrets ...string) Redactor {
	var r Redactor
	for _, s := range secrets {
		if len(s) < minSecretLength {
			continue
		}
		r.secrets = append(r.secrets, s)
		for _, encoded := range []string{url.QueryEscape(s), url.PathEscape(s)} {
			if encoded != s {
				r.secrets = append(r.secrets, encoded)
			}
		}
	}
	return r
}

// String returns s with every secret replaced.
func (r Redactor) String(s string) string {
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, Placeholder)
	}
	return s
}

// Error returns the redacted text of err, or "" for a nil error.
func (r Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}
