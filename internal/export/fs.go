package export

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// lockName is the file a run locks in the export folder, so two commands
// never write the same archives or manifest at once.
const lockName = ".optimole-export.lock"

// errLocked means another command holds the export folder's lock.
var errLocked = errors.New("another optimole command is downloading into this folder")

// lockDir creates dir as a plain folder and locks it for this process. The
// lock goes with the returned func, or with the process.
func lockDir(dir string) (func(), error) {
	if err := realDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, lockName)
	if err := clearLink(path); err != nil {
		return nil, &writeError{err}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollow, 0o644)
	if err != nil {
		return nil, &writeError{err}
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return func() { f.Close() }, nil
}

// realDir creates dir and its parents, and checks that dir is a plain folder:
// a symlink or another reparse point planted there would send the archives
// somewhere else. Lstat reports those as links or irregular files, never as
// folders.
func realDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &writeError{err}
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return &writeError{err}
	}
	if !st.IsDir() {
		return &writeError{fmt.Errorf("%s is not a plain folder (a link?); refusing to write through it", dir)}
	}
	return nil
}

// clearLink removes path when it is a link or another non-regular file, so
// the open that follows creates a plain file; a link's target is left alone.
func clearLink(path string) error {
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case st.Mode().IsRegular():
		return nil
	}
	return os.Remove(path)
}
