package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

func TestParseArgs(t *testing.T) {
	kv := map[string]string{"FakeReadyDelay": "5s", "FakeNoReady": "true"}
	argv, _ := gameserver.Launch{Map: "M", GamePort: 1, QueryPort: 2, MaxPlayers: 3, ServerName: "S", WorldName: "W",
		DataDir: "data", Rules: map[string]string{"PvP": "true"}, ExtraArgs: []string{"-FakeReadyDelay=10ms", "-FakeIgnoreStop"}}.Args()
	m, err := parseArgs(argv, kv)
	if err != nil {
		t.Fatal(err)
	}
	o, err := buildOptions(m, kv)
	if err != nil {
		t.Fatal(err)
	}
	if o.mapName != "M" || o.port != 1 || o.queryPort != 2 || o.maxPlayers != 3 || o.serverName != "S" ||
		o.worldName != "W" || !filepath.IsAbs(o.dataDir) || o.readyDelay != 10*time.Millisecond ||
		!o.noReady || !o.ignoreStop || o.exitCode != 1 || o.saveInterval != time.Second {
		t.Fatalf("options %+v", o)
	}
	if _, err := parseArgs([]string{"-A=1", "stray"}, map[string]string{}); err == nil {
		t.Fatal("stray positional accepted")
	}
	if _, err := buildOptions("", map[string]string{"Port": "x"}); err == nil {
		t.Fatal("bad port accepted")
	}
}

type fakeRun struct {
	cmd  *exec.Cmd
	data string
	done chan error
}

func startFake(t *testing.T, exe string, extra ...string) *fakeRun {
	t.Helper()
	return startFakeIn(t, exe, filepath.Join(t.TempDir(), "data"), extra...)
}

func startFakeIn(t *testing.T, exe, data string, extra ...string) *fakeRun {
	t.Helper()
	argv, _ := gameserver.Launch{GamePort: 0, QueryPort: 0, BindIP: "127.0.0.1", MaxPlayers: 8, ServerName: "T",
		WorldName: "w", DataDir: data, AdminPassword: "s3cret-admin"}.Args()
	argv = append(argv, "-FakeSaveInterval=200ms", "-FakeReadyDelay=100ms")
	argv = append(argv, extra...)
	cmd := exec.Command(exe, argv...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &fakeRun{cmd: cmd, data: data, done: make(chan error, 1)}
	go func() { r.done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-r.done
	})
	return r
}

func (r *fakeRun) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // let cleanup read it again
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	case <-time.After(timeout):
		t.Fatal("fake did not exit")
	}
	return -1
}

func waitReady(t *testing.T, data string) gameserver.Status {
	t.Helper()
	var st gameserver.Status
	testkit.Eventually(t, 10*time.Second, func() bool {
		s, err := gameserver.ReadStatus(data)
		st = s
		return err == nil && s.State == gameserver.StateReady
	}, "fake never reported ready")
	return st
}

func TestFakeLifecycle(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t))
	st := waitReady(t, r.data)
	if st.PID != r.cmd.Process.Pid || st.Ports.Game == 0 || st.Ports.Query == 0 || st.Ports.Game == st.Ports.Query {
		t.Fatalf("status %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gameserver.Probe(ctx, "127.0.0.1", st.Ports.Query); err != nil {
		t.Fatalf("query probe: %v", err)
	}
	save := filepath.Join(r.data, filepath.FromSlash(gameserver.SaveFileRel("w")))
	testkit.Eventually(t, 5*time.Second, func() bool { _, err := os.Stat(save); return err == nil }, "no save file")
	if err := testkit.WriteControl(r.data, testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 5*time.Second, func() bool {
		b, _ := os.ReadFile(gameserver.ServerLogPath(r.data))
		return strings.Count(string(b), "Command line: ") == 2
	}, "the command line was not printed again")
	if err := testkit.WriteControl(r.data, testkit.ControlStop); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Fatalf("exit code %d after polite stop", code)
	}
	st2, _ := gameserver.ReadStatus(r.data)
	if st2.State != gameserver.StateStopping {
		t.Fatalf("final state %q", st2.State)
	}
	log, err := os.ReadFile(gameserver.ServerLogPath(r.data))
	if err != nil {
		t.Fatal(err)
	}
	text := string(log)
	if !gameserver.ReadyLine.MatchString(text) || !gameserver.WorldSavedLine.MatchString(text) {
		t.Fatalf("log misses ready or saved lines:\n%s", text)
	}
	// Like a real server, the fake prints its whole command line; masking
	// it is ESM's job.
	if !strings.Contains(text, "Command line: ") || !strings.Contains(text, "-AdminPassword=s3cret-admin -log") {
		t.Fatalf("log misses the command line:\n%s", text)
	}
	files, _ := gameserver.SaveFiles(r.data)
	if len(files) != 1 {
		t.Fatalf("save files %v", files)
	}
}

func TestFakeCounterContinues(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	save := filepath.Join(data, filepath.FromSlash(gameserver.SaveFileRel("w")))
	os.MkdirAll(filepath.Dir(save), 0o700)
	if err := os.WriteFile(save, []byte("fake-save world=w counter=41\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startFakeIn(t, testkit.FakeServer(t), data, "-FakeSaveInterval=1h")
	waitReady(t, r.data)
	testkit.WriteControl(r.data, testkit.ControlStop)
	r.wait(t, 10*time.Second)
	b, _ := os.ReadFile(save)
	if !strings.Contains(string(b), "counter=42") {
		t.Fatalf("final save did not continue the counter: %q", b)
	}
}

func TestFakeFailStartAndCrash(t *testing.T) {
	exe := testkit.FakeServer(t)
	if code := startFake(t, exe, "-FakeFailStart").wait(t, 10*time.Second); code != 3 {
		t.Fatalf("fail start exit %d", code)
	}
	r := startFake(t, exe, "-FakeExitCode=7")
	waitReady(t, r.data)
	testkit.WriteControl(r.data, testkit.ControlCrash)
	if code := r.wait(t, 10*time.Second); code != 7 {
		t.Fatalf("crash exit %d", code)
	}
	if code := startFake(t, exe, "-FakeCrashAfter=300ms").wait(t, 10*time.Second); code != 1 {
		t.Fatalf("crash-after exit %d", code)
	}
}

func TestFakeHangIgnoresStop(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t))
	waitReady(t, r.data)
	testkit.WriteControl(r.data, testkit.ControlHang)
	time.Sleep(300 * time.Millisecond)
	testkit.WriteControl(r.data, testkit.ControlStop)
	select {
	case <-r.done:
		t.Fatal("fake stopped while hanging")
	case <-time.After(700 * time.Millisecond):
	}
	log, _ := os.ReadFile(gameserver.ServerLogPath(r.data))
	if !strings.Contains(string(log), "Ignoring stop request") {
		t.Fatalf("no ignore line:\n%s", log)
	}
}

func TestFakeJSONBreaksBuild(t *testing.T) {
	dir := testkit.MakeBuild(t, t.TempDir(), testkit.BuildOptions{Version: "broken", Fake: map[string]any{"FakeFailStart": true}})
	exe := filepath.Join(dir, filepath.FromSlash(map[string]string{"linux": testkit.LinuxExe, "windows": testkit.WindowsExe}[runtime.GOOS]))
	r := startFake(t, exe)
	if code := r.wait(t, 10*time.Second); code != 3 {
		t.Fatalf("broken build exit %d", code)
	}
	good := testkit.MakeBuild(t, t.TempDir(), testkit.BuildOptions{Version: "good-1"})
	exe = filepath.Join(good, filepath.FromSlash(map[string]string{"linux": testkit.LinuxExe, "windows": testkit.WindowsExe}[runtime.GOOS]))
	r = startFake(t, exe)
	if st := waitReady(t, r.data); st.Version != "good-1" {
		t.Fatalf("version %q", st.Version)
	}
}

func TestFakeSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM cannot be sent on Windows; the stop file is used there")
	}
	r := startFake(t, testkit.FakeServer(t))
	waitReady(t, r.data)
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Fatalf("exit %d after SIGTERM", code)
	}
}

func TestFakeUnready(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t))
	st := waitReady(t, r.data)
	testkit.WriteControl(r.data, testkit.ControlUnready)
	testkit.Eventually(t, 5*time.Second, func() bool {
		s, err := gameserver.ReadStatus(r.data)
		return err == nil && s.State == gameserver.StateStarting
	}, "status file still says ready")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if gameserver.Probe(ctx, "127.0.0.1", st.Ports.Query) == nil {
		t.Fatal("query port still answers after unready")
	}
	select {
	case <-r.done:
		t.Fatal("fake exited after unready")
	case <-time.After(300 * time.Millisecond):
	}
	testkit.WriteControl(r.data, testkit.ControlStop)
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
