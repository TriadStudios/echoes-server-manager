//go:build linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

func termios(fd uintptr, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return termios(f.Fd(), syscall.TCGETS, &t) == nil
}

// echoOff stops the terminal f from showing what is typed, and returns the
// function that turns it back on. Lines and Ctrl-C work as before.
func echoOff(f *os.File) (restore func(), err error) {
	fd := f.Fd()
	var old syscall.Termios
	if err := termios(fd, syscall.TCGETS, &old); err != nil {
		return nil, err
	}
	t := old
	t.Lflag &^= syscall.ECHO
	t.Lflag |= syscall.ICANON | syscall.ISIG
	t.Iflag |= syscall.ICRNL
	if err := termios(fd, syscall.TCSETS, &t); err != nil {
		return nil, err
	}
	return func() { termios(fd, syscall.TCSETS, &old) }, nil
}
