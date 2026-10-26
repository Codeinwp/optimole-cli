//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package termecho

import (
	"os"
	"syscall"
	"unsafe"
)

func getTermios(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, errno
	}
	return &t, nil
}

func setTermios(fd uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}

func isTerminal(f *os.File) bool {
	_, err := getTermios(f.Fd())
	return err == nil
}

func disable(f *os.File) (func(), error) {
	fd := f.Fd()
	old, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	quiet := *old
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	if err := setTermios(fd, &quiet); err != nil {
		return nil, err
	}
	return func() { _ = setTermios(fd, old) }, nil
}
