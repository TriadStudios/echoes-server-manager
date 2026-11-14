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
	"strings"

	"syscall"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

func TestParseArgs(t *testing.T) {
	file := map[string]string{"FakeReadyDelay": "5s", "FakeNoReady": "true", "FakeStopDelay": "1s"}
	argv := gameserver.Launch{Map: "M", GamePort: 1, QueryPort: 2, MaxPlayers: 3, ServerName: "S EU", WorldName: "W", Listed: true,
		UserDir: "data", LogFile: "logs/server.log", Rules: map[string]string{"PvP": "true"},
		ExtraArgs: []string{"-FakeReadyDelay=10ms", "-FakeIgnoreStop", "-FakeStopDelay=300ms", "-FakeStopDelay=2s"}}.Args()
	m, sw, err := parseArgs(argv, file)
	if err != nil {
		t.Fatal(err)
	}
	o, err := buildOptions(m, sw)
	if err != nil {
		t.Fatal(err)
	}
	if o.mapName != "M" || o.port != 1 || o.queryPort != 2 || o.maxPlayers != 3 || o.serverName != "S EU" || !o.listed ||
		o.worldName != "W" || !filepath.IsAbs(o.userDir) || !filepath.IsAbs(o.logFile) || filepath.Base(o.logFile) != "server.log" ||
		o.readyDelay != 10*time.Millisecond || !o.noReady || !o.ignoreStop || o.exitCode != 1 || o.saveInterval != time.Second ||
		o.stopDelay != 300*time.Millisecond {
		t.Fatalf("options %+v", o)
	}
	if _, _, err := parseArgs([]string{"-A=1", "stray"}, nil); err == nil {
		t.Fatal("stray positional accepted")
	}
	if _, sw, _ := parseArgs([]string{"M", "-Port=x"}, nil); sw.line == "" {
		t.Fatal("no command line")
	} else if _, err := buildOptions("", sw); err == nil {
		t.Fatal("bad port accepted")
	}
	for _, a := range []string{"-MULTIHOME=127.0.0.1", "-multihome=192.0.2.10", "-MultiHome", "-Motd=x;MULTIHOME=192.0.2.10"} {
		if _, _, err := parseArgs([]string{"M", a}, nil); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s accepted: %v", a, err)
		}
	}
}

type fakeRun struct {
	cmd  *exec.Cmd
	data string
	log  string
	done chan error
}

// The passwords the test fakes are started with.
var fakeSecrets = gameserver.Secrets{AdminPassword: `s3cret "admin" $1 -X`}

func startFake(t *testing.T, exe string, extra ...string) *fakeRun {
	t.Helper()
	return startFakeIn(t, exe, filepath.Join(t.TempDir(), "data"), extra...)
}

// testLaunch is the adapter's launch for a fake with data folder data and
// its log next to it.
func testLaunch(data string) gameserver.Launch {
	return gameserver.Launch{Map: "TestMap", GamePort: 7777, QueryPort: 0, MaxPlayers: 8, ServerName: "T", WorldName: "w",
		UserDir: data, LogFile: filepath.Join(filepath.Dir(data), "logs", "server.log")}
}

// startFakeIn starts a fake with the adapter's command line, its passwords
// in a secrets file.
func startFakeIn(t *testing.T, exe, data string, extra ...string) *fakeRun {
	t.Helper()
	l := testLaunch(data)
	l.SecretsFile, l.Secrets = filepath.Join(t.TempDir(), gameserver.SecretsFileName), fakeSecrets
	if err := os.WriteFile(l.SecretsFile, fakeSecrets.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	return startFakeWith(t, exe, l, extra...)
}

func startFakeWith(t *testing.T, exe string, l gameserver.Launch, extra ...string) *fakeRun {
	t.Helper()
	argv := l.Args()
	argv = append(argv, extra...)
	// Like the server, the fake reads the first of two switches of one
	// name: these count only where extra has none of its own.
	argv = append(argv, "-FakeSaveInterval=200ms", "-FakeReadyDelay=100ms")
	cmd := exec.Command(exe, argv...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &fakeRun{cmd: cmd, data: l.UserDir, log: l.LogFile, done: make(chan error, 1)}
	go func() { r.done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-r.done
	})
	return r
}

func (r *fakeRun) logText() string {
	b, _ := os.ReadFile(r.log)
	return string(b)
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

// countLines counts the log lines match takes.
func countLines(text string, match func(string) bool) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if match(line) {
			n++
		}
	}
	return n
}

func serverID(text string) []string {
	var ids []string
	for _, line := range strings.Split(text, "\n") {
		if id, ok := gameserver.ServerID(line); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestFakeLifecycle(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t))
	st := waitReady(t, r.data)
	if st.PID != r.cmd.Process.Pid || st.Ports.Game != 7777 || st.Ports.Query == 0 {
		t.Fatalf("status %+v", st)
	}
	// Like a real server: a socket on the query port only, none on the
	// game port.
	if socks, err := testkit.ProcessSockets(st.PID); err == nil {
		for _, s := range socks {
			if s.Port == 7777 || s.Port != st.Ports.Query {
				t.Errorf("socket %s: the fake has a socket other than the query port", s)
			}
		}
	} else if !errors.Is(err, testkit.ErrSocketsUnsupported) {
		t.Fatal(err)
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
	// A2S_INFO: the challenge step, then the server's name, map and slots.
	info, err := gameserver.QueryInfo(ctx, "127.0.0.1", st.Ports.Query)
	if err != nil || info.Name != "T" || info.Map != "TestMap" || info.MaxPlayers != 8 || info.Players != 0 || info.ServerType != 'd' {
		t.Fatalf("A2S_INFO: %+v %v", info, err)
	}
	// Sent from loopback, like every socket of a test.
	conn, err := net.DialUDP("udp", gameserver.LoopbackLocalAddr("127.0.0.1"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: st.Ports.Query})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	conn.Write(gameserver.InfoRequest(nil))
	reply := make([]byte, 2048)
	n, err := conn.Read(reply)
	if ch, _, perr := gameserver.ParseQueryReply(reply[:n]); err != nil || perr != nil || len(ch) != 4 {
		t.Fatalf("a request without the challenge got %q (%v %v), want the challenge", reply[:n], err, perr)
	}
	save := testkit.FakeSavePath(r.data, "TestMap", "w")
	testkit.Eventually(t, 5*time.Second, func() bool { _, err := os.Stat(save + ".bak2"); return err == nil }, "no rotated save files")
	if err := testkit.WriteControl(r.data, testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 5*time.Second, func() bool {
		return strings.Count(r.logText(), "Command line: ") == 2
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
	text := r.logText()
	if countLines(text, gameserver.IsReadyLine) != 1 || countLines(text, gameserver.IsWorldSavedLine) < 3 ||
		countLines(text, gameserver.IsWorldResumedLine) != 0 || len(serverID(text)) != 1 {
		t.Fatalf("log misses ready, saved or server id lines, or says resumed:\n%s", text)
	}
	if !strings.Contains(text, gameserver.FlushedLine) || !strings.Contains(text, gameserver.NewWorldLine("w")) {
		t.Fatalf("log misses the flush or new world line:\n%s", text)
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
	want := []string{"Saved/SaveGames/Fake/TestMap__w.json", "Saved/SaveGames/Fake/TestMap__w.json.bak1",
		"Saved/SaveGames/Fake/TestMap__w.json.bak2", "Saved/SaveGames/Fake/TestMap__w.meta.json"}
	if !slices.Equal(files, want) {
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
	secrets := gameserver.Secrets{ServerPassword: "join me 7", AdminPassword: "adm1n-pw"}
	l := testLaunch(filepath.Join(t.TempDir(), "data"))
	l.Delivery, l.Secrets = gameserver.DeliveryCommandLine, secrets
	r := startFakeWith(t, testkit.FakeServer(t), l)
	st := waitReady(t, r.data)
	if !st.ServerPasswordSet || !st.AdminPasswordSet {
		t.Fatalf("status %+v", st)
	}
	checkPasswords(t, st.Ports.Query, secrets)
	if !strings.Contains(r.logText(), "-Password=join me 7 -AdminPassword=adm1n-pw -FakeSaveInterval") {
		t.Fatalf("the command line in the log:\n%s", r.logText())
	}
	status, _ := os.ReadFile(gameserver.StatusFilePath(r.data))
	if strings.Contains(string(status), "join me") || strings.Contains(string(status), "adm1n") {
		t.Fatalf("the status file has a password:\n%s", status)
	}
}

// A fake without any password lets everybody in.
func TestFakeWithoutPasswords(t *testing.T) {
	file := filepath.Join(t.TempDir(), gameserver.SecretsFileName)
	if err := os.WriteFile(file, gameserver.Secrets{}.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	l := testLaunch(filepath.Join(t.TempDir(), "data"))
	l.SecretsFile = file
	r := startFakeWith(t, testkit.FakeServer(t), l)
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
	base := testLaunch(filepath.Join(dir, "data")).Args()
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

// A fake that finds a save of its world says it resumes it and carries on
// with its counter.
func TestFakeResumesSavedWorld(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if err := testkit.WriteFakeSave(data, "TestMap", "w", 41); err != nil {
		t.Fatal(err)
	}
	r := startFakeIn(t, testkit.FakeServer(t), data, "-FakeSaveInterval=1h")
	waitReady(t, r.data)
	testkit.WriteControl(r.data, testkit.ControlStop)
	r.wait(t, 10*time.Second)
	// One save at start, one at the stop.
	if n := testkit.FakeSaveCounter(data, "TestMap", "w"); n != 43 {
		t.Fatalf("final save counter %d, want 43", n)
	}
	text := r.logText()
	if countLines(text, gameserver.IsWorldResumedLine) != 1 || strings.Contains(text, gameserver.NewWorldLine("w")) {
		t.Fatalf("the fake did not say it resumed the world:\n%s", text)
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
	if !strings.Contains(r.logText(), "Ignoring stop request") {
		t.Fatalf("no ignore line:\n%s", r.logText())
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

// One SIGINT: the fake saves, confirms the flush and exits with the real
// server's code after its stop delay.
func TestFakeSIGINT(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals cannot be sent on Windows; the stop file is used there")
	}
	r := startFake(t, testkit.FakeServer(t), "-FakeSaveInterval=1h")
	waitReady(t, r.data)
	if err := r.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := r.wait(t, 10*time.Second); code != gameserver.StopExitCode {
		t.Fatalf("exit %d after SIGINT", code)
	}
	text := r.logText()
	if testkit.SecondSignal(r.data) || testkit.FakeSaveCounter(r.data, "TestMap", "w") != 2 || !strings.Contains(text, gameserver.FlushedLine) {
		t.Fatalf("not one clean stop:\n%s", text)
	}
	// SIGTERM is treated the same, with the shell's code for it.
	r = startFake(t, testkit.FakeServer(t))
	waitReady(t, r.data)
	r.cmd.Process.Signal(syscall.SIGTERM)
	if code := r.wait(t, 10*time.Second); code != 143 {
		t.Fatalf("exit %d after SIGTERM", code)
	}
}

// A second signal is recorded and ends the fake at once with its own code,
// so a test that sent one fails loudly.
func TestFakeSecondSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals cannot be sent on Windows; the stop file is used there")
	}
	for _, extra := range [][]string{{"-FakeStopDelay=5s"}, {"-FakeIgnoreStop"}} {
		r := startFake(t, testkit.FakeServer(t), extra...)
		waitReady(t, r.data)
		r.cmd.Process.Signal(syscall.SIGINT)
		testkit.Eventually(t, 5*time.Second, func() bool {
			return strings.Contains(r.logText(), "Stop requested (interrupt)") || strings.Contains(r.logText(), "Ignoring stop request (interrupt)")
		}, "%v: the first signal did not arrive", extra)
		if testkit.SecondSignal(r.data) {
			t.Fatalf("%v: one signal recorded as two", extra)
		}
		r.cmd.Process.Signal(syscall.SIGINT)
		if code := r.wait(t, 10*time.Second); code != testkit.SecondSignalExitCode {
			t.Fatalf("%v: exit %d after a second signal", extra, code)
		}
		if !testkit.SecondSignal(r.data) {
			t.Fatalf("%v: the second signal is not recorded", extra)
		}
	}
}

// Every start logs a new Steam server id.
func TestFakeNewServerIDEachStart(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	var ids []string
	for range 2 {
		r := startFakeIn(t, testkit.FakeServer(t), data)
		waitReady(t, r.data)
		testkit.WriteControl(r.data, testkit.ControlStop)
		r.wait(t, 10*time.Second)
		ids = serverID(r.logText())
	}
	if len(ids) != 2 || ids[0] == ids[1] || len(ids[0]) != 17 {
		t.Fatalf("server ids of two starts: %q", ids)
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
	l := testLaunch(filepath.Join(t.TempDir(), "data"))
	l.QueryPort = port
	r := startFakeWith(t, testkit.FakeServer(t), l, "-FakeReadyDelay=1h", "-FakeNoStatusFile", "-FakeNoQuery")
	readyLines := func() int { return countLines(r.logText(), gameserver.IsReadyLine) }
	testkit.Eventually(t, 10*time.Second, func() bool {
		return strings.Contains(r.logText(), "Command line: ")
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

// Told to, the fake becomes ready without its ready line: only the query
// port says so.
func TestFakeReadyWithoutLine(t *testing.T) {
	r := startFake(t, testkit.FakeServer(t), "-FakeNoReadyLine")
	st := waitReady(t, r.data)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gameserver.Probe(ctx, "127.0.0.1", st.Ports.Query); err != nil {
		t.Fatalf("query probe: %v", err)
	}
	if n := countLines(r.logText(), gameserver.IsReadyLine); n != 0 {
		t.Fatalf("%d ready lines", n)
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

// -MULTIHOME stops the fake with exit code 2 before anything is written or
// bound: ESM must never pass it.
func TestFakeRefusesMultihome(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	argv := append(testLaunch(data).Args(), "-MULTIHOME=127.0.0.1")
	out, err := exec.Command(testkit.FakeServer(t), argv...).CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "-MULTIHOME is refused") {
		t.Fatalf("-MULTIHOME: %v\n%s", err, out)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("a refused start wrote a data folder: %v", err)
	}
}

// The fake reads the line its arguments are joined into, as the server
// does, not argument by argument: a switch inside the value of another one
// is found there, the first one counts, and an empty value takes the next
// argument.
func TestFakeReadsTheJoinedCommandLine(t *testing.T) {
	exe := testkit.FakeServer(t)
	l := testLaunch(filepath.Join(t.TempDir(), "data"))
	l.Delivery = gameserver.DeliveryCommandLine
	l.Secrets = gameserver.Secrets{ServerPassword: "join-pw-1", AdminPassword: "real-adm1n"}
	l.Rules = map[string]string{"MOTD": "hi;AdminPassword=sneaky99"}
	r := startFakeWith(t, exe, l)
	checkPasswords(t, waitReady(t, r.data).Ports.Query, gameserver.Secrets{ServerPassword: "join-pw-1", AdminPassword: "sneaky99"})

	l = testLaunch(filepath.Join(t.TempDir(), "data"))
	l.Delivery = gameserver.DeliveryCommandLine
	r = startFakeWith(t, exe, l, "-AdminPassword=", "-Password=join-pw-2")
	checkPasswords(t, waitReady(t, r.data).Ports.Query, gameserver.Secrets{ServerPassword: "join-pw-2", AdminPassword: "-Password=join-pw-2"})
}

// The fake finds a password switch the way the game server does: the join
// password is "Password=", which -ServerPassword= and -AdminPassword= do
// not match.
func TestFakeFindsSwitchesLikeTheServer(t *testing.T) {
	read := func(args ...string) gameserver.Secrets {
		t.Helper()
		_, sw, err := parseArgs(append([]string{"M"}, args...), nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := readSecrets(sw)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := read("-ServerPassword=not-this", "-AdminPassword=admin-pw"); s.ServerPassword != "" || s.AdminPassword != "admin-pw" {
		t.Fatalf("%+v", s)
	}
	if s := read("-ServerPassword=not-this", "-AdminPassword=admin-pw", "-password=join-pw"); s.ServerPassword != "join-pw" {
		t.Fatalf("%+v", s)
	}
}
