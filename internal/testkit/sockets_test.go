package testkit

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"slices"
	"testing"
)

func TestNotLoopback(t *testing.T) {
	socks := []Socket{
		{"tcp", net.ParseIP("127.0.0.1"), 80},
		{"udp", net.ParseIP("0.0.0.0"), 81},
		{"udp", net.ParseIP("0.0.0.0"), 0}, // not bound yet
		{"tcp6", net.ParseIP("::ffff:127.0.0.1"), 82},
		{"udp6", net.ParseIP("::"), 83},
		{"tcp", net.ParseIP("192.0.2.10"), 84},
		{"udp6", net.ParseIP("::1"), 85},
	}
	var got []int
	for _, s := range notLoopback(socks) {
		got = append(got, s.Port)
	}
	if !slices.Equal(got, []int{81, 83, 84}) {
		t.Fatalf("flagged ports %v, want [81 83 84]", got)
	}
}

func TestParseProcNet(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the samples are from a little-endian host")
	}
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F91 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12346 1 0000000000000000 100 0 0 10 0
`
	got, err := parseProcNet("tcp", []byte(tcp))
	if err != nil {
		t.Fatal(err)
	}
	if s := got["12345"]; s.String() != "tcp 127.0.0.1:8080" {
		t.Errorf("12345: %v", s)
	}
	if s := got["12346"]; s.String() != "tcp 0.0.0.0:8081" {
		t.Errorf("12346: %v", s)
	}
	const udp6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
  1: 00000000000000000000000001000000:6987 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 222 2 0000000000000000 0
  2: 0000000000000000FFFF00000100007F:0050 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 223 2 0000000000000000 0
  3: 00000000000000000000000000000000:0051 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 224 2 0000000000000000 0
`
	got, err = parseProcNet("udp6", []byte(udp6))
	if err != nil {
		t.Fatal(err)
	}
	for inode, want := range map[string]string{"222": "udp6 [::1]:27015", "223": "udp6 127.0.0.1:80", "224": "udp6 [::]:81"} {
		if s := got[inode]; s.String() != want {
			t.Errorf("%s: %v, want %s", inode, s, want)
		}
	}
	if _, err := parseProcNet("tcp", []byte("header\n 0: 7F:1 x x x x x x x 9\n")); err == nil {
		t.Error("a short address was read")
	}
}

// winTable builds a table as GetExtendedTcpTable and GetExtendedUdpTable
// return it.
func winTable(proto string, rows ...Socket) []byte {
	l := winRows[proto]
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
	for i, s := range rows {
		r := make([]byte, l.size)
		ip := s.IP.To16()
		if l.addrLen == 4 {
			ip = s.IP.To4()
		}
		copy(r[l.addr:], ip)
		r[l.port], r[l.port+1] = byte(s.Port>>8), byte(s.Port)
		binary.LittleEndian.PutUint32(r[l.pid:], uint32(40+i%2)) // rows alternate between pid 40 and 41
		b = append(b, r...)
	}
	return b
}

func TestParseWinTable(t *testing.T) {
	for proto, rows := range map[string][]Socket{
		"tcp":  {{"tcp", net.ParseIP("127.0.0.1"), 8740}, {"tcp", net.ParseIP("0.0.0.0"), 1}, {"tcp", net.ParseIP("192.0.2.10"), 443}},
		"tcp6": {{"tcp6", net.ParseIP("::1"), 8740}, {"tcp6", net.ParseIP("::"), 1}, {"tcp6", net.ParseIP("::"), 8741}},
		"udp":  {{"udp", net.ParseIP("127.0.0.1"), 27015}, {"udp", net.ParseIP("0.0.0.0"), 2}, {"udp", net.ParseIP("0.0.0.0"), 7777}},
		"udp6": {{"udp6", net.ParseIP("::1"), 27016}, {"udp6", net.ParseIP("::"), 3}, {"udp6", net.ParseIP("::"), 7778}},
	} {
		got, err := parseWinTable(proto, winTable(proto, rows...), 40)
		if err != nil {
			t.Fatalf("%s: %v", proto, err)
		}
		want := []Socket{rows[0], rows[2]}
		if len(got) != 2 || got[0].String() != want[0].String() || got[1].String() != want[1].String() {
			t.Errorf("%s: %v, want %v", proto, got, want)
		}
	}
	if _, err := parseWinTable("udp", []byte{5, 0, 0, 0, 1, 2}, 40); err == nil {
		t.Error("a table cut short was read")
	}
}

// The sockets of this test process are found, with their loopback
// addresses.
func TestProcessSockets(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	socks, err := ProcessSockets(os.Getpid())
	if errors.Is(err, ErrSocketsUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		Socket{"udp", net.IPv4(127, 0, 0, 1), udp.LocalAddr().(*net.UDPAddr).Port}.String(): false,
		Socket{"tcp", net.IPv4(127, 0, 0, 1), tcp.Addr().(*net.TCPAddr).Port}.String():      false,
	}
	for _, s := range socks {
		if _, ok := want[s.String()]; ok {
			want[s.String()] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("%s not among the sockets of this process: %v", s, socks)
		}
	}
	CheckLoopback(t, os.Getpid())
	if s, err := ProcessSockets(1 << 30); err != nil || len(s) != 0 {
		t.Errorf("a process that does not exist: %v %v", s, err)
	}
}
