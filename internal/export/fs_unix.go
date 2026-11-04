//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package export

import (
	"errors"
	"os"
	"syscall"
)

// noFollow makes an open fail on a symlink in the last path component.
const noFollow = syscall.O_NOFOLLOW

// lockFile takes an exclusive lock on f, released when f is closed (or the
// process ends). It fails with errLocked when another process holds it.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
