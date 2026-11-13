package page

import (
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/sysinfo"
)

// After a login the page goes only to one of its own paths.
func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/builds":                "/builds",
		"/servers/alpha/log":     "/servers/alpha/log",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"https://evil.example":   "/",
		"javascript:alert(1)":    "/",
		"/api/v1/instances":      "/",
		"/setup":                 "/",
		"/static/esm.js":         "/",
		"/servers?x=1":           "/",
		"/a b":                   "/",
		"/servers/alpha\n/x":     "/",
		"/C:/Windows/win.ini":    "/",
		"/servers/alpha#section": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 8 << 30: "8.0 GiB", 200 << 30: "200 GiB", -2048: "-2.0 KiB"} {
		if got := sizeOf(n); got != want {
			t.Errorf("sizeOf(%d) = %q, want %q", n, got, want)
		}
	}
	for _, c := range []struct {
		part, whole int64
		want        int
	}{{0, 100, 0}, {1, 100, 0}, {3, 100, 5}, {33, 100, 35}, {99, 100, 100}, {150, 100, 100}, {5, 0, 0}, {-1, 10, 0}} {
		if got := step(c.part, c.whole); got != c.want || got%5 != 0 {
			t.Errorf("step(%d, %d) = %d, want %d", c.part, c.whole, got, c.want)
		}
	}
	for line, want := range map[string]string{
		"[12:00] World saved (3)":            "",
		"LogNet: Warning: slow tick":         "warning",
		"[WARN] port busy":                   "warning",
		"LogTemp: Error: could not load":     "error",
		"FATAL error":                        "error",
		"0 errors, 2 warnings":               "",
		"terrorist bandit spawned":           "",
		"Critical section entered":           "error",
		"warning and error on the same line": "error",
	} {
		if got := lineLevel(line); got != want {
			t.Errorf("lineLevel(%q) = %q, want %q", line, got, want)
		}
	}
	for target, want := range map[string]bool{"alpha": true, "alpha/2026-manual": true, "alpha to 1.1": true, "alphabet": false, "beta": false} {
		if got := targets(target, "alpha"); got != want {
			t.Errorf("targets(%q) = %v", target, got)
		}
	}
	for d, want := range map[time.Duration]string{0: "never", time.Hour: "every hour", 6 * time.Hour: "every 6 hours", 90 * time.Minute: "every 90 minutes", 90 * time.Second: "every 1m30s"} {
		if got := everyWords(config.Duration(d)); got != want {
			t.Errorf("everyWords(%s) = %q, want %q", d, got, want)
		}
	}
	if c := backupChoices(config.Duration(6 * time.Hour)); len(c) != 5 || !c[2].Selected || c[2].Value != "6h0m0s" {
		t.Errorf("choices for 6h: %+v", c)
	}
	if c := backupChoices(config.Duration(30 * time.Minute)); len(c) != 6 || !c[5].Selected || c[5].Label != "30 minutes (now)" {
		t.Errorf("choices for 30m: %+v", c)
	}
	if got := sentence("memory and CPU limits are not enforced"); got != "Memory and CPU limits are not enforced." {
		t.Errorf("sentence: %q", got)
	}
}

// The box budget shows only what the API reported, and says why for the
// rest.
func TestBudgetIsHonest(t *testing.T) {
	gib := int64(1 << 30)
	total, avail, disk, free, load := 16*gib, 4*gib, 100*gib, 25*gib, 3.0
	known := api.Box{Supervisor: "process", Usage: sysinfo.Box{CPUCount: 4, MemoryTotalBytes: &total, MemoryAvailableBytes: &avail,
		Load1: &load, DiskTotalBytes: &disk, DiskFreeBytes: &free, Unknown: []sysinfo.Unknown{}}}
	list := []instances.View{{Instance: instances.Instance{Limits: instances.Limits{MemoryMax: "2G"}}}, {}}
	rows := budget(known, list)
	if len(rows) != 3 || !rows[0].Known || rows[0].Step != 75 || rows[0].Value != "12.0 GiB / 16.0 GiB" ||
		!strings.Contains(rows[0].Note, "2.0 GiB in all (1 server)") || !strings.Contains(rows[0].Note, "Not enforced in process mode") {
		t.Fatalf("memory row %+v", rows[0])
	}
	if !rows[1].Known || rows[1].Step != 75 || !rows[2].Known || rows[2].Step != 75 {
		t.Fatalf("cpu and disk rows %+v %+v", rows[1], rows[2])
	}
	none := api.Box{Usage: sysinfo.Box{CPUCount: 4, Unknown: []sysinfo.Unknown{
		{Field: "memory_total_bytes", Reason: "no meminfo"}, {Field: "load_1m", Reason: "no loadavg"}, {Field: "disk_total_bytes", Reason: "no statfs"}}}}
	rows = budget(none, nil)
	for i, why := range []string{"no meminfo", "no loadavg", "no statfs"} {
		if rows[i].Known || rows[i].Step != 0 || !strings.Contains(rows[i].Note, why) {
			t.Errorf("unknown row %d: %+v", i, rows[i])
		}
	}
}
