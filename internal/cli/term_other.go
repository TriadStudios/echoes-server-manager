//go:build !linux && !windows

package cli

import (
	"errors"
	"os"
)

// isTerminal reports whether f may be a terminal: on this system, whether
// it is a character device.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// echoOff cannot stop a terminal from showing what is typed on this
// system: the password is then read from standard input only.
func echoOff(f *os.File) (restore func(), err error) {
	return nil, errors.New("esm cannot hide what is typed on this system; give the new password on standard input instead, like: esm user passwd <name> < file")
}
