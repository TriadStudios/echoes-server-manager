package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

type rig struct {
	t  *testing.T
	st *store.Store
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &rig{t: t, st: st}
}

// dataDir creates the instance folder and returns its data folder.
func (r *rig) dataDir(name string) string {
	r.t.Helper()
	if err := r.st.MkdirAll("instances/"+name+"/data", 0o700); err != nil {
		r.t.Fatal(err)
	}
	d, _ := r.st.Abs("instances/" + name + "/data")
	return d
}

func (r *rig) spec(name string, extra ...string) Spec {
	r.t.Helper()
	data := r.dataDir(name)
	argv := gameserver.Launch{BindIP: "127.0.0.1", MaxPlayers: 4, ServerName: name, WorldName: "w", DataDir: data}.Args()
	argv = append(argv, "-FakeReadyDelay=100ms", "-FakeSaveInterval=200ms")
	argv = append(argv, extra...)
	dir, _ := r.st.Abs("instances/" + name)
	out, _ := r.st.Abs("instances/" + name + "/console.log")
	return Spec{
		Name:       name,
		Executable: testkit.FakeServer(r.t),
		Args:       argv,
		Dir:        dir,
		OutputFile: out,
		StopFile:   gameserver.StopRequestPath(data),
	}
}

// cleanupStop makes sure nothing started under the rig outlives the test.
func (r *rig) cleanupStop(name string) {
	r.t.Cleanup(func() {
		p, err := NewProcess(r.st, Options{})
		if err != nil {
			return
		}
		defer p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		p.Adopt()
		p.Stop(ctx, name, time.Second)
	})
}

func waitReady(t *testing.T, data string) gameserver.Status {
	t.Helper()
	var st gameserver.Status
	testkit.Eventually(t, 10*time.Second, func() bool {
		s, err := gameserver.ReadStatus(data)
		st = s
		return err == nil && s.State == gameserver.StateReady
	}, "server never reported ready")
	return st
}

func newProc(t *testing.T, st *store.Store) *Process {
	t.Helper()
	p, err := NewProcess(st, Options{PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProcessLifecycle(t *testing.T) {
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	spec := r.spec("alpha")
	r.cleanupStop("alpha")
	if p.Kind() != "process" || p.LimitsEnforced() {
		t.Fatal("kind or limits wrong")
	}
	data := r.dataDir("alpha")
	// A stop request left from an earlier killed run must not stop the
	// new server.
	if err := testkit.WriteControl(data, testkit.ControlStop); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	pr, err := p.Status("alpha")
	if err != nil || !pr.Running || pr.PID <= 0 || pr.StartedAt.IsZero() {
		t.Fatalf("status after start: %+v %v", pr, err)
	}
	st := waitReady(t, data)
	time.Sleep(300 * time.Millisecond)
	if pr, _ := p.Status("alpha"); !pr.Running {
		t.Fatal("server stopped right after start (old stop request was not cleared)")
	}
	if st.PID != pr.PID {
		t.Fatalf("status file pid %d, supervisor pid %d", st.PID, pr.PID)
	}
	var rec pidRecord
	if err := r.st.ReadJSON("instances/alpha/run/pid.json", &rec); err != nil {
		t.Fatal(err)
	}
	if rec.PID != pr.PID || rec.StartMarker == "" || !filepath.IsAbs(rec.Executable) {
		t.Fatalf("pid file %+v", rec)
	}

	start := time.Now()
	if err := p.Stop(context.Background(), "alpha", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("polite stop took too long (was it forced?)")
	}
	pr, _ = p.Status("alpha")
	if pr.Running || pr.ExitCode == nil || *pr.ExitCode != 0 || pr.ExitedAt.IsZero() {
		t.Fatalf("after polite stop: %+v", pr)
	}
	if s, _ := gameserver.ReadStatus(data); s.State != gameserver.StateStopping {
		t.Fatalf("server did not stop politely, state %q", s.State)
	}
	if files, _ := gameserver.SaveFiles(data); len(files) != 1 {
		t.Fatalf("world not saved: %v", files)
	}
	testkit.Eventually(t, 2*time.Second, func() bool { return !r.st.Exists("instances/alpha/run/pid.json") }, "pid file left behind")
	console, err := r.st.ReadFile("instances/alpha/console.log")
	if err != nil || !strings.Contains(string(console), "Server is ready") {
		t.Fatalf("console.log: %v\n%s", err, console)
	}
	// Stopping again is fine.
	if err := p.Stop(context.Background(), "alpha", time.Second); err != nil {
		t.Fatal(err)
	}
	// It can start again.
	if err := p.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background(), "alpha", 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestProcessForceStop(t *testing.T) {
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	r.cleanupStop("stubborn")
	if err := p.Start(context.Background(), r.spec("stubborn", "-FakeIgnoreStop")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir("stubborn")
	waitReady(t, data)
	start := time.Now()
	if err := p.Stop(context.Background(), "stubborn", time.Second); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < time.Second {
		t.Fatalf("stop returned after %v, before the polite timeout", el)
	}
	pr, _ := p.Status("stubborn")
	if pr.Running || pr.ExitCode == nil || *pr.ExitCode == 0 {
		t.Fatalf("after force stop: %+v", pr)
	}
	log, _ := os.ReadFile(gameserver.ServerLogPath(data))
	if !strings.Contains(string(log), "Ignoring stop request") {
		t.Fatalf("polite request never reached the server:\n%s", log)
	}
}

func TestProcessCrashDetected(t *testing.T) {
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	r.cleanupStop("crashy")
	if err := p.Start(context.Background(), r.spec("crashy", "-FakeCrashAfter=600ms", "-FakeExitCode=7")); err != nil {
		t.Fatal(err)
	}
	var pr Proc
	testkit.Eventually(t, 10*time.Second, func() bool {
		pr, _ = p.Status("crashy")
		return !pr.Running
	}, "crash not detected")
	if pr.ExitCode == nil || *pr.ExitCode != 7 || pr.ExitedAt.IsZero() {
		t.Fatalf("crash status %+v", pr)
	}
	// A server that fails at once is also reported, not hidden by Start.
	if err := p.Start(context.Background(), r.spec("crashy", "-FakeFailStart")); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		pr, _ = p.Status("crashy")
		return !pr.Running && pr.ExitCode != nil
	}, "fail-start not detected")
	if *pr.ExitCode != 3 {
		t.Fatalf("fail-start exit %d", *pr.ExitCode)
	}
}

func TestProcessAdoptAfterSupervisorRestart(t *testing.T) {
	r := newRig(t)
	r.cleanupStop("keeper")
	first := newProc(t, r.st)
	if err := first.Start(context.Background(), r.spec("keeper")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir("keeper")
	waitReady(t, data)
	before, _ := first.Status("keeper")
	// The first supervisor goes away; the server keeps running.
	first.Close()
	first = nil

	second := newProc(t, r.st)
	defer second.Close()
	if _, err := second.Status("keeper"); err != nil {
		t.Fatalf("status before adopt: %v", err)
	}
	procs, err := second.Adopt()
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 1 || procs[0].Name != "keeper" || procs[0].PID != before.PID || !procs[0].Running {
		t.Fatalf("adopted %+v, want pid %d", procs, before.PID)
	}
	pr, err := second.Status("keeper")
	if err != nil || !pr.Running || pr.PID != before.PID {
		t.Fatalf("status after adopt: %+v %v", pr, err)
	}
	if again, _ := second.Adopt(); len(again) != 1 {
		t.Fatalf("second adopt: %+v", again)
	}
	if err := second.Stop(context.Background(), "keeper", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	pr, _ = second.Status("keeper")
	if pr.Running {
		t.Fatalf("still running after stop: %+v", pr)
	}
	if pr.ExitCode != nil {
		t.Fatalf("exit code should be unknown after adoption, got %d", *pr.ExitCode)
	}
	if s, _ := gameserver.ReadStatus(data); s.State != gameserver.StateStopping {
		t.Fatalf("adopted server was not stopped politely: %q", s.State)
	}
	testkit.Eventually(t, 2*time.Second, func() bool { return !r.st.Exists("instances/keeper/run/pid.json") }, "pid file left behind")
	// The new supervisor can start it again.
	if err := second.Start(context.Background(), r.spec("keeper")); err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(context.Background(), "keeper", 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestProcessAdoptedCrashDetected(t *testing.T) {
	r := newRig(t)
	r.cleanupStop("fragile")
	first := newProc(t, r.st)
	if err := first.Start(context.Background(), r.spec("fragile")); err != nil {
		t.Fatal(err)
	}
	data := r.dataDir("fragile")
	waitReady(t, data)
	first.Close()

	second := newProc(t, r.st)
	defer second.Close()
	if procs, _ := second.Adopt(); len(procs) != 1 {
		t.Fatalf("adopt: %+v", procs)
	}
	// A second instance of the same name cannot start while it runs.
	if err := second.Start(context.Background(), r.spec("fragile")); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("duplicate start: %v", err)
	}
	if err := testkit.WriteControl(data, testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		pr, _ := second.Status("fragile")
		return !pr.Running
	}, "crash of adopted server not detected")
	testkit.Eventually(t, 2*time.Second, func() bool { return !r.st.Exists("instances/fragile/run/pid.json") }, "stale pid file kept")
}

func TestProcessRefusesForeignPID(t *testing.T) {
	r := newRig(t)
	data := r.dataDir("foreign")
	fake := testkit.FakeServer(t)
	argv := gameserver.Launch{BindIP: "127.0.0.1", MaxPlayers: 1, ServerName: "f", WorldName: "w", DataDir: data}.Args()
	cmd := exec.Command(fake, append(argv, "-FakeReadyDelay=50ms")...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	waitReady(t, data)
	pid := cmd.Process.Pid
	marker, err := startMarker(pid)
	if err != nil {
		t.Fatal(err)
	}
	alive := func() bool {
		select {
		case <-done:
			return false
		case <-time.After(300 * time.Millisecond):
			return true
		}
	}
	write := func(rec pidRecord) {
		if err := r.st.WriteJSON("instances/foreign/run/pid.json", rec, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exe, _ := filepath.EvalSymlinks(fake)
	stopFile := gameserver.StopRequestPath(data)

	cases := map[string]pidRecord{
		"wrong start marker": {PID: pid, StartMarker: marker + "0", Executable: exe, StopFile: stopFile},
		"wrong executable":   {PID: pid, StartMarker: marker, Executable: os.Args[0], StopFile: stopFile},
	}
	for what, rec := range cases {
		write(rec)
		p := newProc(t, r.st)
		if pr, _ := p.Status("foreign"); pr.Running {
			t.Errorf("%s: reported as running", what)
		}
		if err := p.Stop(context.Background(), "foreign", 100*time.Millisecond); !errors.Is(err, ErrNotOurs) {
			t.Errorf("%s: Stop = %v, want ErrNotOurs", what, err)
		}
		if !alive() {
			t.Fatalf("%s: the foreign process was touched", what)
		}
		p2 := newProc(t, r.st)
		if procs, _ := p2.Adopt(); len(procs) != 0 {
			t.Errorf("%s: adopted %+v", what, procs)
		}
		if r.st.Exists("instances/foreign/run/pid.json") {
			t.Errorf("%s: stale pid file kept after adopt", what)
		}
		if !alive() {
			t.Fatalf("%s: the foreign process was touched by Adopt", what)
		}
		p.Close()
		p2.Close()
	}

	// With the right marker and executable it is ours and can be stopped.
	write(pidRecord{PID: pid, StartMarker: marker, Executable: exe, StopFile: stopFile})
	p := newProc(t, r.st)
	defer p.Close()
	if pr, _ := p.Status("foreign"); !pr.Running || pr.PID != pid {
		t.Fatalf("matching record not recognised: %+v", pr)
	}
	if err := p.Stop(context.Background(), "foreign", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("matching process was not stopped")
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit %d, want a polite stop", cmd.ProcessState.ExitCode())
	}
}

func TestProcessStartValidation(t *testing.T) {
	r := newRig(t)
	p := newProc(t, r.st)
	defer p.Close()
	ctx := context.Background()
	good := r.spec("valid")
	r.cleanupStop("valid")

	bad := good
	bad.Name = "nothere"
	if err := p.Start(ctx, bad); !errors.Is(err, ErrUnknownInstance) {
		t.Errorf("unknown instance: %v", err)
	}
	bad = good
	bad.Name = "Bad_Name"
	if err := p.Start(ctx, bad); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: %v", err)
	}
	bad = good
	bad.Executable = "esm-fakeserver"
	if err := p.Start(ctx, bad); err == nil {
		t.Error("relative executable accepted")
	}
	bad = good
	bad.OutputFile = filepath.Join(t.TempDir(), "console.log")
	if err := p.Start(ctx, bad); err == nil {
		t.Error("output file outside the root accepted")
	}
	bad = good
	bad.StopFile = filepath.Join(t.TempDir(), "stop")
	if err := p.Start(ctx, bad); err == nil {
		t.Error("stop file outside the root accepted")
	}
	if _, err := p.Status("valid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("status of never-started instance: %v", err)
	}
	if err := p.Stop(ctx, "valid", time.Second); err != nil {
		t.Errorf("stop of never-started instance: %v", err)
	}
	if err := p.Start(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, good); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("double start: %v", err)
	}
	if err := p.Stop(ctx, "valid", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if err := p.Start(ctx, good); !errors.Is(err, ErrClosed) {
		t.Errorf("start after close: %v", err)
	}
}

func TestNewSelectsKind(t *testing.T) {
	r := newRig(t)
	s, err := New(r.st, Options{Kind: "process"})
	if err != nil || s.Kind() != KindProcess || s.LimitsEnforced() {
		t.Fatalf("process: %v %v", s, err)
	}
	s.Close()
	if _, err := New(r.st, Options{Kind: "docker"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	auto, err := New(r.st, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	want := KindProcess
	if SystemdAvailable() {
		want = KindSystemd
	}
	if auto.Kind() != want {
		t.Fatalf("auto picked %s, want %s", auto.Kind(), want)
	}
	if runtime.GOOS != "linux" {
		if _, err := New(r.st, Options{Kind: "systemd"}); err == nil {
			t.Fatal("systemd accepted off Linux")
		}
	}
	for _, n := range []string{"a", "alpha-2", "a234567890123456789012345678901"} {
		if !ValidName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "1a", "A", "a_b", "a/b", "../x", strings.Repeat("a", 33)} {
		if ValidName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}
