package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
	argv := gameserver.Launch{Map: "M", GamePort: 1, QueryPort: 2, MaxPlayers: 3, ServerName: "S", WorldName: "W",
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

// The passwords the test fakes are started with.
var fakeSecrets = gameserver.Secrets{AdminPassword: `s3cret "admin" $1 -X`}

func startFake(t *testing.T, exe string, extra ...string) *fakeRun {
	t.Helper()
	return startFakeIn(t, exe, filepath.Join(t.TempDir(), "data"), extra...)
}

// startFakeIn starts a fake with the adapter's command line, its passwords
// in a secrets file.
func startFakeIn(t *testing.T, exe, data string, extra ...string) *fakeRun {
	t.Helper()
	l := gameserver.Launch{GamePort: 0, QueryPort: 0, BindIP: "127.0.0.1", MaxPlayers: 8, ServerName: "T",
		WorldName: "w", DataDir: data, SecretsFile: filepath.Join(t.TempDir(), gameserver.SecretsFileName), Secrets: fakeSecrets}
	if err := os.WriteFile(l.SecretsFile, fakeSecrets.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	return startFakeWith(t, exe, data, l, extra...)
}

func startFakeWith(t *testing.T, exe, data string, l gameserver.Launch, extra ...string) *fakeRun {
	t.Helper()
	argv := l.Args()
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
	// The server listens on loopback only, and so does this test.
	testkit.CheckLoopback(t, st.PID, os.Getpid())
	return st
}

func TestFakeLifecycle(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t))
	st := waitReady(t, r.data)
	if st.PID != r.cmd.Process.Pid || st.Ports.Game == 0 || st.Ports.Query == 0 || st.Ports.Game == st.Ports.Query {
		t.Fatalf("status %+v", st)
	}
	// The passwords came from the secrets file.
	if st.ServerPasswordSet || !st.AdminPasswordSet {
		t.Fatalf("status says the wrong passwords are set: %+v", st)
	}
	checkPasswords(t, st.Ports.Query, fakeSecrets)
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
	ready, saved := false, false
	for _, line := range strings.Split(text, "\n") {
		ready = ready || gameserver.IsReadyLine(line)
		saved = saved || gameserver.IsWorldSavedLine(line)
	}
	if !ready || !saved {
		t.Fatalf("log misses ready or saved lines:\n%s", text)
	}
	// Like a real server, the fake prints its whole command line. With a
	// secrets file there is no password on it.
	if !strings.Contains(text, "Command line: ") || !strings.Contains(text, " "+gameserver.SecretsFileSwitch+"=") {
		t.Fatalf("log misses the command line:\n%s", text)
	}
	if strings.Contains(text, "s3cret") || strings.Contains(strings.ToLower(text), "password") {
		t.Fatalf("the log has a password or a password switch:\n%s", text)
	}
	files, _ := gameserver.SaveFiles(r.data)
	if len(files) != 1 {
		t.Fatalf("save files %v", files)
	}
}

// checkPasswords asks the fake behind the query port which passwords it
// has, and fails unless they are exactly want.
func checkPasswords(t *testing.T, queryPort int, want gameserver.Secrets) {
	t.Helper()
	for kind, pw := range map[string]string{testkit.JoinPassword: want.ServerPassword, testkit.AdminPassword: want.AdminPassword} {
		if ok, err := testkit.CheckPassword("127.0.0.1", queryPort, kind, pw); err != nil || !ok {
			t.Errorf("%s with the right password: %v %v", kind, ok, err)
		}
		for _, wrong := range []string{pw + "x", "wrong", strings.TrimSuffix(pw, "X") + " "} {
			if ok, err := testkit.CheckPassword("127.0.0.1", queryPort, kind, wrong); err != nil || ok {
				t.Errorf("%s with a wrong password: %v %v", kind, ok, err)
			}
		}
	}
}

// With command-line delivery the fake takes its passwords from the
// switches, and prints them with its command line: that is what makes this
// way unsafe.
func TestFakePasswordsOnCommandLine(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	secrets := gameserver.Secrets{ServerPassword: "join me 7", AdminPassword: "adm1n-pw"}
	r := startFakeWith(t, testkit.FakeServer(t), data, gameserver.Launch{BindIP: "127.0.0.1", MaxPlayers: 8, ServerName: "T", WorldName: "w",
		DataDir: data, Delivery: gameserver.DeliveryCommandLine, Secrets: secrets})
	st := waitReady(t, r.data)
	if !st.ServerPasswordSet || !st.AdminPasswordSet {
		t.Fatalf("status %+v", st)
	}
	checkPasswords(t, st.Ports.Query, secrets)
	log, _ := os.ReadFile(gameserver.ServerLogPath(r.data))
	if !strings.Contains(string(log), "-ServerPassword=join me 7 -AdminPassword=adm1n-pw -log") {
		t.Fatalf("the command line in the log:\n%s", log)
	}
	status, _ := os.ReadFile(gameserver.StatusFilePath(r.data))
	if strings.Contains(string(status), "join me") || strings.Contains(string(status), "adm1n") {
		t.Fatalf("the status file has a password:\n%s", status)
	}
}

// A fake without any password lets everybody in.
func TestFakeWithoutPasswords(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	file := filepath.Join(t.TempDir(), gameserver.SecretsFileName)
	if err := os.WriteFile(file, gameserver.Secrets{}.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startFakeWith(t, testkit.FakeServer(t), data, gameserver.Launch{BindIP: "127.0.0.1", MaxPlayers: 8, ServerName: "T", WorldName: "w",
		DataDir: data, SecretsFile: file})
	st := waitReady(t, r.data)
	if st.ServerPasswordSet || st.AdminPasswordSet {
		t.Fatalf("status %+v", st)
	}
	checkPasswords(t, st.Ports.Query, gameserver.Secrets{})
}

// Told to behave like a build from before the secrets file, the fake
// ignores the file and runs without passwords.
func TestFakeIgnoresSecretsFileOnRequest(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t), "-FakeIgnoreSecretsFile")
	st := waitReady(t, r.data)
	if st.ServerPasswordSet || st.AdminPasswordSet {
		t.Fatalf("status %+v", st)
	}
	checkPasswords(t, st.Ports.Query, gameserver.Secrets{})
}

// With the secrets file switch the fake accepts no password switch, and a
// secrets file it cannot read stops it. Its messages never repeat a value.
func TestFakeRefusesBadSecrets(t *testing.T) {
	exe := testkit.FakeServer(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(good, gameserver.Secrets{ServerPassword: "from-file-9"}.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(`{"server_password": Zq7-not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := gameserver.Launch{BindIP: "127.0.0.1", MaxPlayers: 8, ServerName: "T", WorldName: "w", DataDir: filepath.Join(dir, "data")}.Args()
	for what, extra := range map[string][]string{
		"server password next to the file": {gameserver.SecretsFileSwitch + "=" + good, gameserver.ServerPasswordSwitch + "=Zq7-on-argv"},
		"admin password next to the file":  {gameserver.AdminPasswordSwitch + "=Zq7-on-argv", gameserver.SecretsFileSwitch + "=" + good},
		"another password switch":          {gameserver.SecretsFileSwitch + "=" + good, "-RconPassword=Zq7-on-argv"},
		"a missing file":                   {gameserver.SecretsFileSwitch + "=" + filepath.Join(dir, "missing.json")},
		"a file that is not JSON":          {gameserver.SecretsFileSwitch + "=" + bad},
	} {
		out, err := exec.Command(exe, append(slices.Clone(base), extra...)...).CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Errorf("%s: %v, want exit code 2\n%s", what, err, out)
		}
		if strings.Contains(string(out), "Zq7") || strings.Contains(string(out), "from-file-9") || !strings.Contains(string(out), "esm-fakeserver: ") {
			t.Errorf("%s: output %q", what, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
		t.Errorf("a refused start wrote a data folder: %v", err)
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

// With no status file and no query answers, the only sign of ready is the
// ready line in the log, and the "ready" request file makes the fake write
// it.
func TestFakeReadyOnRequest(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	r := startFake(t, testkit.FakeServer(t), "-FakeReadyDelay=1h", "-FakeNoStatusFile", "-FakeNoQuery", "-QueryPort="+strconv.Itoa(port))
	readyLines := func() int {
		b, _ := os.ReadFile(gameserver.ServerLogPath(r.data))
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if gameserver.IsReadyLine(line) {
				n++
			}
		}
		return n
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		b, _ := os.ReadFile(gameserver.ServerLogPath(r.data))
		return strings.Contains(string(b), "Command line: ")
	}, "the fake did not start")
	time.Sleep(300 * time.Millisecond)
	if readyLines() != 0 {
		t.Fatal("ready before it was asked for")
	}
	if err := testkit.WriteControl(r.data, testkit.ControlReady); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 5*time.Second, func() bool { return readyLines() == 1 }, "no ready line after the request")
	if _, err := os.Stat(gameserver.StatusFilePath(r.data)); !os.IsNotExist(err) {
		t.Fatalf("a status file was written: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if gameserver.Probe(ctx, "127.0.0.1", port) == nil {
		t.Fatal("the query port answers")
	}
	testkit.WriteControl(r.data, testkit.ControlStop)
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Fatalf("exit %d", code)
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

// The fake binds loopback only: 127.0.0.1 without -MULTIHOME, and any
// address that is not loopback is refused before anything is bound.
func TestFakeBindsLoopbackOnly(t *testing.T) {
	for host, want := range map[string]string{"": "127.0.0.1", "127.0.0.1": "127.0.0.1", "::1": "::1", "127.0.0.2": "127.0.0.2"} {
		o, err := buildOptions("", map[string]string{"MULTIHOME": host})
		if err != nil || o.multihome != want {
			t.Errorf("-MULTIHOME=%q: %q %v, want %q", host, o.multihome, err, want)
		}
	}
	for _, host := range []string{"0.0.0.0", "::", "192.0.2.10", "example.com"} {
		if _, err := buildOptions("", map[string]string{"MULTIHOME": host}); err == nil || !strings.Contains(err.Error(), "loopback addresses only") {
			t.Errorf("-MULTIHOME=%q accepted: %v", host, err)
		}
	}
	// Without -MULTIHOME it runs and answers on 127.0.0.1.
	data := filepath.Join(t.TempDir(), "data")
	r := startFakeWith(t, testkit.FakeServer(t), data, gameserver.Launch{MaxPlayers: 8, ServerName: "T", WorldName: "w", DataDir: data})
	st := waitReady(t, r.data)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gameserver.Probe(ctx, "127.0.0.1", st.Ports.Query); err != nil {
		t.Fatalf("query port on 127.0.0.1: %v", err)
	}
	// An address that is not loopback stops it with exit code 2. (Were the
	// refusal gone, binding this documentation address would fail anyway.)
	out, err := exec.Command(testkit.FakeServer(t), gameserver.Launch{BindIP: "192.0.2.10", MaxPlayers: 8, ServerName: "T", WorldName: "w",
		DataDir: filepath.Join(t.TempDir(), "data")}.Args()...).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "-MULTIHOME=192.0.2.10: the fake server binds loopback addresses only") {
		t.Fatalf("-MULTIHOME=192.0.2.10: %v\n%s", err, out)
	}
}
