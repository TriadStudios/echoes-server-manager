package instances

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// readEvents returns the remembered events as the read role gets them.
func (r *rig) readEvents() []events.Event {
	evs, _ := r.bus.Since(0, events.Filter{Public: true})
	return evs
}

// checkReadEvents fails if the read role would get a log event of the
// instance among events, or a state event of it whose reason is not one of
// allowed.
func checkReadEvents(t *testing.T, name string, evs []events.Event, allowed ...string) {
	t.Helper()
	for _, e := range evs {
		if e.Instance != name {
			continue
		}
		switch e.Type {
		case events.TypeLog:
			t.Fatalf("the read role gets a log event: %s", e.Data)
		case events.TypeState:
			var st Status
			if err := json.Unmarshal(e.Data, &st); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(allowed, st.Reason) {
				t.Fatalf("the read role gets a state event with the reason %q", st.Reason)
			}
		}
	}
}

// An instance set to command-line delivery hands its server the passwords
// as switches. The server prints them with its command line, so the read
// role gets none of its output and no state reason, while operators and
// admins see both. Going back to the secrets file needs the server stopped
// and sets its output aside.
func TestCommandLineDelivery(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	ctx := context.Background()
	v, err := r.m.Create(CreateRequest{Name: "alpha", Settings: Settings{
		BindIP: ptr("127.0.0.1"), Rules: fastRules(nil), SecretDelivery: ptr(gameserver.DeliveryCommandLine)}})
	if err != nil {
		t.Fatal(err)
	}
	if v.SecretDelivery != gameserver.DeliveryCommandLine || !slices.Equal(v.Warnings, []string{WarnSecretsOnCommandLine}) {
		t.Fatalf("created %+v with warnings %q", v.Instance, v.Warnings)
	}
	if got := v.ForRead(); got.Status.Reason != HiddenNotice || got.Status.State != Stopped {
		t.Fatalf("the view for the read role: %+v", got.Status)
	}
	const serverPW, adminPW = "join-pw-4711", "adm-pw-0815"
	secrets := gameserver.Secrets{ServerPassword: serverPW, AdminPassword: adminPW}
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(serverPW), AdminPassword: ptr(adminPW)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if !r.has(v, secrets) || r.has(v, gameserver.Secrets{}) {
		t.Fatal("the server does not have exactly the passwords of its command line")
	}
	raw := r.rawOutput("alpha")
	if !strings.Contains(raw, "-ServerPassword="+serverPW+" -AdminPassword="+adminPW+" -log") || strings.Contains(raw, gameserver.SecretsFileSwitch) {
		t.Fatalf("the server's command line:\n%s", raw)
	}
	// The file records what the running server was given.
	if got, ok := r.runSecrets("alpha"); !ok || got != secrets {
		t.Fatalf("the secrets file of the running server: %+v %v", got, ok)
	}

	// The read role: no output at all, masked or not.
	for _, source := range []string{SourceServer, SourceConsole} {
		_, err := r.m.Logs("alpha", source, 100, false)
		if err != ErrOutputHidden || fault.CodeOf(err) != "hidden_unsafe_secrets" || !fault.Is(err, fault.Forbidden) || err.Error() != HiddenNotice {
			t.Fatalf("the %s log for the read role: %v", source, err)
		}
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		evs, _ := r.bus.Since(0, events.Filter{Types: []string{events.TypeLog}})
		return len(evs) >= 4
	}, "no log events")
	checkReadEvents(t, "alpha", r.readEvents(), HiddenNotice)
	if view, _ := r.m.Get("alpha"); view.ForRead().Status.Reason != HiddenNotice || view.Status.Reason == HiddenNotice {
		t.Fatalf("status reasons: %q for the read role, %q for the others", view.ForRead().Status.Reason, view.Status.Reason)
	}

	// Operators and admins: the output, with the passwords ESM knows
	// masked.
	for _, source := range []string{SourceServer, SourceConsole} {
		lines, err := r.m.Logs("alpha", source, 100, true)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(lines, "\n")
		if !strings.Contains(text, "-ServerPassword="+gameserver.Mask+" -AdminPassword="+gameserver.Mask+" -log") ||
			strings.Contains(text, serverPW) || strings.Contains(text, adminPW) {
			t.Fatalf("the %s log for an operator:\n%s", source, text)
		}
	}
	all, _ := r.bus.Since(0, events.Filter{})
	allRaw, _ := json.Marshal(all)
	if !strings.Contains(string(allRaw), "Command line: ") || !strings.Contains(string(allRaw), `"reason":"ready (status_file)"`) ||
		strings.Contains(string(allRaw), serverPW) || strings.Contains(string(allRaw), adminPW) {
		t.Fatalf("events for an operator: %s", allRaw)
	}

	// Back to the file: refused while the server runs.
	_, _, err = r.m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryFile)}})
	if fault.CodeOf(err) != "instance_running" || !fault.Is(err, fault.Conflict) {
		t.Fatalf("change to file delivery while running: %v", err)
	}
	if view, _ := r.m.Get("alpha"); !view.OutputHidden() {
		t.Fatal("a refused change was applied")
	}
	if _, err := r.m.Stop(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("the secrets file is still there after the stop")
	}
	if _, err := r.m.Logs("alpha", SourceServer, 100, false); err != ErrOutputHidden {
		t.Fatalf("a stopped instance with command-line delivery shows its output to the read role: %v", err)
	}
	v, restart, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryFile)}})
	if err != nil || restart || v.OutputHidden() || len(v.Warnings) != 0 || v.Status.Reason != outputSetAside || v.ForRead().Status.Reason != outputSetAside {
		t.Fatalf("change to file delivery: %+v %q %v %v", v.Status, v.Warnings, restart, err)
	}
	// The output so far is set aside, whole, and no longer served.
	for _, rel := range []string{consoleFile("alpha"), serverLog("alpha")} {
		if r.st.Exists(rel) {
			t.Errorf("%s is still in place", rel)
		}
		if b, err := r.st.ReadFile(rel + unsafeSuffix); err != nil || !strings.Contains(string(b), "-ServerPassword="+serverPW) {
			t.Errorf("%s was not set aside whole: %v", rel, err)
		}
	}
	for _, source := range []string{SourceServer, SourceConsole} {
		if lines, err := r.m.Logs("alpha", source, 100, false); err != nil || len(lines) != 0 {
			t.Fatalf("the %s log for the read role after the change: %q %v", source, lines, err)
		}
	}
	// What was published while the instance was set to the command line
	// stays kept from the read role.
	checkReadEvents(t, "alpha", r.readEvents(), HiddenNotice, outputSetAside)

	// Started again, the server reads the file, and the read role sees its
	// output: there is no password in it.
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if !r.has(v, secrets) {
		t.Fatal("the server did not get the passwords from the secrets file")
	}
	for _, source := range []string{SourceServer, SourceConsole} {
		lines, err := r.m.Logs("alpha", source, 100, false)
		text := strings.Join(lines, "\n")
		if err != nil || !strings.Contains(text, "Command line: ") || strings.Contains(strings.ToLower(text), "password") ||
			strings.Contains(text, gameserver.Mask) || strings.Contains(text, serverPW) || strings.Contains(text, adminPW) {
			t.Fatalf("the %s log for the read role with file delivery: %v\n%s", source, err, text)
		}
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		for _, e := range r.readEvents() {
			if e.Type == events.TypeLog && strings.Contains(string(e.Data), "Command line: ") {
				return true
			}
		}
		return false
	}, "the read role gets no log events of the server with file delivery")
	readRaw, _ := json.Marshal(r.readEvents())
	if strings.Contains(string(readRaw), serverPW) || strings.Contains(string(readRaw), adminPW) || strings.Contains(strings.ToLower(string(readRaw)), "serverpassword") {
		t.Fatalf("events for the read role: %s", readRaw)
	}

	// To the command line while the server runs: allowed, and the output
	// is hidden from the read role at once.
	v, restart, err = r.m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}})
	if err != nil || !restart || !v.OutputHidden() || !v.Status.RestartRequired || !slices.Equal(v.Warnings, []string{WarnSecretsOnCommandLine}) {
		t.Fatalf("change to command-line delivery while running: %+v %q %v %v", v.Status, v.Warnings, restart, err)
	}
	if _, err := r.m.Logs("alpha", SourceServer, 100, false); err != ErrOutputHidden {
		t.Fatalf("output for the read role right after the change: %v", err)
	}
	before := len(r.readEvents())
	data, _ := r.st.Abs(dataDir("alpha"))
	if err := testkit.WriteControl(data, testkit.ControlCommandLine); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		evs, _ := r.bus.Since(0, events.Filter{Types: []string{events.TypeLog}})
		n := 0
		for _, e := range evs {
			if strings.Contains(string(e.Data), "Command line: ") {
				n++
			}
		}
		return n >= 6 // two starts and this request, in the log and the output
	}, "the command line was not printed again")
	for _, e := range r.readEvents()[before:] {
		if e.Type == events.TypeLog {
			t.Fatalf("the read role gets a log event after the change to the command line: %s", e.Data)
		}
	}
}

// A start that fails with the passwords on the command line: even when the
// supervisor repeats the whole command line in its error, the passwords are
// masked for operators and admins, and the read role gets the notice.
func TestCommandLineStartFailure(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if _, err := m.Create(CreateRequest{Name: "alpha", Settings: Settings{
		BindIP: ptr("127.0.0.1"), SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(testServerPW), AdminPassword: ptr(testAdminPW)}); err != nil {
		t.Fatal(err)
	}
	secret := pieces(testServerPW, testAdminPW)
	_, err := m.Start(context.Background(), "alpha", true)
	if fault.CodeOf(err) != "start_failed" || !strings.Contains(err.Error(), "-ServerPassword="+gameserver.Mask) {
		t.Fatalf("start error: %v", err)
	}
	noPieces(t, "start error", err.Error(), secret)
	spec, _, _ := sup.last(t)
	if !slices.Equal(spec.SecretValues, []string{testServerPW, testAdminPW}) {
		t.Fatalf("the supervisor was told these secret values: %q", spec.SecretValues)
	}
	if !slices.Contains(spec.Args, "-ServerPassword="+testServerPW) || !slices.Contains(spec.Args, "-AdminPassword="+testAdminPW) ||
		strings.Contains(strings.Join(spec.Args, "\n"), gameserver.SecretsFileSwitch) {
		t.Fatalf("the command line with command-line delivery: %q", spec.Args)
	}

	// A failing restart after a crash records the error as the reason.
	in := m.lookup("alpha")
	in.mu.Lock()
	in.gen++
	gen := in.gen
	m.setLocked(in, Crashed, "test crash")
	in.mu.Unlock()
	m.restartAfterCrash(in, gen)
	v, _ := m.Get("alpha")
	if v.Status.State != Failed || !strings.Contains(v.Status.Reason, "restart after a crash failed") || v.ForRead().Status.Reason != HiddenNotice {
		t.Fatalf("after a failed restart: %+v", v.Status)
	}
	noPieces(t, "state reason", v.Status.Reason, secret)
	evs, _ := r.bus.Since(0, events.Filter{})
	raw, _ := json.Marshal(evs)
	noPieces(t, "events", string(raw), secret)
	checkReadEvents(t, "alpha", r.readEvents(), HiddenNotice)
	state, _ := r.st.ReadFile(stateFile("alpha"))
	noPieces(t, "state.json", string(state), secret)
	noPieces(t, "service log", r.log.String(), secret)
	if !strings.Contains(r.log.String(), "UNSAFE on a shared box") {
		t.Fatalf("the service log does not warn about the command line:\n%s", r.log.String())
	}

	// Set back to the file (nothing runs), the failure reason goes too: it
	// repeats what the supervisor's tool printed.
	v, _, err = m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryFile)}})
	if err != nil || v.Status.State != Failed || v.Status.Reason != outputSetAside {
		t.Fatalf("change to file delivery: %+v %v", v.Status, err)
	}
	state, _ = r.st.ReadFile(stateFile("alpha"))
	if strings.Contains(string(state), "restart after a crash failed") {
		t.Fatalf("state.json still has the old reason:\n%s", state)
	}
}

// A new manager on the same root keeps an instance with command-line
// delivery hidden, and treats an instance.json without the setting as file
// delivery.
func TestDeliveryAcrossRestart(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("plain", nil)
	if _, err := r.m.Create(CreateRequest{Name: "unsafe", Settings: Settings{
		BindIP: ptr("127.0.0.1"), SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatal(err)
	}
	// An instance.json from before the setting existed.
	var old map[string]any
	if err := r.st.ReadJSON(instFile("plain"), &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "secret_delivery")
	if err := r.st.WriteJSON(instFile("plain"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	m := r.newManager(&recordSup{})
	defer m.Close()
	plain, _ := m.Get("plain")
	unsafe, _ := m.Get("unsafe")
	if plain.SecretDelivery != gameserver.DeliveryFile || plain.OutputHidden() || len(plain.Warnings) != 0 {
		t.Fatalf("an instance without the setting: %q %q", plain.SecretDelivery, plain.Warnings)
	}
	if !unsafe.OutputHidden() || !slices.Equal(unsafe.Warnings, []string{WarnSecretsOnCommandLine}) {
		t.Fatalf("the instance with command-line delivery: %q %q", unsafe.SecretDelivery, unsafe.Warnings)
	}
	if _, err := m.Logs("unsafe", SourceServer, 10, false); err != ErrOutputHidden {
		t.Fatalf("output for the read role: %v", err)
	}
	if _, err := m.Logs("plain", SourceServer, 10, false); err != nil {
		t.Fatalf("output of a file-delivery instance for the read role: %v", err)
	}
	// An empty or unknown value is refused on a change.
	for _, bad := range []string{"", "environment", "File"} {
		if _, _, err := m.Patch("plain", PatchRequest{Settings: Settings{SecretDelivery: ptr(bad)}}); !fault.Is(err, fault.Invalid) {
			t.Fatalf("secret_delivery %q: %v", bad, err)
		}
	}
}

// A server build that cannot read the secrets file starts without the
// passwords of its instance. ESM cannot stop that, but it shows: the
// running server does not confirm the passwords, and the instance carries a
// warning until it runs a build that does, or is set to the command line.
func TestUnconfirmedSecretsAreFlagged(t *testing.T) {
	r := newRig(t, nil)
	// 0.9 stands for a server build from before the secrets file.
	r.install(testkit.BuildOptions{Version: "0.9", Fake: map[string]any{"FakeIgnoreSecretsFile": true}})
	r.install(testkit.BuildOptions{Version: "1.0"})
	ctx := context.Background()
	secrets := gameserver.Secrets{ServerPassword: "join-pw-4711", AdminPassword: "adm-pw-0815"}
	for _, name := range []string{"old", "new"} {
		build := map[string]string{"old": "0.9", "new": "1.0"}[name]
		v, err := r.m.Create(CreateRequest{Name: name, Build: build, Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}})
		if err != nil {
			t.Fatal(err)
		}
		// Without passwords there is nothing to confirm.
		if _, err := r.m.Start(ctx, name, true); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.m.Get(name); len(got.Warnings) != 0 || !r.has(v, gameserver.Secrets{}) {
			t.Fatalf("%s without passwords: warnings %q", name, got.Warnings)
		}
		// Set while it runs, they are not confirmed before the restart.
		if _, _, err := r.m.SetSecrets(name, SecretsRequest{ServerPassword: &secrets.ServerPassword, AdminPassword: &secrets.AdminPassword}); err != nil {
			t.Fatal(err)
		}
		if got, _ := r.m.Get(name); !slices.Equal(got.Warnings, []string{WarnSecretsUnconfirmed}) || !got.Status.RestartRequired {
			t.Fatalf("%s with passwords set while running: warnings %q, status %+v", name, got.Warnings, got.Status)
		}
		if _, err := r.m.Restart(ctx, name, "", true); err != nil {
			t.Fatal(err)
		}
	}
	// The build that reads the file confirms; the other one runs open.
	if got, _ := r.m.Get("new"); len(got.Warnings) != 0 || !r.has(got, secrets) {
		t.Fatalf("the build that reads the secrets file: warnings %q", got.Warnings)
	}
	old, _ := r.m.Get("old")
	if !slices.Equal(old.Warnings, []string{WarnSecretsUnconfirmed}) || !r.has(old, gameserver.Secrets{}) || r.has(old, secrets) {
		t.Fatalf("the build that ignores the secrets file: warnings %q", old.Warnings)
	}
	if !strings.Contains(WarningText(WarnSecretsUnconfirmed), "WITHOUT passwords") || WarningText("something_else") != "something_else" {
		t.Fatal("warning texts")
	}
	// Stopped, there is no server to confirm anything.
	if _, err := r.m.Stop(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.m.Get("old"); len(got.Warnings) != 0 {
		t.Fatalf("stopped: warnings %q", got.Warnings)
	}
	// The way out for such a build is the command line, with its own
	// warning.
	if _, _, err := r.m.Patch("old", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "old", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.m.Get("old"); !slices.Equal(got.Warnings, []string{WarnSecretsOnCommandLine}) || !r.has(got, secrets) {
		t.Fatalf("the old build with command-line delivery: warnings %q", got.Warnings)
	}
}
