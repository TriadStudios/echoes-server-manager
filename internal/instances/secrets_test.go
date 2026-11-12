package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// Passwords with spaces, quotes, shell and JSON punctuation and something
// that looks like a switch inside.
const (
	testServerPW = `ovs7 Qq$&"';%,=<>1 -Zed=1 zk-old9`
	testAdminPW  = `  oad4 -Wy8 \ $HOME jm-old3  `
	nextServerPW = `nvs5 Rr$&"';%,=<>3 xp-new6`
	nextAdminPW  = `nad2 -Vb6 "x" hq-new8`
)

// pieces lists each password and every space-separated piece of it that is
// at least 4 bytes long: none of them may ever be shown.
func pieces(passwords ...string) []string {
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

// noPieces fails the test if text, or any string inside it when it is JSON
// (so escaped characters are compared as they are meant), has one of
// pieces.
func noPieces(t *testing.T, what, text string, pieces []string) {
	t.Helper()
	texts := []string{text}
	for _, line := range strings.Split(text, "\n") {
		var v any
		if json.Unmarshal([]byte(line), &v) == nil {
			texts = jsonStrings(v, texts)
		}
	}
	for _, s := range texts {
		for _, p := range pieces {
			if strings.Contains(s, p) {
				t.Errorf("the %s has %q of a password:\n%s", what, p, s)
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

// recordSup is a supervisor that starts nothing. It records what it is
// asked to start, reads the secrets file the command line names, and fails
// with an error that repeats the whole command line: the worst the tools
// of a supervisor could do.
type recordSup struct {
	mu    sync.Mutex
	specs []supervisor.Spec
	files [][]byte      // the secrets file as it was at each start
	modes []fs.FileMode // and its permission bits
}

func (s *recordSup) Start(_ context.Context, spec supervisor.Spec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	var content []byte
	var mode fs.FileMode
	for _, a := range spec.Args {
		if path, ok := strings.CutPrefix(a, gameserver.SecretsFileSwitch+"="); ok {
			content, _ = os.ReadFile(path)
			if fi, err := os.Stat(path); err == nil {
				mode = fi.Mode().Perm()
			}
		}
	}
	s.files = append(s.files, content)
	s.modes = append(s.modes, mode)
	return fmt.Errorf("start failed: %s %s", spec.Executable, strings.Join(spec.Args, " "))
}
func (s *recordSup) Stop(context.Context, string, time.Duration) error { return nil }
func (s *recordSup) Status(name string) (supervisor.Proc, error) {
	return supervisor.Proc{Name: name}, supervisor.ErrNotFound
}
func (s *recordSup) Adopt() ([]supervisor.Proc, error) { return nil, nil }
func (s *recordSup) Kind() string                      { return supervisor.KindProcess }
func (s *recordSup) LimitsEnforced() bool              { return false }
func (s *recordSup) Close() error                      { return nil }

func (s *recordSup) last(t *testing.T) (supervisor.Spec, []byte, fs.FileMode) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.specs) == 0 {
		t.Fatal("the supervisor was not asked to start anything")
	}
	n := len(s.specs) - 1
	return s.specs[n], s.files[n], s.modes[n]
}

// The command line ESM builds has no password and no password switch: it
// names the secrets file, which holds the passwords while the server is
// started. So even a supervisor that repeats the whole command line in its
// error shows no password anywhere.
func TestPasswordsAreNeverOnTheCommandLine(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(testServerPW), AdminPassword: ptr(testAdminPW)}); err != nil {
		t.Fatal(err)
	}
	secret := pieces(testServerPW, testAdminPW)
	_, err := m.Start(context.Background(), "alpha", true)
	if fault.CodeOf(err) != "start_failed" {
		t.Fatalf("start error: %v", err)
	}
	noPieces(t, "start error", err.Error(), secret)

	spec, file, mode := sup.last(t)
	wantFile, _ := r.st.Abs(runSecretsFile("alpha"))
	named := 0
	for _, a := range spec.Args {
		if a == gameserver.SecretsFileSwitch+"="+wantFile {
			named++
		}
		if strings.Contains(strings.ToLower(a), "password") {
			t.Errorf("a password switch is on the command line: %q", a)
		}
		noPieces(t, "command line", a, secret)
	}
	if named != 1 {
		t.Fatalf("the command line names the secrets file %d times: %q", named, spec.Args)
	}
	if string(file) != string(gameserver.Secrets{ServerPassword: testServerPW, AdminPassword: testAdminPW}.File()) {
		t.Fatalf("the secrets file at the start: %q", file)
	}
	if runtime.GOOS != "windows" && mode != 0o600 {
		t.Fatalf("the secrets file has mode %o, want 600", mode)
	}
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("the secrets file is still there after the start failed")
	}

	// A failing restart after a crash records the error as the reason.
	in := m.lookup("alpha")
	in.mu.Lock()
	in.gen++
	gen := in.gen
	m.setLocked(in, Crashed, "test crash")
	in.mu.Unlock()
	m.restartAfterCrash(in, gen)
	st, _ := m.Status("alpha")
	if st.State != Failed || !strings.Contains(st.Reason, "restart after a crash failed") {
		t.Fatalf("after a failed restart: %+v", st)
	}
	noPieces(t, "state reason", st.Reason, secret)
	evs, _ := r.bus.Since(0, events.Filter{})
	raw, _ := json.Marshal(evs)
	noPieces(t, "events", string(raw), secret)
	state, _ := r.st.ReadFile(stateFile("alpha"))
	noPieces(t, "state.json", string(state), secret)
	noPieces(t, "service log", r.log.String(), secret)
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("the secrets file is still there after the restart failed")
	}

	// Without passwords the file is written all the same, empty.
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(""), AdminPassword: ptr("")}); err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background(), "alpha", false)
	if _, file, _ := sup.last(t); string(file) != "{}\n" {
		t.Fatalf("the secrets file without passwords: %q", file)
	}
}

// rawOutput returns what the server of an instance wrote, as it is on
// disk: its log and its console output.
func (r *rig) rawOutput(name string) string {
	r.t.Helper()
	log, _ := r.st.ReadFile(serverLog(name))
	console, _ := r.st.ReadFile(consoleFile(name))
	return string(log) + "\n" + string(console)
}

// has asks the running fake of an instance whether these are its
// passwords.
func (r *rig) has(v View, want gameserver.Secrets) bool {
	r.t.Helper()
	join, err := testkit.CheckPassword(v.BindIP, v.Ports.Query, testkit.JoinPassword, want.ServerPassword)
	if err != nil {
		r.t.Fatal(err)
	}
	admin, err := testkit.CheckPassword(v.BindIP, v.Ports.Query, testkit.AdminPassword, want.AdminPassword)
	if err != nil {
		r.t.Fatal(err)
	}
	return join && admin
}

func (r *rig) runSecrets(name string) (gameserver.Secrets, bool) {
	r.t.Helper()
	b, err := r.st.ReadFile(runSecretsFile(name))
	if err != nil {
		return gameserver.Secrets{}, false
	}
	s, err := gameserver.ParseSecretsFile(b)
	if err != nil {
		r.t.Fatalf("the secrets file cannot be read back: %v", err)
	}
	return s, true
}

// A server gets its passwords through the secrets file: the file is there,
// private, while the server runs, holds what the server was started with,
// is written afresh at each start and removed when the server has stopped.
// What the server writes, and everything ESM says, has no password in it,
// so there is nothing to mask.
func TestSecretsFileLifecycle(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	v := r.create("alpha", nil)
	ctx := context.Background()
	first := gameserver.Secrets{ServerPassword: testServerPW, AdminPassword: testAdminPW}
	next := gameserver.Secrets{ServerPassword: nextServerPW, AdminPassword: nextAdminPW}
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: &first.ServerPassword, AdminPassword: &first.AdminPassword}); err != nil {
		t.Fatal(err)
	}
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("a secrets file exists before any start")
	}
	st, err := r.m.Start(ctx, "alpha", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.runSecrets("alpha"); !ok || got != first {
		t.Fatalf("the secrets file while the server runs: %+v %v", got, ok)
	}
	if runtime.GOOS != "windows" {
		for rel, want := range map[string]fs.FileMode{runSecretsFile("alpha"): 0o600, secretsFile("alpha"): 0o600, instDir("alpha") + "/run": 0o700, instDir("alpha"): 0o700} {
			if fi, err := r.st.Stat(rel); err != nil || fi.Mode().Perm() != want {
				t.Errorf("%s has mode %v (%v), want %o", rel, fi.Mode().Perm(), err, want)
			}
		}
	}
	data, _ := r.st.Abs(dataDir("alpha"))
	if gs, err := gameserver.ReadStatus(data); err != nil || !gs.ServerPasswordSet || !gs.AdminPasswordSet || gs.PID != st.PID {
		t.Fatalf("the server does not say it has both passwords: %+v %v", gs, err)
	}
	if !r.has(v, first) {
		t.Fatal("the server did not get the passwords of the secrets file")
	}
	if r.has(v, gameserver.Secrets{}) || r.has(v, next) {
		t.Fatal("the server accepts other passwords")
	}

	// A change reaches the server with its next start, not before.
	_, restart, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: &next.ServerPassword, AdminPassword: &next.AdminPassword})
	if err != nil || !restart {
		t.Fatalf("change while running: %v %v", restart, err)
	}
	if got, _ := r.runSecrets("alpha"); got != first || !r.has(v, first) {
		t.Fatalf("a change while the server runs touched its secrets file: %+v", got)
	}
	if _, err := r.m.Restart(ctx, "alpha", "", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.runSecrets("alpha"); got != next || !r.has(v, next) || r.has(v, first) {
		t.Fatalf("after a restart the server does not have the new passwords: %+v", got)
	}

	// A crash removes the file; the restart that follows writes it again.
	pid := r.waitState("alpha", Running, 10*time.Second).PID
	if err := testkit.WriteControl(data, testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		st, _ := r.m.Status("alpha")
		return st.State == Running && st.PID != pid
	}, "no restart after the crash")
	if got, ok := r.runSecrets("alpha"); !ok || got != next || !r.has(v, next) {
		t.Fatalf("after a crash restart: %+v %v", got, ok)
	}

	if _, err := r.m.Stop(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("the secrets file is still there after the stop")
	}

	// A server that stops by itself and is not restarted leaves no file.
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{RestartOnCrash: ptr(false)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if err := testkit.WriteControl(data, testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	r.waitState("alpha", Crashed, 10*time.Second)
	if r.st.Exists(runSecretsFile("alpha")) {
		t.Fatal("the secrets file is still there after the server crashed for good")
	}

	// Nothing the server wrote and nothing ESM said has a password: not
	// masked, but never there.
	secret := pieces(first.ServerPassword, first.AdminPassword, next.ServerPassword, next.AdminPassword)
	raw := r.rawOutput("alpha")
	if strings.Count(raw, "Command line: ") != 8 {
		t.Fatalf("the server printed its command line %d times, want 4 in its log and 4 in its output", strings.Count(raw, "Command line: "))
	}
	if strings.Contains(strings.ToLower(raw), "password") || !strings.Contains(raw, " "+gameserver.SecretsFileSwitch+"=") {
		t.Fatalf("the server's own files show a password switch, or no secrets file switch:\n%s", raw)
	}
	noPieces(t, "server's own files", raw, secret)
	for _, source := range []string{SourceServer, SourceConsole} {
		lines, err := r.m.Logs("alpha", source, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(lines, "\n")
		if strings.Contains(text, gameserver.Mask) {
			t.Errorf("the %s log has something masked although nothing secret was in it:\n%s", source, text)
		}
		noPieces(t, source+" log", text, secret)
	}
	evs, _ := r.bus.Since(0, events.Filter{})
	evRaw, _ := json.Marshal(evs)
	noPieces(t, "events", string(evRaw), secret)
	state, _ := r.st.ReadFile(stateFile("alpha"))
	noPieces(t, "state.json", string(state), secret)
	noPieces(t, "service log", r.log.String(), secret)
	// ESM itself never writes a command line anywhere.
	for what, text := range map[string]string{"service log": r.log.String(), "state.json": string(state)} {
		for _, part := range []string{"-Port=", "-QueryPort=", gameserver.SecretsFileSwitch, gameserver.DataDirSwitch + "=", "-log"} {
			if strings.Contains(text, part) {
				t.Errorf("the %s has %q of a command line:\n%s", what, part, text)
			}
		}
	}
}

// What ESM says itself is never matched against a password: a password
// that is one of ESM's own words leaves those words as they are, and the
// server still becomes ready. (Words shorter than 8 characters, like
// "ready", cannot be passwords at all.)
func TestOwnWordsAreNeverMasked(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr("ready")}); fault.CodeOf(err) != "password_too_short" {
		t.Fatalf("the password \"ready\": %v", err)
	}
	const restarted, stopping = "restarted", "stopped for a restart"
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(restarted), AdminPassword: ptr(stopping)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	st, err := r.m.Restart(ctx, "alpha", "", true)
	if err != nil || st.State != Running || st.Reason != "ready ("+st.ReadySource+")" {
		t.Fatalf("restart: %+v %v", st, err)
	}
	if st, err = r.m.Stop(ctx, "alpha"); err != nil || st.Reason != "stopped" {
		t.Fatalf("stop: %+v %v", st, err)
	}
	_, err = r.m.Start(ctx, "ghost", false)
	if err == nil || strings.Contains(err.Error(), gameserver.Mask) {
		t.Fatalf("error text: %v", err)
	}
	evs, _ := r.bus.Since(0, events.Filter{Types: []string{events.TypeState}})
	raw, _ := json.Marshal(evs)
	if strings.Contains(string(raw), gameserver.Mask) || !strings.Contains(string(raw), `"reason":"`+restarted+`"`) ||
		!strings.Contains(string(raw), `"reason":"`+stopping+`"`) {
		t.Fatalf("state events: %s", raw)
	}
}

// Passwords have at least 8 characters, and a password may not occur, in
// any case, in a setting every role sees and the server may print: the
// output filter would mask that setting and so give the password away.
// Neither refusal repeats the value.
func TestPasswordRules(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	v, err := r.m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr("Alpha EU"),
		World: ptr("north"), Cluster: ptr("eu-1"), Rules: &map[string]string{"Difficulty": "Hard-Mode-77", "Motd": "welcome"},
		ExtraArgs: &[]string{"-Banner=OpenSesame-42"}}})
	if err != nil {
		t.Fatal(err)
	}
	refused := func(what string, err error, code, value string) {
		t.Helper()
		if fault.CodeOf(err) != code || !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: %v, want %s", what, err, code)
		} else if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(value)) {
			t.Errorf("%s: the refusal repeats the value: %v", what, err)
		}
	}
	for _, short := range []string{"Zq7-abc", "密码密码密码密"} {
		_, _, err := r.m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr(short)})
		refused("short password", err, "password_too_short", short)
	}
	for pw, field := range map[string]string{
		"hard-mode-77":      "rules",
		"OPENSESAME-42":     "extra_args",
		"Alpha EU":          "display_name",
		"difficulty":        "rules",
		"-Banner=OpenSesam": "extra_args",
	} {
		_, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(pw)})
		refused("password in "+field, err, "password_in_settings", pw)
		if err != nil && !strings.Contains(err.Error(), "occurs in "+field) {
			t.Errorf("%q: the refusal does not name %s: %v", pw, field, err)
		}
	}
	if fl := r.m.secretFlags("alpha"); fl.ServerPasswordSet || fl.AdminPasswordSet {
		t.Fatalf("a refused password was stored: %+v", fl)
	}
	const pw = "Zq7-Gate-Keeper"
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(pw), AdminPassword: ptr("an-admin-pw-9")}); err != nil {
		t.Fatal(err)
	}
	for field, s := range map[string]Settings{
		"display_name": {DisplayName: ptr("The zq7-gate-keeper server")},
		"world":        {World: ptr("zq7-gate-keeperx")},
		"cluster":      {Cluster: ptr("zq7-gate-keeper")},
		"rules":        {Rules: &map[string]string{"Motd": "say ZQ7-GATE-KEEPER"}},
		"extra_args":   {ExtraArgs: &[]string{"-Banner=" + pw}},
	} {
		_, _, err := r.m.Patch("alpha", PatchRequest{Settings: s})
		refused("change of "+field, err, "password_in_settings", pw)
	}
	if got, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha West")}}); err != nil || got.DisplayName != "Alpha West" {
		t.Fatalf("an ordinary change: %+v %v", got, err)
	}
	if v.Name != "alpha" {
		t.Fatal(v.Name)
	}
}

// Should a server print its passwords itself, the lines it wrote are still
// shown with them masked: the ones now set and the ones the server was
// started with.
func TestServerOutputIsFiltered(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	const started, admin, later = "started-with-pw7", "the-admin-pw9", "set-later-pw3"
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(started), AdminPassword: ptr(admin)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ServerPassword: ptr(later)}); err != nil {
		t.Fatal(err)
	}
	say := func(rel, line string) {
		t.Helper()
		f, err := r.st.OpenAppend(rel, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	say(serverLog("alpha"), "Admin logged in with "+admin+" after trying "+started+" and "+later)
	say(serverLog("alpha"), "Join request: /Somewhere?Name=Player?Password=unknown-value")
	say(consoleFile("alpha"), "console: "+later+" / "+started)
	secret := []string{started, admin, later, "unknown-value"}
	testkit.Eventually(t, 10*time.Second, func() bool {
		evs, _ := r.bus.Since(0, events.Filter{Types: []string{events.TypeLog}})
		raw, _ := json.Marshal(evs)
		return strings.Contains(string(raw), "Admin logged in with") && strings.Contains(string(raw), "console: ")
	}, "the new lines did not show up as log events")
	evs, _ := r.bus.Since(0, events.Filter{})
	raw, _ := json.Marshal(evs)
	noPieces(t, "events", string(raw), secret)
	for _, source := range []string{SourceServer, SourceConsole} {
		lines, err := r.m.Logs("alpha", source, 1000, true)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(lines, "\n")
		noPieces(t, source+" log", text, secret)
		if !strings.Contains(text, gameserver.Mask) {
			t.Errorf("nothing is masked in the %s log:\n%s", source, text)
		}
	}
	lines, _ := r.m.Logs("alpha", SourceServer, 1000, true)
	want := "Admin logged in with " + gameserver.Mask + " after trying " + gameserver.Mask + " and " + gameserver.Mask
	if text := strings.Join(lines, "\n"); !strings.Contains(text, want) || !strings.Contains(text, "?Password="+gameserver.Mask) {
		t.Fatalf("the server log:\n%s", text)
	}
}
