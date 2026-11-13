package doctor

import "syscall"

// writable reports whether this user may write to dir.
func writable(dir string) (ok, known bool) {
	const wOK = 2
	return syscall.Access(dir, wOK) == nil, true
}
