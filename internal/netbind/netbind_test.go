package netbind

import (
	"errors"
	"net"
	"slices"
	"strconv"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.1.2.3": true, "::1": true, "localhost": true, "LocalHost": true,
		"": false, "192.0.2.10": false, "2001:db8::1": false, "example.com": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

// In a test binary only loopback addresses are bound; everything else,
// all interfaces included, is refused before any socket is opened, and
// listed by Refused.
func TestTestBinariesBindLoopbackOnly(t *testing.T) {
	if !testing.Testing() {
		t.Fatal("testing.Testing() is false in a test binary")
	}
	before := len(Refused())
	if err := PortFree("", 1); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("all interfaces: %v", err)
	}
	if err := PortFree("192.0.2.10", 2); !errors.Is(err, ErrNotLoopback) {
		t.Fatalf("another address: %v", err)
	}
	if l, err := Listen("tcp", ":3"); !errors.Is(err, ErrNotLoopback) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("listen on all interfaces: %v", err)
	}
	got := Refused()[before:]
	if want := []string{"0.0.0.0:1", "192.0.2.10:2", ":3"}; !slices.Equal(got, want) {
		t.Fatalf("Refused() = %q, want %q", got, want)
	}
	// Loopback is fine, and the refusals stay listed.
	l, err := Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if n := len(Refused()); n != before+3 {
		t.Fatalf("%d refusals, want %d", n, before+3)
	}
}

// PortFree finds a port busy when either UDP or TCP is taken.
func TestPortFree(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if PortFree("127.0.0.1", udp.LocalAddr().(*net.UDPAddr).Port) == nil {
		t.Fatal("a port taken for UDP is free")
	}
	// A port the system picks for TCP may be one it keeps for itself for
	// UDP (Windows reserves ranges of ports); such a port is skipped.
	for range 20 {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		if PortFree("127.0.0.1", port) == nil {
			t.Fatal("a port taken for TCP is free")
		}
		tcp.Close()
		if !udpBindable(port) {
			continue
		}
		if err := PortFree("127.0.0.1", port); err != nil {
			t.Fatalf("a closed port is busy: %v", err)
		}
		return
	}
	t.Skip("no port the system picked for TCP was free for UDP too")
}

// udpBindable reports whether UDP port can be bound on 127.0.0.1 now.
func udpBindable(port int) bool {
	c, err := net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}
