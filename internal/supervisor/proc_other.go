//go:build !linux && !windows

package supervisor

import (
	"errors"
	"os"
	"syscall"
)

const (
	processSupported = false
	signalsAvailable = true
)

var errUnsupported = errors.New("supervisor: process mode is only supported on Linux and Windows")

func detachedAttr() *syscall.SysProcAttr { return nil }

func interrupt(*os.Process) error { return errUnsupported }

func textBusy(error) bool { return false }

func startMarker(int) (string, error) { return "", errUnsupported }

func inspect(int) (bool, string, error) { return false, "", errUnsupported }

func sameExecutable(int, string) bool { return false }
