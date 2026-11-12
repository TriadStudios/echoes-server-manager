package doctor

import "syscall"

func freeBytes(dir string) (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, err
	}
	return s.Bavail * uint64(s.Bsize), nil
}

// writable reports whether this user may write to dir.
func writable(dir string) (ok, known bool) {
	const wOK = 2
	return syscall.Access(dir, wOK) == nil, true
}
