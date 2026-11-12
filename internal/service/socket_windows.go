package service

import "net"

// checkSocketDir has nothing to check on Windows: folder permissions are
// ACLs there, which ESM does not set or read (a known limit).
func checkSocketDir(dir string) error { return nil }

// listenUnix listens on the unix socket path.
func listenUnix(path string) (net.Listener, error) {
	return net.Listen("unix", path)
}
