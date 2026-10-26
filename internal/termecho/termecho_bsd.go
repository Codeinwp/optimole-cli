//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package termecho

import "syscall"

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
