package service

import (
	"fmt"
	"runtime"
)

// maxSocketPath is the longest unix socket path the system takes, in
// bytes: sun_path holds 108 bytes on Linux and Windows and 104 on macOS
// and the BSDs, the closing NUL included.
var maxSocketPath = func() int {
	switch runtime.GOOS {
	case "linux", "windows":
		return 107
	}
	return 103
}()

// checkSocketPath refuses a socket path longer than the system takes,
// with a message that says so (the system's own error does not).
func checkSocketPath(path string) error {
	if n := len(path); n > maxSocketPath {
		return fmt.Errorf("the socket path %s is %d bytes long; this system allows at most %d: set \"socket\" in esm.json to a shorter path (in a folder only the ESM account can write to), or use a shorter root", path, n, maxSocketPath)
	}
	return nil
}

// socketListened is called right after the socket file is made (tests
// look at its mode there).
var socketListened = func(path string) {}
