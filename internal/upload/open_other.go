//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package upload

import (
	"errors"
	"io/fs"
	"os"
)

var errSymlink = errors.New("symbolic link")

// fileIdentity is not available here; os.SameFile is used instead.
func fileIdentity(fs.FileInfo) (any, bool) { return nil, false }

// openNoFollow refuses a symlink or non-regular file before opening it. The
// caller compares the opened file with the planned one, which closes the
// gap between this check and the open.
func openNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, errSymlink
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	return os.Open(path)
}

func isSymlinkError(err error) bool { return errors.Is(err, errSymlink) }
