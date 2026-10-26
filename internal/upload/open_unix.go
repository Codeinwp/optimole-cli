//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package upload

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// fileIdentity returns the device and inode of a file.
func fileIdentity(info fs.FileInfo) (any, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, true
}

// openNoFollow opens a file for reading without following a symlink in its
// last component. O_NONBLOCK keeps a FIFO swapped in from blocking the open;
// reads from regular files ignore it.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// isSymlinkError reports the error O_NOFOLLOW gives for a symlink (ELOOP;
// EMLINK on FreeBSD and DragonFly, EFTYPE on NetBSD).
func isSymlinkError(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) || isEFTYPE(err)
}
