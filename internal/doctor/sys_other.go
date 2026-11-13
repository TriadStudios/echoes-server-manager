//go:build !linux && !windows

package doctor

func writable(string) (ok, known bool) { return true, false }
