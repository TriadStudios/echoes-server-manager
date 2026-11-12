package testkit

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Socket is a TCP or UDP socket of a process and its local address.
type Socket struct {
	Proto string // "tcp", "tcp6", "udp" or "udp6"
	IP    net.IP
	Port  int
}

func (s Socket) String() string {
	return s.Proto + " " + net.JoinHostPort(s.IP.String(), strconv.Itoa(s.Port))
}

// ErrSocketsUnsupported means ProcessSockets does not work on this system.
var ErrSocketsUnsupported = errors.New("testkit: listing the sockets of a process is not supported on " + runtime.GOOS)

// ProcessSockets lists the TCP and UDP sockets of process pid with their
// local addresses: from /proc on Linux, from the system's TCP and UDP
// tables on Windows. A process that is gone has none.
func ProcessSockets(pid int) ([]Socket, error) { return processSockets(pid) }

// CheckLoopback fails the test when one of the processes has a TCP or UDP
// socket on an address that is not loopback, all interfaces included.
// Tests bind 127.0.0.1 only: a test binary or a fake server listening on
// another address makes a desktop firewall ask about it, and test binaries
// get a new path with every build. Sockets that are not bound yet (port 0)
// are left out. On a system where ProcessSockets does not work, the check
// is skipped with a log line.
func CheckLoopback(t testing.TB, pids ...int) {
	t.Helper()
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		socks, err := ProcessSockets(pid)
		if errors.Is(err, ErrSocketsUnsupported) {
			t.Logf("loopback check skipped: %v", err)
			return
		}
		if err != nil {
			t.Errorf("sockets of process %d: %v", pid, err)
			continue
		}
		for _, s := range notLoopback(socks) {
			t.Errorf("process %d has the socket %s, which is not on loopback: tests must bind 127.0.0.1 only", pid, s)
		}
	}
}

// notLoopback returns the bound sockets that are not on loopback.
func notLoopback(socks []Socket) []Socket {
	var out []Socket
	for _, s := range socks {
		if s.Port != 0 && !s.IP.IsLoopback() {
			out = append(out, s)
		}
	}
	return out
}

// parseProcNet reads a Linux /proc/<pid>/net/{tcp,tcp6,udp,udp6} table
// into its sockets by inode. Addresses there are hex words in the host's
// byte order, ports hex numbers.
func parseProcNet(proto string, b []byte) (map[string]Socket, error) {
	out := map[string]Socket{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	first := true
	for sc.Scan() {
		if first { // the header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		ip, port, err := procAddr(f[1])
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", proto, f[1], err)
		}
		out[f[9]] = Socket{Proto: proto, IP: ip, Port: port}
	}
	return out, sc.Err()
}

func procAddr(s string) (net.IP, int, error) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return nil, 0, errors.New("no port")
	}
	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != net.IPv4len && len(raw) != net.IPv6len) {
		return nil, 0, errors.New("bad address")
	}
	ip := make(net.IP, len(raw))
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(ip[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return nil, 0, errors.New("bad port")
	}
	return ip, int(port), nil
}

// winRow is the layout of one row of a Windows owner-pid table
// (MIB_TCPROW_OWNER_PID, MIB_TCP6ROW_OWNER_PID, MIB_UDPROW_OWNER_PID,
// MIB_UDP6ROW_OWNER_PID): its size and the offsets of the local address,
// the local port (network byte order) and the owning pid.
type winRow struct {
	size, addr, addrLen, port, pid int
}

var winRows = map[string]winRow{
	"tcp":  {size: 24, addr: 4, addrLen: 4, port: 8, pid: 20},
	"tcp6": {size: 56, addr: 0, addrLen: 16, port: 20, pid: 52},
	"udp":  {size: 12, addr: 0, addrLen: 4, port: 4, pid: 8},
	"udp6": {size: 28, addr: 0, addrLen: 16, port: 20, pid: 24},
}

// parseWinTable reads a table from GetExtendedTcpTable or
// GetExtendedUdpTable (a row count, then the rows) and returns the sockets
// owned by pid.
func parseWinTable(proto string, b []byte, pid uint32) ([]Socket, error) {
	row, ok := winRows[proto]
	if !ok {
		return nil, fmt.Errorf("unknown table %q", proto)
	}
	if len(b) < 4 {
		return nil, fmt.Errorf("%s table: %d bytes", proto, len(b))
	}
	n := int(binary.LittleEndian.Uint32(b))
	if 4+n*row.size > len(b) {
		return nil, fmt.Errorf("%s table: %d rows do not fit in %d bytes", proto, n, len(b))
	}
	var out []Socket
	for i := range n {
		r := b[4+i*row.size : 4+(i+1)*row.size]
		if binary.LittleEndian.Uint32(r[row.pid:]) != pid {
			continue
		}
		ip := make(net.IP, row.addrLen)
		copy(ip, r[row.addr:row.addr+row.addrLen])
		out = append(out, Socket{Proto: proto, IP: ip, Port: int(r[row.port])<<8 | int(r[row.port+1])})
	}
	return out, nil
}
