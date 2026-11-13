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
	cfg := `{"listen":"192.0.2.1:8740","game_ports":"26000-26002","query_ports":"26010-26012"}`
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

// Instances that pass their passwords on the server command line are
// reported, by name.
func TestSecretDeliveryCheck(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if c := find(Run(Options{Root: root, PortHost: "127.0.0.1"}), "secret_delivery"); c.Status != OK {
		t.Fatalf("a root without instances: %+v", c)
	}
	for name, body := range map[string]string{
		"alpha": `{"name":"alpha","secret_delivery":"file"}`,
		"beta":  `{"name":"beta","secret_delivery":"command_line"}`,
		"gamma": `{"name":"gamma","secret_delivery":"command_line"}`,
		"older": `{"name":"older"}`,
		"odd":   `not json`,
	} {
		dir := filepath.Join(root, "instances", name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "instance.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "instances", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := Run(Options{Root: root, PortHost: "127.0.0.1"})
	c := find(r, "secret_delivery")
	if c.Status != Warn || !strings.Contains(c.Message, "UNSAFE on a shared box: 2 instances (beta, gamma)") ||
		strings.Contains(c.Message, "alpha") || strings.Contains(c.Message, "older") || r.Warnings == 0 {
		t.Fatalf("with two unsafe instances: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(root, "instances", "gamma", "instance.json"), []byte(`{"secret_delivery":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := find(Run(Options{Root: root, PortHost: "127.0.0.1"}), "secret_delivery"); c.Status != Warn || !strings.Contains(c.Message, "1 instance (beta)") {
		t.Fatalf("with one unsafe instance: %+v", c)
	}
}
