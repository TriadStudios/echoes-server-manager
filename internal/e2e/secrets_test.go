package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

const (
	leakServerPW = "open sesame 42"
	leakAdminPW  = "adm1n-secret"
)

// Passwords with spaces and shell, URL and JSON punctuation, one with
// something that looks like a switch inside. Before and after a change.
const (
	oldServerPW = `ovs7 Qq$&"';%,=<>1 zk-old9`
	oldAdminPW  = `oad4 -Wy8 $&"';%,=<>2 jm-old3`
	newServerPW = `nvs5 Rr$&"';%,=<>3 xp-new6`
	newAdminPW  = `nad2 -Vb6 $&"';%,=<>4 hq-new8`
)

// Passwords with "$" for a systemd that needs them doubled.
const (
	dollarServerPW = `sd$v7 $Kq9 w$&"x 3`
	dollarAdminPW  = "ad$m-$ecret"
)

// passwordPieces lists each password and every space-separated piece of
// it that is at least 4 bytes long: none of them may ever be shown.
func passwordPieces(passwords ...string) []string {
	var out []string
	for _, pw := range passwords {
		out = append(out, pw)
		for _, f := range strings.Fields(pw) {
			if len(f) >= 4 && f != pw {
				out = append(out, f)
			}
		}
	}
	return out
}

// noPieces fails the test if raw, or any string inside it when it (or one
// of its lines) is JSON, so escaped characters are compared as they are
// meant, contains one of pieces.
func noPieces(t *testing.T, what string, raw []byte, pieces []string) {
	t.Helper()
	texts := []string{string(raw)}
	for _, part := range append([]string{string(raw)}, strings.Split(string(raw), "\n")...) {
		var v any
		if json.Unmarshal([]byte(part), &v) == nil {
			texts = jsonStrings(v, texts)
		}
	}
	for _, s := range texts {
		for _, p := range pieces {
			if strings.Contains(s, p) {
				t.Errorf("the %s shows %q of a password:\n%s", what, p, s)
				return
			}
		}
	}
}

func jsonStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = jsonStrings(e, out)
		}
	case map[string]any:
		for _, e := range x {
			out = jsonStrings(e, out)
		}
	}
	return out
}

// brokenBuild makes a build whose executables are text files, so starting
// it fails inside the supervisor.
func brokenBuild(t *testing.T, parent, version string) string {
	t.Helper()
	dir := filepath.Join(parent, version)
	files := map[string]string{"bin/server": "not a program\n", "bin/server.exe": "not a program\n"}
	sums := map[string]string{}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		sums[rel] = hex.EncodeToString(sum[:])
	}
	m := map[string]any{"version": version, "commit": "broken", "channel": "live",
		"executables": map[string]string{"linux": "bin/server", "windows": "bin/server.exe"}, "files": sums}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeSystemdRun puts a systemd-run on PATH that fails and repeats its
// whole command line, the worst a supervisor's tool could do. It records
// its arguments, one per line, in dir/args, and returns dir.
func fakeSystemdRun(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + dir + "/args'\n" +
		"echo \"Failed to start transient service unit: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeOldSystemd puts a systemctl on PATH that reports systemd 252 (older
// than --expand-environment, so ESM doubles every "$" of the command line)
// and hands every other call to the real one, and a systemd-run that fails,
// records its arguments in dir/args and repeats them: whole, and each value
// without its switch. It returns dir.
func fakeOldSystemd(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("systemctl")
	if err != nil {
		t.Skip("no systemctl")
	}
	dir := t.TempDir()
	scripts := map[string]string{
		"systemctl": "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'systemd 252 (252.19-1)'; exit 0; fi\nexec '" + real + "' \"$@\"\n",
		"systemd-run": "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + dir + "/args'\n" +
			"echo \"Failed to start transient service unit: $*\" >&2\n" +
			"for a in \"$@\"; do case \"$a\" in *=*) echo \"invalid value: ${a#*=}\" >&2;; esac; done\nexit 1\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// A start that fails inside the supervisor never shows the passwords: not
// in the answer, the status, the events, the audit log or the service log.
func TestStartFailureHidesPasswords(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	dir := brokenBuild(t, h.src, "0.9-broken")
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, nil)
	startFailureHidesPasswords(t, h, "alpha", "0.9-broken", leakServerPW, leakAdminPW, []string{leakServerPW, leakAdminPW, "sesame"})
}

// The same when systemd-run fails and repeats its whole command line: the
// passwords were never on it.
func TestStartFailureHidesPasswordsSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	dir := fakeSystemdRun(t)
	startFailureHidesPasswords(t, h, unitSafeName("sdpw"), "1.0", leakServerPW, leakAdminPW, []string{leakServerPW, leakAdminPW, "sesame"})
	noPasswordsInArgs(t, dir, []string{leakServerPW, leakAdminPW, "sesame"})
}

// On systemd versions without --expand-environment ESM doubles every "$"
// of the command line. Passwords with "$" are not on it either, in any
// form.
func TestStartFailureHidesPasswordsOldSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	if os.Getenv("ESM_TEST_SYSTEMD") != "1" {
		t.Skip("set ESM_TEST_SYSTEMD=1 to run tests that start systemd user units")
	}
	dir := fakeOldSystemd(t)
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	pieces := passwordPieces(dollarServerPW, dollarAdminPW,
		strings.ReplaceAll(dollarServerPW, "$", "$$"), strings.ReplaceAll(dollarAdminPW, "$", "$$"))
	startFailureHidesPasswords(t, h, unitSafeName("sdold"), "1.0", dollarServerPW, dollarAdminPW, pieces)
	noPasswordsInArgs(t, dir, pieces)
}

// noPasswordsInArgs checks the arguments a fake systemd-run recorded in
// dir/args: the secrets file switch is among them, no password switch and
// no piece of a password is.
func noPasswordsInArgs(t *testing.T, dir string, pieces []string) {
	t.Helper()
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatalf("the fake systemd-run was not used: %v", err)
	}
	if !strings.Contains(string(args), "\n"+gameserver.SecretsFileSwitch+"=") || strings.Contains(strings.ToLower(string(args)), "password") {
		t.Errorf("systemd-run got a password switch, or no secrets file switch:\n%s", args)
	}
	noPieces(t, "arguments of systemd-run", args, pieces)
}

func startFailureHidesPasswords(t *testing.T, h *harness, name, build, serverPW, adminPW string, pieces []string) {
	h.create(name, build, nil)
	h.must(http.StatusOK, "PUT", "/api/v1/instances/"+name+"/secrets",
		map[string]string{"server_password": serverPW, "admin_password": adminPW}, nil)
	s := h.openEvents("", h.admin, "")
	var raw json.RawMessage
	st := h.call("POST", "/api/v1/instances/"+name+"/start?wait=true", h.admin, struct{}{}, &raw)
	var eb api.ErrorBody
	json.Unmarshal(raw, &eb)
	if st != http.StatusUnprocessableEntity || eb.Error.Code != "start_failed" {
		t.Fatalf("start of a build that cannot run: %d %s", st, raw)
	}
	var status json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/status", nil, &status)
	var seen []events.Event
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case e := <-s.events:
			<-s.names
			seen = append(seen, e)
		case <-deadline:
			break collect
		}
	}
	evRaw, _ := json.Marshal(seen)
	auditLog, err := os.ReadFile(filepath.Join(h.root, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{
		"error answer": string(raw), "status": string(status), "events": string(evRaw),
		"audit log": string(auditLog), "service log": h.stderr.String(),
	} {
		noPieces(t, what, []byte(text), pieces)
		noLaunchLine(t, what, text)
	}
	if !strings.Contains(string(auditLog), `"action":"instance.start"`) {
		t.Fatalf("the failed start was not audited:\n%s", auditLog)
	}
	if _, err := os.Stat(h.runSecretsPath(name)); !os.IsNotExist(err) {
		t.Fatalf("the secrets file is still there after the start failed: %v", err)
	}
}

// noLaunchLine fails if text has a piece of a server command line. ESM
// never writes one itself, and cuts it out of what a supervisor's tool
// printed.
func noLaunchLine(t *testing.T, what, text string) {
	t.Helper()
	for _, part := range []string{"-Port=", "-QueryPort=", "-ServerName=", "-WorldName=", gameserver.SecretsFileSwitch + "=",
		gameserver.DataDirSwitch + "=", gameserver.ServerPasswordSwitch, gameserver.AdminPasswordSwitch} {
		if strings.Contains(text, part) {
			t.Errorf("the %s has %q of a server command line:\n%s", what, part, text)
		}
	}
}

// eventLog records every event of a stream in the background.
type eventLog struct {
	mu   sync.Mutex
	list []events.Event
}

func (h *harness) recordEvents() *eventLog {
	h.t.Helper()
	return h.recordEventsAs(h.admin, "")
}

// recordEventsAs records the event stream a token gets, from now on, or
// with lastID from the event after that id.
func (h *harness) recordEventsAs(token, lastID string) *eventLog {
	h.t.Helper()
	s := h.openEvents("", token, lastID)
	l := &eventLog{}
	go func() {
		for e := range s.events {
			<-s.names
			l.mu.Lock()
			l.list = append(l.list, e)
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *eventLog) all() []events.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]events.Event(nil), l.list...)
}

// commandLines counts the log events of source that carry the server's
// command line.
func (l *eventLog) commandLines(source string) int {
	n := 0
	for _, e := range l.all() {
		var ll instances.LogLine
		if e.Type == events.TypeLog && json.Unmarshal(e.Data, &ll) == nil && ll.Source == source &&
			strings.Contains(ll.Line, "Command line: ") {
			n++
		}
	}
	return n
}

// waitCommandLines waits until both log sources have shown the command
// line n times.
func (l *eventLog) waitCommandLines(t *testing.T, n int) {
	t.Helper()
	testkit.Eventually(t, 10*time.Second, func() bool {
		return l.commandLines(instances.SourceServer) >= n && l.commandLines(instances.SourceConsole) >= n
	}, "the command line did not show %d times in the log events", n)
}

// runSecretsPath is the secrets file a running server of name was started
// with.
func (h *harness) runSecretsPath(name string) string {
	return filepath.Join(h.root, "instances", name, "run", gameserver.SecretsFileName)
}

// runSecrets reads the secrets file of a running server.
func (h *harness) runSecrets(name string) (gameserver.Secrets, error) {
	return gameserver.ReadSecretsFile(h.runSecretsPath(name))
}

// rawOutput returns what the server of an instance wrote, as it is on
// disk: its log and its console output.
func (h *harness) rawOutput(name string) string {
	h.t.Helper()
	log, _ := os.ReadFile(gameserver.ServerLogPath(h.dataDir(name)))
	console, _ := os.ReadFile(filepath.Join(h.root, "instances", name, "console.log"))
	return string(log) + "\n" + string(console)
}

// serverHas asks the running fake of an instance whether these are its
// passwords.
func (h *harness) serverHas(v instances.View, want gameserver.Secrets) bool {
	h.t.Helper()
	join, err := testkit.CheckPassword(v.BindIP, v.Ports.Query, testkit.JoinPassword, want.ServerPassword)
	if err != nil {
		h.t.Fatal(err)
	}
	admin, err := testkit.CheckPassword(v.BindIP, v.Ports.Query, testkit.AdminPassword, want.AdminPassword)
	if err != nil {
		h.t.Fatal(err)
	}
	return join && admin
}

// token makes an API token with the given role.
func (h *harness) token(role string) string {
	h.t.Helper()
	var tok api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: role + " for tests", Role: role}, &tok)
	return tok.Token
}

// checkNothingToHide fetches everything ESM shows about an instance, with
// a read token where the read role may look (log tails of both sources,
// status, settings, the list) and fails if any piece of a password shows,
// there or in the recorded events, the service log, the audit log or the
// files the server wrote itself. Each log tail must have the server's
// command line, with the secrets file switch and without anything masked:
// nothing secret was ever in it.
func (h *harness) checkNothingToHide(t *testing.T, name string, ev *eventLog, pieces []string) {
	t.Helper()
	read := h.token("read")
	for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
		var raw json.RawMessage
		if st := h.call("GET", "/api/v1/instances/"+name+"/logs?lines=2000&source="+source, read, nil, &raw); st != http.StatusOK {
			t.Fatalf("%s log with a read token: %d %s", source, st, raw)
		}
		noPieces(t, source+" log", raw, pieces)
		var logs api.Logs
		json.Unmarshal(raw, &logs)
		text := strings.Join(logs.Lines, "\n")
		if !strings.Contains(text, "Command line: ") || !strings.Contains(text, " "+gameserver.SecretsFileSwitch+"=") {
			t.Errorf("the %s log has no command line with the secrets file switch:\n%s", source, text)
		}
		if strings.Contains(strings.ToLower(text), "password") || strings.Contains(text, gameserver.Mask) {
			t.Errorf("the %s log has a password switch, or something masked:\n%s", source, text)
		}
	}
	for _, path := range []string{"/instances/" + name + "/status", "/instances/" + name, "/instances", "/box", "/box/doctor"} {
		var raw json.RawMessage
		if st := h.call("GET", "/api/v1"+path, read, nil, &raw); st != http.StatusOK {
			t.Fatalf("GET %s with a read token: %d %s", path, st, raw)
		}
		noPieces(t, "answer of GET "+path, raw, pieces)
	}
	if ev != nil {
		evRaw, err := json.Marshal(ev.all())
		if err != nil {
			t.Fatal(err)
		}
		noPieces(t, "events", evRaw, pieces)
	}
	noPieces(t, "service log", []byte(h.stderr.String()), pieces)
	auditLog, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
	noPieces(t, "audit log", auditLog, pieces)
	raw := h.rawOutput(name)
	noPieces(t, "files the server wrote", []byte(raw), pieces)
	if strings.Contains(strings.ToLower(raw), "password") {
		t.Errorf("the files the server wrote have a password switch:\n%s", raw)
	}
	state, _ := os.ReadFile(filepath.Join(h.root, "instances", name, "run", "state.json"))
	pid, _ := os.ReadFile(filepath.Join(h.root, "instances", name, "run", "pid.json"))
	noPieces(t, "state.json and pid.json", append(state, pid...), pieces)
	// ESM itself never writes a command line anywhere.
	for what, text := range map[string]string{"service log": h.stderr.String(), "state.json": string(state), "pid.json": string(pid), "audit log": string(auditLog)} {
		for _, part := range []string{"-Port=", "-QueryPort=", gameserver.SecretsFileSwitch, gameserver.DataDirSwitch + "="} {
			if strings.Contains(text, part) {
				t.Errorf("the %s has %q of a command line:\n%s", what, part, text)
			}
		}
	}
}

func (h *harness) setPasswords(name, server, admin string) api.SecretsResult {
	h.t.Helper()
	var sr api.SecretsResult
	h.must(http.StatusOK, "PUT", "/api/v1/instances/"+name+"/secrets",
		map[string]string{"server_password": server, "admin_password": admin}, &sr)
	return sr
}

// procCommandLine returns the command line of a running process as other
// accounts on a Linux box can read it, or "" where that is not possible.
func procCommandLine(pid int) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return strings.ReplaceAll(string(b), "\x00", " ")
}

// A server gets its passwords in the secrets file: a private file that is
// there while the server runs and gone when it has stopped. Its command
// line, which every account on the box can read, has no password.
func TestSecretsFileDelivery(t *testing.T) {
	t.Parallel()
	secretsFileDelivery(t, newESM(t), "alpha")
}

func TestSecretsFileDeliverySystemd(t *testing.T) {
	secretsFileDelivery(t, newSystemdESM(t), unitSafeName("sdsf"))
}

func secretsFileDelivery(t *testing.T, h *harness, name string) {
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create(name, "", nil)
	want := gameserver.Secrets{ServerPassword: dollarServerPW, AdminPassword: oldAdminPW}
	pieces := passwordPieces(want.ServerPassword, want.AdminPassword)
	h.setPasswords(name, want.ServerPassword, want.AdminPassword)
	if _, err := os.Stat(h.runSecretsPath(name)); !os.IsNotExist(err) {
		t.Fatalf("a secrets file exists before the start: %v", err)
	}
	ev := h.recordEvents()
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, &st)
	ev.waitCommandLines(t, 1)

	got, err := h.runSecrets(name)
	if err != nil || got != want {
		t.Fatalf("the secrets file while the server runs: %+v %v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(h.runSecretsPath(name))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("the secrets file has mode %v (%v), want 600", fi.Mode().Perm(), err)
		}
	}
	gs, err := gameserver.ReadStatus(h.dataDir(name))
	if err != nil || !gs.ServerPasswordSet || !gs.AdminPasswordSet {
		t.Fatalf("the server does not say it has both passwords: %+v %v", gs, err)
	}
	if !h.serverHas(v.View, want) || h.serverHas(v.View, gameserver.Secrets{}) {
		t.Fatal("the server does not have exactly the passwords of the secrets file")
	}
	if line := procCommandLine(st.PID); runtime.GOOS == "linux" {
		if !strings.Contains(line, gameserver.SecretsFileSwitch+"="+h.runSecretsPath(name)) || strings.Contains(strings.ToLower(line), "password") {
			t.Fatalf("the command line of the running server: %s", line)
		}
		noPieces(t, "command line of the running server", []byte(line), pieces)
	}
	h.checkNothingToHide(t, name, ev, pieces)

	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/stop", struct{}{}, &st)
	if _, err := os.Stat(h.runSecretsPath(name)); !os.IsNotExist(err) {
		t.Fatalf("the secrets file is still there after the stop: %v", err)
	}
}

// Passwords changed while their server runs never show, old or new,
// wherever ESM shows text and in what the server wrote itself. The running
// server keeps the old ones; after a restart it has the new ones.
func TestChangedPasswordsNeverShow(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create("alpha", "", nil)
	old := gameserver.Secrets{ServerPassword: oldServerPW, AdminPassword: oldAdminPW}
	now := gameserver.Secrets{ServerPassword: newServerPW, AdminPassword: newAdminPW}
	ev := h.recordEvents()
	h.setPasswords("alpha", old.ServerPassword, old.AdminPassword)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	ev.waitCommandLines(t, 1)

	if sr := h.setPasswords("alpha", now.ServerPassword, now.AdminPassword); !sr.RestartRequired {
		t.Fatalf("change while running: %+v", sr)
	}
	// The running server, started with the old values, still has them and
	// prints its command line again.
	if got, err := h.runSecrets("alpha"); err != nil || got != old || !h.serverHas(v.View, old) {
		t.Fatalf("the running server after a change: %+v %v", got, err)
	}
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	ev.waitCommandLines(t, 2)
	// Restarted, it has the new values.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restart?wait=true", struct{}{}, nil)
	ev.waitCommandLines(t, 3)
	if got, err := h.runSecrets("alpha"); err != nil || got != now || !h.serverHas(v.View, now) || h.serverHas(v.View, old) {
		t.Fatalf("the restarted server: %+v %v", got, err)
	}

	h.checkNothingToHide(t, "alpha", ev, passwordPieces(oldServerPW, oldAdminPW, newServerPW, newAdminPW))
}

// After ESM restarts, a server it left running is adopted with the secrets
// file it was started with still in place. Nothing ESM shows has a
// password, and the file goes when the server is stopped.
func TestPasswordsNeverShowAfterESMRestart(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create("alpha", "", nil)
	old := gameserver.Secrets{ServerPassword: oldServerPW, AdminPassword: oldAdminPW}
	h.setPasswords("alpha", old.ServerPassword, old.AdminPassword)
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	h.setPasswords("alpha", newServerPW, newAdminPW)

	h.stopService()
	h.startService()
	if got := h.waitState("alpha", instances.Running, 10*time.Second); got.PID != st.PID {
		t.Fatalf("not adopted: %+v, want pid %d", got, st.PID)
	}
	if got, err := h.runSecrets("alpha"); err != nil || got != old || !h.serverHas(v.View, old) {
		t.Fatalf("the adopted server and its secrets file: %+v %v", got, err)
	}
	ev := h.recordEvents()
	// The adopted server prints its command line again.
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	ev.waitCommandLines(t, 1)
	h.checkNothingToHide(t, "alpha", ev, passwordPieces(oldServerPW, oldAdminPW, newServerPW, newAdminPW))

	// Should the server print the passwords it was started with itself,
	// ESM still knows them after its restart and masks them.
	f, err := os.OpenFile(gameserver.ServerLogPath(h.dataDir("alpha")), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("Admin logged in with " + oldAdminPW + "\n")
	f.Close()
	testkit.Eventually(t, 10*time.Second, func() bool {
		var logs api.Logs
		h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/logs?lines=5", nil, &logs)
		return strings.Contains(strings.Join(logs.Lines, "\n"), "Admin logged in with "+gameserver.Mask)
	}, "a password the adopted server printed itself is not masked")

	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)
	if _, err := os.Stat(h.runSecretsPath("alpha")); !os.IsNotExist(err) {
		t.Fatalf("the secrets file is still there after the stop: %v", err)
	}
}

// A server that stopped while ESM was away leaves its secrets file behind;
// the next ESM removes it.
func TestStaleSecretsFileIsRemoved(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create("alpha", "", nil)
	h.setPasswords("alpha", leakServerPW, leakAdminPW)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	h.stopService()
	if _, err := os.Stat(h.runSecretsPath("alpha")); err != nil {
		t.Fatalf("the secrets file of a server that keeps running: %v", err)
	}
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlStop); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		s, err := gameserver.ReadStatus(h.dataDir("alpha"))
		if err != nil || s.State != gameserver.StateStopping {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		return gameserver.Probe(ctx, v.BindIP, v.Ports.Query) != nil // nobody answers any more
	}, "the server did not stop")
	time.Sleep(300 * time.Millisecond) // let the process end
	h.startService()
	if st := h.status("alpha"); st.State != instances.Stopped {
		t.Fatalf("after ESM came back: %+v", st)
	}
	if _, err := os.Stat(h.runSecretsPath("alpha")); !os.IsNotExist(err) {
		t.Fatalf("the stale secrets file is still there: %v", err)
	}
}
