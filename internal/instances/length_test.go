package instances

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// lineCap is the longest server command line a change may lead to,
// engineLimit the longest the server's engine starts with, and startLimit
// the longest ESM starts a server with, 256 under engineLimit, all counted
// as the engine counts (testkit/cmdline).
const (
	lineCap     = 12000
	engineLimit = 16383
	startLimit  = 16127
)

// startedLine starts name on a supervisor that starts nothing and returns
// the line the server's launcher would join its arguments into.
func startedLine(t *testing.T, m *Manager, sup *recordSup, name string) string {
	t.Helper()
	m.Start(context.Background(), name, false)
	spec, _, _ := sup.last(t)
	return cmdline.Join(spec.Args)
}

// pad sets rules named Zp00, Zp01 and so on in rules, so that a command
// line of base characters without them has exactly want with them. A rule
// costs the seven characters of " -Zp00=" and its value.
func pad(t *testing.T, rules map[string]string, base, want int) {
	t.Helper()
	const head = len(" -Zp00=")
	for k := range rules {
		if strings.HasPrefix(k, "Zp") {
			delete(rules, k)
		}
	}
	left := want - base
	for i := 0; left > 0; i++ {
		n := left - head
		if n > 250 {
			// What is left after this rule must be a whole rule again.
			if n = 250; left-head-n <= head {
				n = 240
			}
		}
		if n < 1 || i >= maxRules {
			t.Fatalf("cannot pad a line of %d characters to %d", base, want)
		}
		rules[fmt.Sprintf("Zp%02d", i)] = strings.Repeat("x", n)
		left -= head + n
	}
}

// The server's engine does not start with a command line of more than 16383
// characters. ESM holds the line of an instance, with everything on it (its
// own switches, the folders, the rules, the extra arguments and, with
// command-line delivery, the secrets), to 12000: a change that would make
// it longer is refused, with both numbers and nothing of a value. The line
// is counted as the engine counts it: the quotes around a value with a
// space, and two for a character beyond U+FFFF.
func TestCommandLineLengthIsHeld(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if _, err := m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1")}}); err != nil {
		t.Fatal(err)
	}
	tooLong := func(what string, err error) {
		t.Helper()
		if fault.CodeOf(err) != "command_line_too_long" || !fault.Is(err, fault.Invalid) {
			t.Fatalf("%s: %v, want command_line_too_long", what, err)
		}
		if !strings.Contains(err.Error(), "12000") || strings.Contains(err.Error(), "xxx") || strings.Contains(strings.ToLower(err.Error()), "zq7") {
			t.Errorf("%s: the refusal does not name the limit, or repeats a value: %v", what, err)
		}
	}
	rules := map[string]string{}
	set := func() error {
		_, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &rules}})
		return err
	}
	units := func() int { return cmdline.Units(startedLine(t, m, sup, "alpha")) }
	base := units()

	// Rule after rule, until the line is full.
	for i, last := 0, base; ; i++ {
		if i >= 64 {
			t.Fatalf("64 rules of 250 characters were accepted: a command line of %d characters", last)
		}
		key := fmt.Sprintf("R%02d", i)
		rules[key] = strings.Repeat("x", 250)
		if err := set(); err != nil {
			tooLong(fmt.Sprintf("rule %d", i+1), err)
			if last+256 <= lineCap {
				t.Fatalf("refused at %d + 256 characters", last)
			}
			break
		}
		if last = units(); last > lineCap {
			t.Fatalf("accepted a command line of %d characters", last)
		}
	}
	if v, _ := m.Get("alpha"); len(v.Rules) != len(rules)-1 || slices.Contains(v.Warnings, "command_line_too_long") {
		t.Fatalf("after the refusal: %d rules, warnings %q", len(v.Rules), v.Warnings)
	}
	// A new instance is held to it too.
	_, err := m.Create(CreateRequest{Name: "beta", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: &rules}})
	tooLong("a new instance", err)
	if _, err := m.Get("beta"); !fault.Is(err, fault.NotFound) {
		t.Fatalf("a refused instance was created: %v", err)
	}

	// To the character: a line of exactly 12000 is taken, one more is not.
	clear(rules)
	pad(t, rules, base, lineCap)
	full := rules["Zp00"]
	if len(full) < 8 {
		t.Fatalf("the first padding rule has %d characters", len(full))
	}
	exact := func(what, value string) {
		t.Helper()
		rules["Zp00"] = value
		if err := set(); err != nil {
			t.Fatalf("%s, a line of %d: %v", what, lineCap, err)
		}
		if got := units(); got != lineCap {
			t.Fatalf("%s: the line has %d characters, not %d", what, got, lineCap)
		}
	}
	over := func(what, value string) {
		t.Helper()
		rules["Zp00"] = value
		tooLong(what, set())
		rules["Zp00"] = full
	}
	exact("plain", full)
	over("one character more", full+"x")
	// A character beyond U+FFFF counts twice, as the engine keeps it.
	exact("a character of two units", full[2:]+"\U0001F642")
	over("a character of two units too many", full[1:]+"\U0001F642")
	// The quotes the launcher puts around a value with a space count.
	exact("a quoted value", full[4:]+" x")
	over("the quotes too many", full[3:]+" x")
	exact("plain again", full)

	// Everything on the line counts: the display name and an extra
	// argument too. A change that makes the line no longer is still taken.
	_, _, err = m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("alpha2")}})
	tooLong("a longer display name", err)
	_, _, err = m.Patch("alpha", PatchRequest{Settings: Settings{ExtraArgs: &[]string{"-X"}}})
	tooLong("an extra argument", err)
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("alph"), Autostart: ptr(true)}}); err != nil {
		t.Errorf("a change that makes the line shorter: %v", err)
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("alpha")}}); err != nil {
		t.Errorf("back to the full line: %v", err)
	}

	// The secrets are on the line with command-line delivery only.
	pw, token := strings.Repeat("Zq7JxKv9", 16), strings.Repeat("Zq7-Tk4.", 32)
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{ServerPassword: &pw, AdminPassword: ptr(pw[:20]), AnnouncementsToken: &token,
		ProblemReportToken: &token}); err != nil {
		t.Fatalf("secrets in the secrets file are not on the line: %v", err)
	}
	if got := units(); got != lineCap {
		t.Fatalf("the line has %d characters", got)
	}
	_, _, err = m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}})
	tooLong("command-line delivery on a full line", err)
	clear(rules)
	if err := set(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatalf("command-line delivery with room on the line: %v", err)
	}
	pad(t, rules, units(), lineCap)
	if err := set(); err != nil {
		t.Fatalf("a full line with the secrets on it: %v", err)
	}
	if got := units(); got != lineCap {
		t.Fatalf("the line has %d characters", got)
	}
	_, _, err = m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr(pw[:21])})
	tooLong("a longer password on a full line", err)
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr("Bn4" + pw[:17])}); err != nil {
		t.Errorf("another password of the same length: %v", err)
	}
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{AnnouncementsToken: ptr("")}); err != nil {
		t.Errorf("a token removed: %v", err)
	}
	if got := units(); got >= lineCap {
		t.Fatalf("the line has %d characters", got)
	}
}

// An instance stored by an earlier version with a longer command line
// keeps loading. It carries the warning command_line_too_long; a change
// that leaves the line as it is or makes it shorter is taken, one that
// makes it longer is not. Up to what ESM starts a server with, the server
// is started as before and the service log says that its line is long.
// Beyond that the engine would stop at once: ESM does not start the
// server, and says why.
func TestStoredCommandLineThatIsTooLong(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	r.create("beta", nil)
	store := func(name string, n int) {
		t.Helper()
		var in Instance
		if err := r.st.ReadJSON(instFile(name), &in); err != nil {
			t.Fatal(err)
		}
		in.Rules = map[string]string{}
		for i := 0; i < n; i++ {
			in.Rules[fmt.Sprintf("R%02d", i)] = strings.Repeat("x", 250)
		}
		if err := r.st.WriteJSON(instFile(name), in, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store("alpha", 52) // 52 * 256 = 13312 and the rest: over 12000, under 16383
	store("beta", 64)  // 64 * 256 = 16384: more than the engine starts with
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()

	for _, name := range []string{"alpha", "beta"} {
		v, err := m.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(v.Warnings, "command_line_too_long") {
			t.Errorf("%s: warnings %q", name, v.Warnings)
		}
	}
	if text := WarningText("command_line_too_long"); text == "command_line_too_long" || !strings.Contains(text, "12000") || !strings.Contains(text, "16383") {
		t.Errorf("the warning in words: %q", text)
	}

	// alpha: started as stored.
	line := startedLine(t, m, sup, "alpha")
	if n := cmdline.Units(line); n <= lineCap || n > engineLimit || cmdline.Refuses(line) != "" {
		t.Fatalf("alpha's line has %d characters", n)
	}
	said := false
	for _, entry := range strings.Split(r.log.String(), "\n") {
		if strings.Contains(entry, "longer than") {
			if said = true; strings.Contains(entry, "xxx") {
				t.Errorf("the service log repeats a value: %s", entry)
			}
		}
	}
	if !said {
		t.Errorf("the service log does not say that the line is long:\n%s", r.log.String())
	}
	v, _ := m.Get("alpha")
	same := v.Rules
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Autostart: ptr(false), Rules: &same}}); err != nil {
		t.Errorf("a change that leaves the line as it is: %v", err)
	}
	longer := map[string]string{}
	for k, val := range same {
		longer[k] = val
	}
	longer["More"] = "1"
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &longer}}); fault.CodeOf(err) != "command_line_too_long" {
		t.Errorf("a change that makes a long line longer: %v", err)
	}
	delete(longer, "More")
	delete(longer, "R00")
	if v, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &longer}}); err != nil || !slices.Contains(v.Warnings, "command_line_too_long") {
		t.Errorf("a change that makes a long line shorter: %v, warnings %q", err, v.Warnings)
	}
	short := map[string]string{"R01": "x"}
	if v, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &short}}); err != nil || slices.Contains(v.Warnings, "command_line_too_long") {
		t.Errorf("a short line: %v, warnings %q", err, v.Warnings)
	}

	// beta: not started.
	started := func() int {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.specs)
	}
	before := started()
	_, err := m.Start(context.Background(), "beta", false)
	if fault.CodeOf(err) != "command_line_too_long" || !fault.Is(err, fault.Conflict) || !strings.Contains(err.Error(), "16383") ||
		strings.Contains(err.Error(), "xxx") {
		t.Fatalf("start of beta: %v", err)
	}
	if started() != before {
		t.Fatal("the supervisor was asked to start a server whose engine would stop at once")
	}
	if st, _ := m.Status("beta"); st.State.active() {
		t.Fatalf("beta is %s", st.State)
	}
}

// What starts the server may put more on its command line than ESM passes:
// a build's start script puts the name of the game's project in front, say.
// ESM does not know what a build's script adds, so it keeps 256 of the
// 16383 characters the engine starts with free for that: a line of 16127,
// as ESM passes it, is started, and one character more is not (409
// command_line_too_long, with the numbers and nothing of a value). Only an
// instance stored by an earlier version gets there.
func TestStartLeavesRoomForTheStartScript(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.Rules = map[string]string{}
	store := func() {
		t.Helper()
		if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store()
	sup := &recordSup{}
	m := r.newManager(sup)
	base := cmdline.Units(startedLine(t, m, sup, "alpha"))
	m.Close()

	for _, c := range []struct {
		n      int
		starts bool
	}{{startLimit, true}, {startLimit + 1, false}, {engineLimit, false}} {
		pad(t, in.Rules, base, c.n)
		store()
		sup := &recordSup{}
		m := r.newManager(sup)
		_, err := m.Start(context.Background(), "alpha", false)
		sup.mu.Lock()
		asked := len(sup.specs)
		sup.mu.Unlock()
		switch {
		case c.starts && asked != 1:
			t.Errorf("a line of %d characters is not started: %v", c.n, err)
		case c.starts:
			if got := cmdline.Units(cmdline.Join(sup.specs[0].Args)); got != c.n {
				t.Errorf("the line has %d characters, not %d", got, c.n)
			}
		case asked != 0:
			t.Errorf("a line of %d characters was started", c.n)
		case fault.CodeOf(err) != "command_line_too_long" || !fault.Is(err, fault.Conflict):
			t.Errorf("a line of %d characters: %v, want 409 command_line_too_long", c.n, err)
		default:
			msg := err.Error()
			for _, n := range []int{c.n, engineLimit, startLimit, engineLimit - startLimit} {
				if !strings.Contains(msg, strconv.Itoa(n)) {
					t.Errorf("the refusal does not name %d: %v", n, err)
				}
			}
			if strings.Contains(msg, "xxx") {
				t.Errorf("the refusal repeats a value: %v", err)
			}
		}
		m.Close()
	}
}
