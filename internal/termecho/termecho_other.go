//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package termecho

import "os"

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func disable(*os.File) (func(), error) {
	return nil, ErrUnsupported
}
