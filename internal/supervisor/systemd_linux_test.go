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

// cleanupUnit makes sure the unit is gone after the test: a test may kill
// what ESM never kills.
func cleanupUnit(t *testing.T, name string) {
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", unitName(name)).Run()
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
	if err := testkit.WriteControl(data, testkit.ControlStop); err != nil { // stale request from a killed run
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
	log, _ := os.ReadFile(r.logFile(name))
	if !strings.Contains(string(log), "$HOME and %h stay literal") {
		t.Errorf("argument was rewritten by systemd:\n%s", log)
	}
	out, _ := exec.Command("systemctl", "--user", "show", unitName(name), "-p", "MemoryMax,CPUQuotaPerSecUSec").Output()
	if !strings.Contains(string(out), "MemoryMax=536870912") || !strings.Contains(string(out), "CPUQuotaPerSecUSec=500ms") {
		t.Errorf("limits not applied:\n%s", out)
	}
	// Even a stop from outside ESM sends one SIGINT to the server only,
	// and no SIGKILL after the timeout.
	out, _ = exec.Command("systemctl", "--user", "show", unitName(name), "-p", "KillSignal,KillMode,SendSIGKILL,TimeoutStopUSec").Output()
	for _, want := range []string{"KillSignal=2\n", "KillMode=mixed\n", "SendSIGKILL=no\n", "TimeoutStopUSec=5s\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("unit lacks %q:\n%s", strings.TrimSpace(want), out)
		}
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

	// Start again, then a fresh supervisor adopts it. The status file of
	// the crashed run says ready; only the new run's may count (a signal
	// before the server's handler is set up ends it at once).
	os.Remove(gameserver.StatusFilePath(data))
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
	if testkit.SecondSignal(data) {
		t.Fatal("the server got a second stop signal")
	}
	console, _ := r.st.ReadFile("instances/" + name + "/console.log")
	if !strings.Contains(string(console), "Dedicated session") {
		t.Fatalf("console.log not written by the unit:\n%s", console)
	}
}

// A stop is one SIGINT to the main process. A server that does not exit
// in time is left running (ErrStopTimeout), and stopping it again, even
// from a new supervisor, sends no second signal.
func TestSystemdStopIsOneSignal(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdone")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir(name)
	st := waitReady(t, data)
	start := time.Now()
	if err := s.Stop(context.Background(), name, time.Second); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("stop of a server that ignores it: %v, want ErrStopTimeout", err)
	}
	if time.Since(start) < time.Second {
		t.Fatal("returned before the timeout")
	}
	if pr, _ := s.Status(name); !pr.Running || pr.PID != st.PID {
		t.Fatalf("the server was not left running: %+v", pr)
	}
	if err := s.Stop(context.Background(), name, 300*time.Millisecond); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("second stop: %v", err)
	}
	if err := newSystemd(t, r).Stop(context.Background(), name, 300*time.Millisecond); !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("stop from a new supervisor: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if testkit.SecondSignal(data) {
		t.Fatal("the server got a second stop signal")
	}
	log, _ := os.ReadFile(r.logFile(name))
	if n := strings.Count(string(log), "Ignoring stop request"); n != 1 {
		t.Fatalf("the server saw %d stop requests, want 1:\n%s", n, log)
	}
}

// systemctl --user stop from outside ESM also sends exactly one SIGINT:
// the server saves and exits with its own code, no second signal.
func TestSystemdOutsideStopIsOneSignal(t *testing.T) {
	needSystemd(t)
	r := newRig(t)
	name := uniqueName("sdout")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if err := s.Start(context.Background(), r.spec(name, "-FakeStopDelay=1s")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir(name)
	waitReady(t, data)
	before := testkit.FakeSaveCounter(data, "TestMap", "w")
	if out, err := exec.Command("systemctl", "--user", "stop", unitName(name)).CombinedOutput(); err != nil {
		t.Fatalf("systemctl stop: %v\n%s", err, out)
	}
	if pr, _ := s.Status(name); pr.Running {
		t.Fatalf("still running: %+v", pr)
	}
	if testkit.SecondSignal(data) {
		t.Fatal("the server got a second stop signal")
	}
	log, _ := os.ReadFile(r.logFile(name))
	if !strings.Contains(string(log), "Stop requested (interrupt)") || !strings.Contains(string(log), "Exiting with code 130") ||
		testkit.FakeSaveCounter(data, "TestMap", "w") <= before {
		t.Fatalf("not a clean stop on one SIGINT:\n%s", log)
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

// A systemd-run failure must not repeat the passwords of a server that has
// them on its command line, even when systemd-run itself echoes the whole
// command line.
func TestSystemdStartErrorHidesPasswords(t *testing.T) {
	needSystemd(t)
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"Failed to start transient service unit: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(dir+"/systemd-run", []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	r := newRig(t)
	name := uniqueName("sdpw")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	spec := r.spec(name)
	spec.Args = append(spec.Args, "-ServerPassword=open sesame 42", "-AdminPassword=adm1n-secret")
	spec.SecretValues = []string{"open sesame 42", "adm1n-secret"}
	err := s.Start(context.Background(), spec)
	if err == nil {
		t.Fatal("start with a failing systemd-run succeeded")
	}
	for _, secret := range []string{"open sesame 42", "sesame", "adm1n-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("start error shows a password: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "<server command line>") || !strings.Contains(err.Error(), "Failed to start transient service unit") {
		t.Fatalf("start error lost its detail: %v", err)
	}
}

// fakeOldSystemd puts a systemctl on PATH that reports systemd 252 (older
// than --expand-environment, so ESM doubles every "$" of the command line)
// and hands every other call to the real one, and a systemd-run that fails,
// records its arguments in dir/args and repeats them: whole, and each
// value without its switch. It returns dir.
func fakeOldSystemd(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skip("no systemctl")
	}
	dir := t.TempDir()
	scripts := map[string]string{
		"systemctl": "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'systemd 252 (252.19-1)'; exit 0; fi\nexec '" + real + "' \"$@\"\n",
		"systemd-run": "#!/bin/sh\nprintf '%s\n' \"$@\" > '" + dir + "/args'\n" +
			"echo \"Failed to start transient service unit: $*\" >&2\n" +
			"for a in \"$@\"; do case \"$a\" in *=*) echo \"invalid value: ${a#*=}\" >&2;; esac; done\nexit 1\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

// On systemd versions without --expand-environment the command line goes
// to systemd-run with every "$" doubled. When systemd-run fails and repeats
// it, the passwords are hidden in that form too.
func TestSystemdOldVersionHidesEscapedPasswords(t *testing.T) {
	needSystemd(t)
	dir := fakeOldSystemd(t)
	r := newRig(t)
	name := uniqueName("sdold")
	cleanupUnit(t, name)
	s := newSystemd(t, r)
	if s.expandFlag {
		t.Fatal("the fake systemctl --version was not used")
	}
	const pw, admin = `p$ss w$rd &"';%,=<> 42`, "adm$n-$ecret"
	spec := r.spec(name)
	spec.Args = append(spec.Args, "-ServerPassword="+pw, "-AdminPassword="+admin)
	spec.SecretValues = []string{pw, admin}
	err := s.Start(context.Background(), spec)
	if err == nil {
		t.Fatal("start with a failing systemd-run succeeded")
	}
	args, _ := os.ReadFile(dir + "/args")
	if !strings.Contains(string(args), "-ServerPassword="+strings.ReplaceAll(pw, "$", "$$")) {
		t.Fatalf("systemd-run did not get the escaped command line:\n%s", args)
	}
	for _, piece := range []string{"p$ss", "p$$ss", "w$rd", "w$$rd", `&"';%,=<>`, "adm$n", "adm$$n", "$ecret", "$$ecret"} {
		if strings.Contains(err.Error(), piece) {
			t.Errorf("start error shows %q:\n%v", piece, err)
		}
	}
	if !strings.Contains(err.Error(), "invalid value: ") {
		t.Fatalf("start error lost systemd-run's message: %v", err)
	}
}
