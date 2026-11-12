package doctor

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func find(r Report, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestRunReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	r := Run(Options{Root: root, PortHost: "127.0.0.1"})
	if c := find(r, "root"); c.Status != Warn {
		t.Fatalf("missing root: %+v", c)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("doctor created the root")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	busy, err := net.ListenPacket("udp", "127.0.0.1:26001")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := `{"listen":"0.0.0.0:8740","game_ports":"26000-26002","query_ports":"26010-26012"}`
	if err := os.WriteFile(filepath.Join(root, "esm.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r = Run(Options{Root: root, PortHost: "127.0.0.1", UsedPorts: map[int]bool{26000: true}})
	if c := find(r, "listen"); c.Status != Fail || !strings.Contains(c.Message, "TLS") {
		t.Fatalf("listen rule: %+v", c)
	}
	if c := find(r, "game_ports"); !strings.Contains(c.Message, "1 free, 1 used by instances, 1 busy") {
		t.Fatalf("game ports: %+v", c)
	}
	if r.Problems == 0 {
		t.Fatal("no problems counted")
	}
	if c := find(r, "disk"); c.Status == "" {
		t.Fatal("no disk check")
	}
	// A socket file nobody answers on is stale.
	if err := os.WriteFile(filepath.Join(root, "esm.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r = Run(Options{Root: root, PortHost: "127.0.0.1"})
	if c := find(r, "socket"); c.Status != Warn || !strings.Contains(c.Message, "stale") {
		t.Fatalf("stale socket: %+v", c)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatalf("doctor changed the root: %v", entries)
	}
}
