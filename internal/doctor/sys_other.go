//go:build !linux && !windows

package doctor

import "errors"

func freeBytes(string) (uint64, error) {
	return 0, errors.New("not supported on this system")
}

func writable(string) (ok, known bool) { return true, false }
