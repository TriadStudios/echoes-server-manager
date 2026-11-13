//go:build unix

package service

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// checkSocketDir refuses a socket folder that other accounts may write
// to: they could remove the socket, or put their own in its place.
func checkSocketDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("other accounts can write to the socket folder %s (mode %04o): make it writable by the ESM account only (chmod go-w), or set \"socket\" in esm.json to a path in such a folder", dir, perm)
	}
	return nil
}

// listenUnix listens on the unix socket path. The socket file is made
// under a umask that leaves it to its owner, so it is never open to other
// accounts, not even until its mode is set. (The umask is the process's;
// for that moment anything else ESM makes is only stricter, never looser.)
func listenUnix(path string) (net.Listener, error) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}
