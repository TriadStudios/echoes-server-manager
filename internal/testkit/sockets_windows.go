package testkit

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	iphlpapi                = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable = iphlpapi.NewProc("GetExtendedUdpTable")
)

const (
	afInet               = 2
	afInet6              = 23
	tcpTableOwnerPIDAll  = 5 // TCP_TABLE_OWNER_PID_ALL
	udpTableOwnerPID     = 1 // UDP_TABLE_OWNER_PID
	errInsufficientBuf   = 122
	errNotSupported      = 50
	maxTableReadAttempts = 10
)

// processSockets reads the system's TCP and UDP tables (IPv4 and IPv6)
// with their owning process ids and keeps the rows of pid.
func processSockets(pid int) ([]Socket, error) {
	var out []Socket
	for _, t := range []struct {
		proto     string
		proc      *syscall.LazyProc
		af, class uintptr
	}{
		{"tcp", procGetExtendedTcpTable, afInet, tcpTableOwnerPIDAll},
		{"tcp6", procGetExtendedTcpTable, afInet6, tcpTableOwnerPIDAll},
		{"udp", procGetExtendedUdpTable, afInet, udpTableOwnerPID},
		{"udp6", procGetExtendedUdpTable, afInet6, udpTableOwnerPID},
	} {
		b, err := ownerTable(t.proc, t.af, t.class)
		if err != nil {
			return nil, fmt.Errorf("%s table: %w", t.proto, err)
		}
		if b == nil {
			continue // this address family is not there
		}
		socks, err := parseWinTable(t.proto, b, uint32(pid))
		if err != nil {
			return nil, err
		}
		out = append(out, socks...)
	}
	return out, nil
}

// ownerTable calls GetExtendedTcpTable or GetExtendedUdpTable until the
// buffer is big enough.
func ownerTable(proc *syscall.LazyProc, af, class uintptr) ([]byte, error) {
	if err := proc.Find(); err != nil {
		return nil, err
	}
	size := uint32(16 << 10)
	for range maxTableReadAttempts {
		buf := make([]byte, size)
		r, _, _ := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, class, 0)
		switch r {
		case 0:
			return buf, nil
		case errInsufficientBuf:
			size += 4 << 10 // the table may grow before the next call
		case errNotSupported:
			return nil, nil
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, errors.New("the table kept growing")
}
