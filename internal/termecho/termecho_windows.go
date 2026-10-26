//go:build windows

package termecho

import (
	"os"
	"syscall"
)

const enableEchoInput = 0x0004

var procSetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

func setConsoleMode(h syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

func disable(f *os.File) (func(), error) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if err := setConsoleMode(h, mode&^enableEchoInput); err != nil {
		return nil, err
	}
	return func() { _ = setConsoleMode(h, mode) }, nil
}
