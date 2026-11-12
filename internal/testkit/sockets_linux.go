package testkit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// processSockets matches the socket inodes among the open files of the
// process with the TCP and UDP tables of its network namespace.
func processSockets(pid int) ([]Socket, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		// Gone; or exiting (its open files can no longer be read) or not
		// ours (its pid was reused): no server of the tests.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(dir, fd.Name()))
		if err != nil {
			continue // closed meanwhile
		}
		if inode, ok := strings.CutPrefix(link, "socket:["); ok {
			inodes[strings.TrimSuffix(inode, "]")] = true
		}
	}
	var out []Socket
	for _, proto := range []string{"tcp", "tcp6", "udp", "udp6"} {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", pid, proto))
		if errors.Is(err, fs.ErrNotExist) {
			continue // gone, or no IPv6
		}
		if err != nil {
			return nil, err
		}
		table, err := parseProcNet(proto, b)
		if err != nil {
			return nil, err
		}
		for inode, s := range table {
			if inodes[inode] {
				out = append(out, s)
			}
		}
	}
	return out, nil
}
