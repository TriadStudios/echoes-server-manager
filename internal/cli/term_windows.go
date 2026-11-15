//go:build windows

package cli

import (
	"os"
	"syscall"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableEchoInput is ENABLE_ECHO_INPUT of a console's input mode.
const enableEchoInput = 0x0004

// isTerminal reports whether f is a console.
func isTerminal(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}

func setConsoleMode(h syscall.Handle, mode uint32) error {
	if r, _, err := procSetConsoleMode.Call(uintptr(h), uintptr(mode)); r == 0 {
		return err
	}
	return nil
}

// echoOff stops the console f from showing what is typed, and returns the
// function that turns it back on. The console still reads whole lines
// (with its own editing) and Ctrl-C works as before.
func echoOff(f *os.File) (restore func(), err error) {
	h := syscall.Handle(f.Fd())
	var mode uint32
	if err := syscall.GetConsoleMode(h, &mode); err != nil {
		return nil, err
	}
	if err := setConsoleMode(h, mode&^enableEchoInput); err != nil {
		return nil, err
	}
	return func() { setConsoleMode(h, mode) }, nil
}
