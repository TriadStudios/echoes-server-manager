package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// noPieces fails the test if raw, or any string inside it when it is JSON
// (so escaped characters are compared as they are meant), contains one of
// pieces.
func noPieces(t *testing.T, what string, raw []byte, pieces []string) {
	t.Helper()
	texts := []string{string(raw)}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		texts = jsonStrings(v, texts)
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
// whole command line, the worst case for leaking passwords.
func fakeSystemdRun(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"Failed to start transient service unit: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
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
// in the answer, the status, the events or the audit log.
func TestStartFailureHidesPasswords(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	dir := brokenBuild(t, h.src, "0.9-broken")
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, nil)
	startFailureHidesPasswords(t, h, "alpha", "0.9-broken", leakServerPW, leakAdminPW, []string{leakServerPW, leakAdminPW, "sesame"})
}

func TestStartFailureHidesPasswordsSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	fakeSystemdRun(t)
	startFailureHidesPasswords(t, h, unitSafeName("sdpw"), "1.0", leakServerPW, leakAdminPW, []string{leakServerPW, leakAdminPW, "sesame"})
}

// On systemd versions without --expand-environment ESM doubles every "$"
// of the command line. A failing systemd-run that repeats it shows the
// passwords in that form; they are hidden from every answer and log.
func TestStartFailureHidesEscapedPasswordsOldSystemd(t *testing.T) {
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
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "-ServerPassword="+strings.ReplaceAll(dollarServerPW, "$", "$$")) {
		t.Fatalf("systemd-run did not get the escaped command line (the fake systemd version was not used):\n%s", args)
	}
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
	}
	if !strings.Contains(string(auditLog), `"action":"instance.start"`) {
		t.Fatalf("the failed start was not audited:\n%s", auditLog)
	}
}

// eventLog records every event of a stream in the background.
type eventLog struct {
	mu   sync.Mutex
	list []events.Event
}

func (h *harness) recordEvents() *eventLog {
	h.t.Helper()
	s := h.openEvents("", h.admin, "")
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

// checkHidden fetches everything ESM shows about an instance (log tails of
// both sources, status, settings, the recorded events) and fails if any
// piece of a password shows. Each log tail must still have the command line,
// masked.
func (h *harness) checkHidden(t *testing.T, name string, ev *eventLog, pieces []string) {
	t.Helper()
	for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
		var raw json.RawMessage
		h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/logs?lines=2000&source="+source, nil, &raw)
		noPieces(t, source+" log", raw, pieces)
		var logs api.Logs
		json.Unmarshal(raw, &logs)
		text := strings.Join(logs.Lines, "\n")
		if !strings.Contains(text, "-ServerPassword="+gameserver.Mask+" -AdminPassword="+gameserver.Mask+" -log") {
			t.Errorf("the %s log has no masked command line:\n%s", source, text)
		}
	}
	for _, path := range []string{"/status", ""} {
		var raw json.RawMessage
		h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+path, nil, &raw)
		noPieces(t, "answer of GET "+path, raw, pieces)
	}
	evRaw, err := json.Marshal(ev.all())
	if err != nil {
		t.Fatal(err)
	}
	noPieces(t, "events", evRaw, pieces)
	noPieces(t, "service log", []byte(h.stderr.String()), pieces)
}

func (h *harness) setPasswords(name, server, admin string) api.SecretsResult {
	h.t.Helper()
	var sr api.SecretsResult
	h.must(http.StatusOK, "PUT", "/api/v1/instances/"+name+"/secrets",
		map[string]string{"server_password": server, "admin_password": admin}, &sr)
	return sr
}

// Passwords changed while their server runs stay hidden, old and new
// values alike, wherever ESM shows text: the log tails of both sources, log
// and state events, the status and the settings. The running server keeps
// printing the old values; after a restart it prints the new ones.
func TestChangedPasswordsStayHidden(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	ev := h.recordEvents()
	h.setPasswords("alpha", oldServerPW, oldAdminPW)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	ev.waitCommandLines(t, 1)

	if sr := h.setPasswords("alpha", newServerPW, newAdminPW); !sr.RestartRequired {
		t.Fatalf("change while running: %+v", sr)
	}
	// The running server, started with the old values, prints them again.
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	ev.waitCommandLines(t, 2)
	// Restarted, it has the new values.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restart?wait=true", struct{}{}, nil)
	ev.waitCommandLines(t, 3)

	h.checkHidden(t, "alpha", ev, passwordPieces(oldServerPW, oldAdminPW, newServerPW, newAdminPW))
}

// After ESM restarts, the values the passwords had before a change are
// still hidden: they are kept with the secrets, not only in memory. The
// server started with the old values keeps running and is adopted.
func TestOldPasswordsStayHiddenAfterESMRestart(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	h.setPasswords("alpha", oldServerPW, oldAdminPW)
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	testkit.Eventually(t, 10*time.Second, func() bool {
		var logs api.Logs
		h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/logs?source=console", nil, &logs)
		return strings.Contains(strings.Join(logs.Lines, "\n"), "Command line: ")
	}, "the server did not print its command line")
	h.setPasswords("alpha", newServerPW, newAdminPW)

	h.stopService()
	h.startService()
	if got := h.waitState("alpha", instances.Running, 10*time.Second); got.PID != st.PID {
		t.Fatalf("not adopted: %+v, want pid %d", got, st.PID)
	}
	ev := h.recordEvents()
	// The adopted server prints its command line, with the old values,
	// again.
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	ev.waitCommandLines(t, 1)

	h.checkHidden(t, "alpha", ev, passwordPieces(oldServerPW, oldAdminPW, newServerPW, newAdminPW))
}
