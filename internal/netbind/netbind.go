// Package netbind is where ESM binds its own network sockets: the TCP
// listener of the API, and the short test-binds that find out whether a
// game or query port is free.
//
// In a test binary (see testing.Testing) it binds loopback addresses only.
// Any other address, all interfaces included, is refused without binding
// it, and remembered (see Refused) so the test run can fail on it. A test
// binary that binds all interfaces makes a desktop firewall ask whether it
// may, and test binaries get a new path with every build, so the question
// would come back with every run. Tests bind 127.0.0.1.
package netbind

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ErrNotLoopback is the refusal of an address that is not a loopback
// address, in a test binary.
var ErrNotLoopback = errors.New("test binaries bind loopback addresses only")

var (
	mu      sync.Mutex
	refused []string
)

// IsLoopback reports whether host, an IP address or "localhost", names the
// loopback interface (127.0.0.0/8 or ::1). An empty host means all
// interfaces and is not loopback.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// allow refuses a host that is not loopback in a test binary.
func allow(host, addr string) error {
	if IsLoopback(host) || !testing.Testing() {
		return nil
	}
	mu.Lock()
	refused = append(refused, addr)
	mu.Unlock()
	return fmt.Errorf("netbind: %w (refused %s)", ErrNotLoopback, addr)
}

// Refused lists the addresses refused in this test binary so far, in the
// order they were asked for.
func Refused() []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(refused)
}

// PortFree reports whether port can be bound now on ip, for UDP and for
// TCP: it binds both and closes them again at once. An empty ip means all
// interfaces, which is what a server without a bind address uses.
func PortFree(ip string, port int) error {
	host := ip
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if err := allow(host, addr); err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	pc.Close()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	l.Close()
	return nil
}

// Listen is net.Listen for ESM's own TCP listener ("tcp", "tcp4" or
// "tcp6"); other networks are passed through as they are.
func Listen(network, addr string) (net.Listener, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if err := allow(host, addr); err != nil {
			return nil, err
		}
	}
	return net.Listen(network, addr)
}
