// Package termecho hides what is typed on a terminal while a secret is read,
// using only the standard library.
package termecho

import (
	"errors"
	"os"
)

// ErrUnsupported means echo cannot be turned off on this platform.
var ErrUnsupported = errors.New("hiding terminal input is not supported on this platform")

// IsTerminal reports whether f is an interactive terminal.
func IsTerminal(f *os.File) bool {
	return isTerminal(f)
}

// Disable turns echo off on the terminal f and returns a function that
// restores the previous state.
func Disable(f *os.File) (restore func(), err error) {
	return disable(f)
}
