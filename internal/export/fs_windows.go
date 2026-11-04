//go:build windows

package export

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// noFollow is not available: callers check with Lstat before opening.
const noFollow = 0

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
)

var procLockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

// lockFile takes an exclusive lock on f, released when f is closed (or the
// process ends). It fails with errLocked when another handle holds it.
func lockFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return errLocked
	}
	return err
}
