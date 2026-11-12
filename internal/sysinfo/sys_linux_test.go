package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeKernel points procRoot and cgroupRoot at files written by the test.
func fakeKernel(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldProc, oldCg := procRoot, cgroupRoot
	procRoot, cgroupRoot = filepath.Join(dir, "proc"), filepath.Join(dir, "cgroup")
	t.Cleanup(func() { procRoot, cgroupRoot = oldProc, oldCg })
}

func TestLinuxMemoryAndLoad(t *testing.T) {
	fakeKernel(t, map[string]string{
		"proc/meminfo": "MemTotal:       16384000 kB\nMemFree:         1000 kB\nMemAvailable:    8192000 kB\n",
		"proc/loadavg": "1.25 0.80 0.50 2/345 6789\n",
	})
	total, avail, err := memory()
	if err != nil || total != 16384000<<10 || avail != 8192000<<10 {
		t.Fatalf("memory %d %d %v", total, avail, err)
	}
	if l, err := load1(); err != nil || l != 1.25 {
		t.Fatalf("load %v %v", l, err)
	}
}

func TestLinuxMemoryRefusesOddFiles(t *testing.T) {
	fakeKernel(t, map[string]string{"proc/meminfo": "MemTotal: 12 MB\nMemAvailable: 1 kB\n"})
	if _, _, err := memory(); err == nil {
		t.Fatal("an unknown unit was accepted")
	}
	fakeKernel(t, map[string]string{"proc/meminfo": "MemTotal: 12 kB\n"})
	if _, _, err := memory(); err == nil {
		t.Fatal("a missing MemAvailable was accepted")
	}
}

// The memory of a server in its own systemd unit is its cgroup's; a server
// that shares a cgroup (process mode) is measured by its resident memory.
func TestLinuxProcessMemory(t *testing.T) {
	fakeKernel(t, map[string]string{
		"proc/41/cgroup": "0::/user.slice/user-1000.slice/user@1000.service/app.slice/esm-alpha.service\n",
		"proc/41/status": "Name:\tfake\nVmRSS:\t   2048 kB\n",
		"proc/42/cgroup": "0::/user.slice/user-1000.slice/session-3.scope\n",
		"proc/42/status": "Name:\tfake\nVmRSS:\t   1024 kB\n",
		"proc/43/cgroup": "0::/../../esm-alpha.service\n",
		"proc/43/status": "VmRSS: 4 kB\n",
		"cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/esm-alpha.service/memory.current": "7340032\n",
		"cgroup/esm-alpha.service/memory.current":                                                        "1\n",
	})
	cases := []struct {
		pid  int
		unit string
		n    int64
		src  string
	}{
		{41, "esm-alpha.service", 7340032, SourceCgroup},
		{41, "", 2048 << 10, SourceProcess},
		{41, "esm-beta.service", 2048 << 10, SourceProcess},
		{42, "esm-alpha.service", 1024 << 10, SourceProcess},
		{43, "esm-alpha.service", 4 << 10, SourceProcess},
	}
	for _, c := range cases {
		n, src, err := ProcessMemory(c.pid, c.unit)
		if err != nil || n != c.n || src != c.src {
			t.Errorf("pid %d unit %q: %d %q %v, want %d %q", c.pid, c.unit, n, src, err, c.n, c.src)
		}
	}
	if _, _, err := ProcessMemory(44, ""); err == nil {
		t.Fatal("a missing process was read")
	}
}
