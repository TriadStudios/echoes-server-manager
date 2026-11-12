package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The systemd tests start real user units, so they only run when asked:
//
//	ESM_TEST_SYSTEMD=1 go test ./internal/supervisor -run Systemd
func needSystemd(t *testing.T) {
	t.Helper()
	if os.Getenv("ESM_TEST_SYSTEMD") != "1" {
		t.Skip("set ESM_TEST_SYSTEMD=1 to run tests that start systemd user units")
	}
	if !SystemdAvailable() {
		t.Skip("systemctl --user does not work here")
	}
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

func newSystemd(t *testing.T, r *rig) *Systemd {
	t.Helper()
	s, err := NewSystemd(r.st, Options{StopTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s.(*Systemd)
}

func cleanupUnit(t *testing.T, name string) {
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "stop", unitName(name)).Run()
		exec.Command("systemctl", "--user", "reset-failed", unitName(name)).Run()
	})
}

func TestSystemdLifecycle(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sd")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if s.Kind() != KindSystemd || !s.LimitsEnforced() {
		t.Fatal("kind or limits wrong")
	}
	if _, err := s.Status(name); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("status of unknown instance: %v", err)
	}
	spec := r.spec(name)
	spec.Limits = Limits{MemoryMaxBytes: 512 << 20, CPUQuotaPercent: 50}
	spec.Args = append(spec.Args, "-ServerName=$HOME and %h stay literal")
	data := r.dataDir(name)
	if err := gameserver.RequestStop(data); err != nil { // stale request from a killed run
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	pr, err := s.Status(name)
	if err != nil || !pr.Running || pr.PID <= 0 {
		t.Fatalf("status after start: %+v %v", pr, err)
	}
	waitReady(t, data)
	time.Sleep(300 * time.Millisecond)
	if pr, _ := s.Status(name); !pr.Running {
		t.Fatal("server stopped right after start (old stop request was not cleared)")
	}
	log, _ := os.ReadFile(gameserver.ServerLogPath(data))
	if !strings.Contains(string(log), "$HOME and %h stay literal") {
		t.Errorf("argument was rewritten by systemd:\n%s", log)
	}
	out, _ := exec.Command("systemctl", "--user", "show", unitName(name), "-p", "MemoryMax,CPUQuotaPerSecUSec").Output()
	if !strings.Contains(string(out), "MemoryMax=536870912") || !strings.Contains(string(out), "CPUQuotaPerSecUSec=500ms") {
		t.Errorf("limits not applied:\n%s", out)
	}
	if err := s.Start(context.Background(), spec); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("double start: %v", err)
	}

	// Crash detection.
	if err := testkit.WriteControl(data, testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		pr, err = s.Status(name)
		return err == nil && !pr.Running
	}, "crash not detected")

	// Start again, then a fresh supervisor adopts it.
	spec.Args = spec.Args[:len(spec.Args)-1]
	if err := s.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	waitReady(t, data)
	before, _ := s.Status(name)
	s2 := newSystemd(t, r)
	procs, err := s2.Adopt()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range procs {
		if p.Name == name && p.Running && p.PID == before.PID {
			found = true
		}
	}
	if !found {
		t.Fatalf("adopt did not find %s (pid %d): %+v", name, before.PID, procs)
	}

	// Polite stop.
	if err := s2.Stop(context.Background(), name, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	pr, err = s2.Status(name)
	if err != nil || pr.Running {
		t.Fatalf("after stop: %+v %v", pr, err)
	}
	if st, _ := gameserver.ReadStatus(data); st.State != gameserver.StateStopping {
		t.Fatalf("not stopped politely: %q", st.State)
	}
	console, _ := r.st.ReadFile("instances/" + name + "/console.log")
	if !strings.Contains(string(console), "Server is ready") {
		t.Fatalf("console.log not written by the unit:\n%s", console)
	}
}

func TestSystemdForceStop(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdforce")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	waitReady(t, r.dataDir(name))
	start := time.Now()
	if err := s.Stop(context.Background(), name, time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("returned before the polite timeout")
	}
	if pr, _ := s.Status(name); pr.Running {
		t.Fatal("still running")
	}
}

func TestSystemdLeavesForeignUnitsAlone(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdforeign")
	r.dataDir(name)
	cleanupUnit(t, name)
	// A unit with our naming scheme that this root did not create.
	if out, err := exec.Command("systemd-run", "--user", "--quiet", "--collect", "--unit=esm-"+name,
		"-p", "Description=something else", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v %s", err, out)
	}
	s := newSystemd(t, r)
	if err := s.Stop(context.Background(), name, time.Second); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Stop = %v, want ErrNotOurs", err)
	}
	if err := s.Start(context.Background(), r.spec(name)); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("Start = %v, want ErrNotOurs", err)
	}
	procs, _ := s.Adopt()
	for _, p := range procs {
		if p.Name == name {
			t.Fatal("foreign unit adopted")
		}
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", unitName(name)).Output()
	if strings.TrimSpace(string(out)) != "active" {
		t.Fatalf("foreign unit was touched: %s", out)
	}
	// Units of instances that are not in the store are never touched either.
	if err := s.Stop(context.Background(), "not-in-store", time.Second); !errors.Is(err, ErrUnknownInstance) {
		t.Fatalf("Stop of unknown instance = %v", err)
	}
}
