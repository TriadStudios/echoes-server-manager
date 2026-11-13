package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// setAsideReason is the state reason of an instance right after it went
// back to file delivery.
const setAsideReason = "server output set aside: secret_delivery changed to file"

// readSees fails if events, as a read token got them, have a log event of
// the instance, or a state event of it with a reason outside allowed.
func readSees(t *testing.T, what, name string, evs []events.Event, allowed ...string) {
	t.Helper()
	states := 0
	for _, e := range evs {
		if e.Instance != name {
			continue
		}
		switch e.Type {
		case events.TypeLog:
			t.Fatalf("%s: the read role got a log event: %s", what, e.Data)
		case events.TypeState:
			states++
			if st := stateOf(e); !slices.Contains(allowed, st.Reason) {
				t.Fatalf("%s: the read role got a state event with the reason %q", what, st.Reason)
			}
		}
	}
	if states == 0 {
		t.Fatalf("%s: the read role got no state event of %s", what, name)
	}
}

// An instance with secret_delivery "command_line" gives its server the
// passwords as switches. Its command line is readable by every account on
// the box, and the server prints it into its log. So the read role gets
// none of that instance's server output (403 on the logs, no log events)
// and a notice in place of every state reason, while operators and admins
// see both. Going back to "file" needs the server stopped and sets the
// output so far aside.
func TestCommandLineModeIsHiddenFromRead(t *testing.T) {
	t.Parallel()
	commandLineMode(t, newESM(t), "alpha")
}

func TestCommandLineModeIsHiddenFromReadSystemd(t *testing.T) {
	commandLineMode(t, newSystemdESM(t), unitSafeName("sdcl"))
}

func commandLineMode(t *testing.T, h *harness, name string) {
	h.install(testkit.BuildOptions{Version: "1.0"})
	read, oper := h.token("read"), h.token("operator")
	pieces := passwordPieces(oldServerPW, oldAdminPW)
	secrets := gameserver.Secrets{ServerPassword: oldServerPW, AdminPassword: oldAdminPW}
	base := "/api/v1/instances/" + name

	// The mode is chosen in so many words, and every answer warns.
	var ch api.InstanceChange
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{
		"name": name, "bind_ip": "127.0.0.1", "rules": fastRules(nil), "secret_delivery": "command_line"}, &ch)
	if ch.SecretDelivery != gameserver.DeliveryCommandLine || !slices.Equal(ch.Warnings, []string{instances.WarnSecretsOnCommandLine}) {
		t.Fatalf("created with %q, warnings %q", ch.SecretDelivery, ch.Warnings)
	}
	if sr := h.setPasswords(name, secrets.ServerPassword, secrets.AdminPassword); !slices.Equal(sr.Warnings, []string{instances.WarnSecretsOnCommandLine}) {
		t.Fatalf("warnings after setting the passwords: %q", sr.Warnings)
	}
	liveRead := h.recordEventsAs(read, "")
	liveOper := h.recordEventsAs(oper, "")
	var st instances.Status
	h.must(http.StatusOK, "POST", base+"/start?wait=true", struct{}{}, &st)
	liveOper.waitCommandLines(t, 1)
	if !h.serverHas(ch.View, secrets) {
		t.Fatal("the server did not get the passwords from its command line")
	}
	if line := procCommandLine(st.PID); runtime.GOOS == "linux" && !strings.Contains(line, "-ServerPassword="+oldServerPW+" -AdminPassword="+oldAdminPW) {
		t.Fatalf("the command line of the running server, as every account can read it: %s", line)
	}

	// The read role: no server output, by no route.
	for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
		var raw json.RawMessage
		status := h.call("GET", base+"/logs?lines=2000&source="+source, read, nil, &raw)
		var eb api.ErrorBody
		json.Unmarshal(raw, &eb)
		if status != http.StatusForbidden || eb.Error.Code != "hidden_unsafe_secrets" || eb.Error.Message != instances.HiddenNotice {
			t.Fatalf("%s log with a read token: %d %s", source, status, raw)
		}
	}
	var view instances.View
	var status instances.Status
	var list api.InstanceList
	for path, into := range map[string]any{base: &view, base + "/status": &status, "/api/v1/instances": &list} {
		var raw json.RawMessage
		if code := h.call("GET", path, read, nil, &raw); code != http.StatusOK {
			t.Fatalf("GET %s with a read token: %d %s", path, code, raw)
		}
		noPieces(t, "answer of GET "+path+" for a read token", raw, pieces)
		json.Unmarshal(raw, into)
	}
	if view.Status.Reason != instances.HiddenNotice || status.Reason != instances.HiddenNotice || list.Instances[0].Status.Reason != instances.HiddenNotice ||
		status.State != instances.Running || !slices.Equal(view.Warnings, []string{instances.WarnSecretsOnCommandLine}) {
		t.Fatalf("the read role sees: instance %+v, status %+v, list %+v", view.Status, status, list.Instances[0].Status)
	}
	time.Sleep(300 * time.Millisecond) // let the streams catch up
	readSees(t, "live", name, liveRead.all(), instances.HiddenNotice)
	readRaw, _ := json.Marshal(liveRead.all())
	noPieces(t, "events for a read token", readRaw, pieces)

	// Operators and admins: the output and the reasons.
	for who, token := range map[string]string{"operator": oper, "admin": h.admin} {
		for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
			var logs api.Logs
			if code := h.call("GET", base+"/logs?lines=2000&source="+source, token, nil, &logs); code != http.StatusOK {
				t.Fatalf("%s log for an %s: %d", source, who, code)
			}
			text := strings.Join(logs.Lines, "\n")
			if !strings.Contains(text, "Command line: ") || !strings.Contains(text, "-ServerPassword="+gameserver.Mask+" -AdminPassword="+gameserver.Mask+"\n") {
				t.Fatalf("the %s log for an %s has no command line with masked passwords:\n%s", source, who, text)
			}
			noPieces(t, source+" log for an "+who, []byte(text), pieces)
		}
		var got instances.Status
		if code := h.call("GET", base+"/status", token, nil, &got); code != http.StatusOK || got.Reason != "ready ("+got.ReadySource+")" {
			t.Fatalf("status for an %s: %d %+v", who, code, got)
		}
	}
	if liveOper.commandLines(instances.SourceServer) == 0 {
		t.Fatal("an operator got no log events")
	}
	operRaw, _ := json.Marshal(liveOper.all())
	noPieces(t, "events for an operator", operRaw, pieces)
	if !strings.Contains(string(operRaw), `"reason":"ready (`) {
		t.Fatalf("an operator got no state reasons: %s", operRaw)
	}

	// Every refusal is in the audit log, and the box says who is unsafe.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	refused := 0
	for _, e := range al.Entries {
		if e.Action == "instance.logs" && !e.OK && e.Actor.Role == "read" && e.Error == instances.HiddenNotice {
			refused++
		}
	}
	if refused != 2 {
		t.Fatalf("the audit log has %d refused log requests of the read token, want 2", refused)
	}
	var box api.Box
	if code := h.call("GET", "/api/v1/box", read, nil, &box); code != http.StatusOK ||
		box.DefaultSecretDelivery != gameserver.DeliveryFile || !slices.Equal(box.SecretsOnCommandLine, []string{name}) {
		t.Fatalf("box: %d %+v", code, box)
	}

	// Back to the file: only with the server stopped.
	var eb api.ErrorBody
	if code := h.call("PATCH", base, h.admin, map[string]any{"secret_delivery": "file"}, &eb); code != http.StatusConflict || eb.Error.Code != "instance_running" {
		t.Fatalf("change to file delivery while running: %d %+v", code, eb)
	}
	h.must(http.StatusOK, "POST", base+"/stop", struct{}{}, nil)
	for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
		if code := h.call("GET", base+"/logs?source="+source, read, nil, &eb); code != http.StatusForbidden {
			t.Fatalf("%s log of the stopped instance with a read token: %d", source, code)
		}
	}
	h.must(http.StatusOK, "PATCH", base, map[string]any{"secret_delivery": "file"}, &ch)
	if ch.SecretDelivery != gameserver.DeliveryFile || len(ch.Warnings) != 0 || ch.Status.Reason != setAsideReason || ch.RestartRequired {
		t.Fatalf("after the change to file delivery: %q %q %+v", ch.SecretDelivery, ch.Warnings, ch.Status)
	}
	// What the server wrote so far is set aside, whole, and served to
	// nobody.
	instDir := filepath.Join(h.root, "instances", name)
	for _, p := range []string{filepath.Join(instDir, "console.log"), h.serverLog(name)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still in place: %v", p, err)
		}
		if b, err := os.ReadFile(p + ".unsafe"); err != nil || !strings.Contains(string(b), "-ServerPassword="+oldServerPW) {
			t.Errorf("%s was not set aside whole: %v", p, err)
		}
	}
	for who, token := range map[string]string{"read": read, "operator": oper, "admin": h.admin} {
		for _, source := range []string{instances.SourceServer, instances.SourceConsole} {
			var logs api.Logs
			if code := h.call("GET", base+"/logs?lines=2000&source="+source, token, nil, &logs); code != http.StatusOK || len(logs.Lines) != 0 {
				t.Fatalf("%s log for %s after the change: %d %q", source, who, code, logs.Lines)
			}
		}
	}
	if code := h.call("GET", base+"/status", read, nil, &status); code != http.StatusOK || status.Reason != setAsideReason {
		t.Fatalf("status for a read token after the change: %d %+v", code, status)
	}
	// Events from before the change stay kept from the read role, also
	// when it asks for everything that is remembered.
	replay := h.recordEventsAs(read, "0")
	time.Sleep(500 * time.Millisecond)
	readSees(t, "replay", name, replay.all(), instances.HiddenNotice, setAsideReason)
	replayRaw, _ := json.Marshal(replay.all())
	noPieces(t, "remembered events for a read token", replayRaw, pieces)

	// Started again, the server reads the file, and the read role sees
	// output that has no password in it.
	fresh := h.recordEventsAs(read, "")
	h.must(http.StatusOK, "POST", base+"/start?wait=true", struct{}{}, &st)
	fresh.waitCommandLines(t, 1)
	if !h.serverHas(ch.View, secrets) {
		t.Fatal("the server did not get the passwords from the secrets file")
	}
	h.checkNothingToHide(t, name, fresh, pieces)
	if code := h.call("GET", "/api/v1/box", read, nil, &box); code != http.StatusOK || len(box.SecretsOnCommandLine) != 0 {
		t.Fatalf("box after the change: %d %+v", code, box)
	}
	h.must(http.StatusOK, "POST", base+"/stop", struct{}{}, nil)
}

// An instance can be set to the command line while its server runs. The
// read role is kept from its output from that moment, although the running
// server still has its passwords in the file.
func TestChangeToCommandLineWhileRunning(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	read := h.token("read")
	v := h.create("alpha", "", nil)
	h.setPasswords("alpha", leakServerPW, leakAdminPW)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	var logs api.Logs
	if code := h.call("GET", "/api/v1/instances/alpha/logs", read, nil, &logs); code != http.StatusOK || len(logs.Lines) == 0 {
		t.Fatalf("log with a read token and file delivery: %d", code)
	}
	var ch api.InstanceChange
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", map[string]any{"secret_delivery": "command_line"}, &ch)
	if !ch.RestartRequired || !ch.Status.RestartRequired || !slices.Equal(ch.Warnings, []string{instances.WarnSecretsOnCommandLine}) {
		t.Fatalf("change to the command line while running: %+v %q", ch.Status, ch.Warnings)
	}
	// From here on the read role gets no log event of the instance, also
	// not of the server that still runs with its passwords in the file.
	live := h.recordEventsAs(read, "")
	var eb api.ErrorBody
	if code := h.call("GET", "/api/v1/instances/alpha/logs", read, nil, &eb); code != http.StatusForbidden || eb.Error.Code != "hidden_unsafe_secrets" {
		t.Fatalf("log with a read token right after the change: %d %+v", code, eb)
	}
	// The restart puts the passwords on the command line.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restart?wait=true", struct{}{}, nil)
	if !h.serverHas(v.View, gameserver.Secrets{ServerPassword: leakServerPW, AdminPassword: leakAdminPW}) {
		t.Fatal("the restarted server does not have the passwords")
	}
	if raw := h.rawOutput("alpha"); !strings.Contains(raw, "-ServerPassword="+leakServerPW) {
		t.Fatalf("the restarted server did not get its passwords on the command line:\n%s", raw)
	}
	time.Sleep(500 * time.Millisecond)
	readSees(t, "after the change", "alpha", live.all(), instances.HiddenNotice)
	raw, _ := json.Marshal(live.all())
	noPieces(t, "events for a read token", raw, []string{leakServerPW, leakAdminPW, "sesame"})
	if code := h.call("GET", "/api/v1/instances/alpha/logs?source=console", read, nil, &eb); code != http.StatusForbidden {
		t.Fatalf("console log with a read token: %d", code)
	}
}

// A start that fails with the passwords on the command line, while
// systemd-run repeats that whole command line: operators and admins get the
// failure with the passwords masked, in every form; the read role gets the
// notice.
func TestCommandLineStartFailureSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	dir := fakeSystemdRun(t)
	commandLineStartFailure(t, h, dir, unitSafeName("sdcf"), leakServerPW, leakAdminPW, []string{leakServerPW, leakAdminPW, "sesame"}, leakServerPW)
}

// The same on a systemd that gets every "$" of the command line doubled.
func TestCommandLineStartFailureOldSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	if os.Getenv("ESM_TEST_SYSTEMD") != "1" {
		t.Skip("set ESM_TEST_SYSTEMD=1 to run tests that start systemd user units")
	}
	dir := fakeOldSystemd(t)
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	escaped := strings.ReplaceAll(dollarServerPW, "$", "$$")
	pieces := passwordPieces(dollarServerPW, dollarAdminPW, escaped, strings.ReplaceAll(dollarAdminPW, "$", "$$"))
	commandLineStartFailure(t, h, dir, unitSafeName("sdco"), dollarServerPW, dollarAdminPW, pieces, escaped)
}

// commandLineStartFailure starts an instance with command-line delivery
// through a fake systemd-run (in dir) that fails and repeats what it was
// given. onArgv is the server password as systemd-run must have got it.
func commandLineStartFailure(t *testing.T, h *harness, dir, name, serverPW, adminPW string, pieces []string, onArgv string) {
	read := h.token("read")
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{
		"name": name, "bind_ip": "127.0.0.1", "rules": fastRules(nil), "secret_delivery": "command_line"}, nil)
	h.setPasswords(name, serverPW, adminPW)
	live := h.recordEvents()
	liveRead := h.recordEventsAs(read, "")
	var raw json.RawMessage
	code := h.call("POST", "/api/v1/instances/"+name+"/start?wait=true", h.admin, struct{}{}, &raw)
	var eb api.ErrorBody
	json.Unmarshal(raw, &eb)
	if code != http.StatusUnprocessableEntity || eb.Error.Code != "start_failed" || !strings.Contains(eb.Error.Message, "Failed to start transient service unit") {
		t.Fatalf("start with a failing systemd-run: %d %s", code, raw)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(args), "\n-ServerPassword="+onArgv+"\n") || strings.Contains(string(args), gameserver.SecretsFileSwitch) {
		t.Fatalf("systemd-run did not get the passwords on the command line (%v):\n%s", err, args)
	}
	var status json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/status", nil, &status)
	time.Sleep(500 * time.Millisecond)
	evRaw, _ := json.Marshal(live.all())
	auditLog, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
	state, _ := os.ReadFile(filepath.Join(h.root, "instances", name, "run", "state.json"))
	for what, text := range map[string]string{
		"error answer": string(raw), "status": string(status), "events": string(evRaw),
		"audit log": string(auditLog), "service log": h.stderr.String(), "state.json": string(state),
	} {
		noPieces(t, what, []byte(text), pieces)
		noLaunchLine(t, what, text)
	}
	var st instances.Status
	if c := h.call("GET", "/api/v1/instances/"+name+"/status", read, nil, &st); c != http.StatusOK || st.Reason != instances.HiddenNotice {
		t.Fatalf("status for a read token: %d %+v", c, st)
	}
	for _, e := range liveRead.all() {
		if e.Instance == name && (e.Type == events.TypeLog || stateOf(e).Reason != instances.HiddenNotice) {
			t.Fatalf("the read role got %s", e.Data)
		}
	}
	if _, err := os.Stat(h.runSecretsPath(name)); !os.IsNotExist(err) {
		t.Fatalf("the secrets file is still there after the start failed: %v", err)
	}
}

// A build that cannot read the secrets file runs without the passwords of
// its instance. The API and the command line say so while it runs.
func TestUnconfirmedSecretsAreFlagged(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	// 0.9 stands for a server build from before the secrets file.
	h.install(testkit.BuildOptions{Version: "0.9", Fake: map[string]any{"FakeIgnoreSecretsFile": true}})
	h.install(testkit.BuildOptions{Version: "1.0"})
	read := h.token("read")
	secrets := gameserver.Secrets{ServerPassword: leakServerPW, AdminPassword: leakAdminPW}
	for name, build := range map[string]string{"old": "0.9", "new": "1.0"} {
		h.create(name, build, nil)
		h.setPasswords(name, secrets.ServerPassword, secrets.AdminPassword)
		h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, nil)
	}
	var old, now instances.View
	if code := h.call("GET", "/api/v1/instances/old", read, nil, &old); code != http.StatusOK ||
		!slices.Equal(old.Warnings, []string{instances.WarnSecretsUnconfirmed}) || !h.serverHas(old, gameserver.Secrets{}) {
		t.Fatalf("the build that ignores the secrets file: %d, warnings %q", code, old.Warnings)
	}
	if code := h.call("GET", "/api/v1/instances/new", read, nil, &now); code != http.StatusOK || len(now.Warnings) != 0 || !h.serverHas(now, secrets) {
		t.Fatalf("the build that reads the secrets file: %d, warnings %q", code, now.Warnings)
	}
	code, out, _ := h.runCLI("status", "old")
	if code != 0 || !strings.Contains(oneLine(t, "status", out), "; warning: the running server has not confirmed that it took its passwords") {
		t.Fatalf("status of the instance on the old build: %d %q", code, out)
	}
	code, out, _ = h.runCLI("status", "new")
	if code != 0 || strings.Contains(out, "warning") {
		t.Fatalf("status of the instance on the new build: %d %q", code, out)
	}
}
