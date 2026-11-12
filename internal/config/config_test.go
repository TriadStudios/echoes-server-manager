package config

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLoadWritesDefaultsOnFirstRun(t *testing.T) {
	st := openStore(t)
	cfg, created, err := Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first load should report created")
	}
	if cfg.Listen != "127.0.0.1:8740" || cfg.GamePorts != "7777-7786" || cfg.QueryPorts != "27015-27024" ||
		cfg.Supervisor != "auto" || cfg.StopTimeout.D() != 30*time.Second || cfg.ReadyTimeout.D() != 120*time.Second ||
		cfg.KeepPreviousBuilds != 2 || cfg.Backoff.Window.D() != 10*time.Minute || cfg.Backoff.MaxCrashes != 5 ||
		cfg.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.SocketPath() != filepath.Join(st.Path(), "esm.sock") {
		t.Fatalf("socket %q", cfg.SocketPath())
	}
	b, err := st.ReadFile(FileName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"stop_timeout": "30s"`) {
		t.Fatalf("durations should be written as strings:\n%s", b)
	}
	_, created, err = Load(st)
	if err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
}

func TestLoadKeepsDefaultsForMissingFields(t *testing.T) {
	st := openStore(t)
	if err := st.WriteFile(FileName, []byte(`{"listen":"127.0.0.1:9000","backoff":{"base":"1s"},"stop_timeout":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(st)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9000" || cfg.Backoff.Base.D() != time.Second || cfg.Backoff.Max.D() != 5*time.Minute ||
		cfg.StopTimeout.D() != 5*time.Second || cfg.GamePorts != DefaultGamePorts {
		t.Fatalf("merge wrong: %+v", cfg)
	}
}

func TestLoadRefusesBadFiles(t *testing.T) {
	cases := map[string]string{
		"unknown field":   `{"lisen":"127.0.0.1:1"}`,
		"bad listen":      `{"listen":"nonsense"}`,
		"half tls":        `{"tls_cert":"/x.pem"}`,
		"bad range":       `{"game_ports":"10-5"}`,
		"overlap":         `{"game_ports":"7000-7010","query_ports":"7005-7020"}`,
		"bad supervisor":  `{"supervisor":"docker"}`,
		"bad backoff":     `{"backoff":{"base":"10m","max":"1m"}}`,
		"zero crashes":    `{"backoff":{"max_crashes":0}}`,
		"bad duration":    `{"stop_timeout":"soon"}`,
		"zero ready":      `{"ready_timeout":"0s"}`,
		"negative builds": `{"keep_previous_builds":-1}`,
		"bad level":       `{"log_level":"loud"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			if err := st.WriteFile(FileName, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load(st); err == nil {
				t.Fatalf("accepted %s", body)
			}
		})
	}
}

func TestSystemdSupervisorOnlyOnLinux(t *testing.T) {
	cfg := Defaults(t.TempDir())
	cfg.Supervisor = "systemd"
	err := cfg.Validate()
	if runtime.GOOS == "linux" && err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" && err == nil {
		t.Fatal("systemd accepted off Linux")
	}
}

func TestListenRule(t *testing.T) {
	cases := []struct {
		listen, cert, key string
		ok                bool
	}{
		{"127.0.0.1:8740", "", "", true},
		{"127.10.0.5:8740", "", "", true},
		{"[::1]:8740", "", "", true},
		{"localhost:8740", "", "", true},
		{"LOCALHOST:8740", "", "", true},
		{"0.0.0.0:8740", "", "", false},
		{":8740", "", "", false},
		{"[::]:8740", "", "", false},
		{"192.0.2.10:8740", "", "", false},
		{"192.0.2.10:8740", "/c.pem", "", false},
		{"192.0.2.10:8740", "/c.pem", "/k.pem", true},
		{"0.0.0.0:8740", "/c.pem", "/k.pem", true},
		{"example.test:8740", "", "", false},
		{"bad", "", "", false},
	}
	for _, c := range cases {
		err := CheckListen(c.listen, c.cert, c.key)
		if (err == nil) != c.ok {
			t.Errorf("CheckListen(%q, %q, %q) = %v, want ok=%v", c.listen, c.cert, c.key, err, c.ok)
		}
	}
}

func TestParsePortRange(t *testing.T) {
	r, err := ParsePortRange("7777-7786")
	if err != nil || r.Lo != 7777 || r.Hi != 7786 || r.Len() != 10 || !r.Contains(7780) || r.Contains(7787) {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := ParsePortRange("27015"); err != nil || r.Len() != 1 || r.String() != "27015" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "a-b", "0-10", "10-70000", "20-10", "1-2-3"} {
		if _, err := ParsePortRange(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if !(PortRange{1, 5}).Overlaps(PortRange{5, 9}) || (PortRange{1, 4}).Overlaps(PortRange{5, 9}) {
		t.Fatal("overlap wrong")
	}
}

func TestResolveRoot(t *testing.T) {
	dir := t.TempDir()
	got, err := ResolveRoot(dir)
	if err != nil || got != dir {
		t.Fatalf("flag: %q %v", got, err)
	}
	t.Setenv(RootEnv, filepath.Join(dir, "env"))
	got, err = ResolveRoot("")
	if err != nil || got != filepath.Join(dir, "env") {
		t.Fatalf("env: %q %v", got, err)
	}
	t.Setenv(RootEnv, "")
	got, err = ResolveRoot("")
	if err != nil || filepath.Base(got) != "durinn" || !filepath.IsAbs(got) {
		t.Fatalf("default: %q %v", got, err)
	}
}

func TestRefuseSuperuser(t *testing.T) {
	if runtime.GOOS == "linux" {
		if checkUID(0) == nil {
			t.Fatal("uid 0 accepted")
		}
	}
	if err := checkUID(1000); err != nil {
		t.Fatal(err)
	}
}
