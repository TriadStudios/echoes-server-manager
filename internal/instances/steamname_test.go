package instances

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// answeredName asks the server of an instance on its query port for the
// name it answers with. The fake server takes that name from
// -SteamServerName, where the game takes the name it gives Steam from.
func answeredName(t *testing.T, m *Manager, name string) string {
	t.Helper()
	v, err := m.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	var info gameserver.Info
	testkit.Eventually(t, 5*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		var err error
		info, err = gameserver.QueryInfo(ctx, v.BindIP, v.Ports.Query)
		return err == nil
	}, "the server of %s does not answer its query port", name)
	return info.Name
}

// The server name reaches the fake server as the name of its session
// (-ServerName) and as the name for Steam's server list
// (-SteamServerName), which it answers its query port with, a name with
// spaces whole. A new name reaches a running server at its next start, not
// before, and the change says that a restart is needed. Without a display
// name the fake answers with the instance's name.
func TestSteamServerNameReachesTheServer(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	const first, second = "Alpha EU - west (PvE)", "Beta, the second one"
	if _, err := r.m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil),
		DisplayName: ptr(first)}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if got := answeredName(t, r.m, "alpha"); got != first {
		t.Fatalf("the query port answers with the name %q, want %q", got, first)
	}
	log, _ := r.st.Abs(serverLog("alpha"))
	if text, _ := os.ReadFile(log); !strings.Contains(string(text), "Dedicated session '"+first+"' registered") {
		t.Fatalf("the session is not named %q:\n%s", first, text)
	}

	v, restart, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(second)}})
	if err != nil || !restart || !v.Status.RestartRequired {
		t.Fatalf("a new name for a running server: restart %v, status %+v, %v", restart, v.Status, err)
	}
	if got := answeredName(t, r.m, "alpha"); got != first {
		t.Fatalf("before its next start the query port answers with the name %q, want %q", got, first)
	}
	if st, err := r.m.Restart(ctx, "alpha", "", true); err != nil || st.RestartRequired {
		t.Fatalf("restart: %+v %v", st, err)
	}
	if got := answeredName(t, r.m, "alpha"); got != second {
		t.Fatalf("after the restart the query port answers with the name %q, want %q", got, second)
	}

	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Restart(ctx, "alpha", "", true); err != nil {
		t.Fatal(err)
	}
	if got := answeredName(t, r.m, "alpha"); got != "alpha" {
		t.Fatalf("without a display name the query port answers with the name %q, want the instance's name", got)
	}
}

// ESM sets -SteamServerName itself, from the server name. No rule, extra
// argument or value can set it, name it a second time or carry it inside
// another value, at a create or at a change, and the refusal repeats
// nothing of the value. A switch whose name only ends in SteamServerName
// is another switch to the server, and the server still reads ESM's.
func TestSteamServerNameCannotBeSetOrOverridden(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	const display, hidden = "Alpha EU", "Zq7 Evil Wx4"
	if _, err := m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr(display)}}); err != nil {
		t.Fatal(err)
	}
	attempts := map[string]Settings{
		"rule":                        {Rules: &map[string]string{"SteamServerName": hidden}},
		"rule in lower case":          {Rules: &map[string]string{"steamservername": "Zq7Evil"}},
		"rule in upper case":          {Rules: &map[string]string{"STEAMSERVERNAME": "Zq7Evil"}},
		"rule key with a _":           {Rules: &map[string]string{"x_SteamServerName": "Zq7Evil"}},
		"extra argument":              {ExtraArgs: &[]string{"-SteamServerName=" + hidden}},
		"extra argument, other case":  {ExtraArgs: &[]string{"-steamservername=Zq7Evil"}},
		"extra argument, two dashes":  {ExtraArgs: &[]string{"--SteamServerName=Zq7Evil"}},
		"extra argument, bare":        {ExtraArgs: &[]string{"-SteamServerName"}},
		"extra argument, colon form":  {ExtraArgs: &[]string{"-SteamServerName:Zq7Evil"}},
		"extra argument, space form":  {ExtraArgs: &[]string{"-SteamServerName " + hidden}},
		"extra argument with a _":     {ExtraArgs: &[]string{"-x_SteamServerName=Zq7Evil"}},
		"display name, unspaced":      {DisplayName: ptr("Alpha;SteamServerName=Zq7Evil")},
		"display name, spaced":        {DisplayName: ptr("Alpha SteamServerName=" + hidden)},
		"display name, a switch":      {DisplayName: ptr("Alpha -SteamServerName Zq7Evil")},
		"display name, a dash":        {DisplayName: ptr("Alpha;-SteamServerName")},
		"rule value":                  {Rules: &map[string]string{"Banner": "a;SteamServerName=Zq7Evil"}},
		"rule value with a space":     {Rules: &map[string]string{"Banner": "Hi SteamServerName=" + hidden}},
		"game setting value":          {Rules: &map[string]string{"MOTD": "Hi, SteamServerName=" + hidden}},
		"extra argument value":        {ExtraArgs: &[]string{"-Banner=x;-SteamServerName=Zq7Evil"}},
		"extra argument value spaced": {ExtraArgs: &[]string{"-Banner=Hi -SteamServerName=" + hidden}},
		"map":                         {Map: ptr("/Game/Maps/x;SteamServerName=Zq7Evil")},
	}
	for what, s := range attempts {
		_, _, err := m.Patch("alpha", PatchRequest{Settings: s})
		if !fault.Is(err, fault.Invalid) || strings.Contains(err.Error(), "Zq7") {
			t.Errorf("%s at a change: %v", what, err)
		}
		s.BindIP = ptr("127.0.0.1")
		_, err = m.Create(CreateRequest{Name: "beta", Settings: s})
		if !fault.Is(err, fault.Invalid) || strings.Contains(err.Error(), "Zq7") {
			t.Errorf("%s at a create: %v", what, err)
		}
	}
	if len(m.List()) != 1 {
		t.Fatalf("a refused instance was created: %d instances", len(m.List()))
	}
	if v, _ := m.Get("alpha"); v.DisplayName != display || len(v.Rules) != 0 || len(v.ExtraArgs) != 0 {
		t.Fatalf("after the refusals: %+v", v.Instance)
	}

	// Another switch whose name ends in SteamServerName, a value that names
	// it as a word, and a rule and an extra argument next to it.
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &map[string]string{"xSteamServerName": "Zq7", "MOTD": "Our SteamServerName is set"},
		ExtraArgs: &[]string{"-Banner=well met", "-NoSteam"}}}); err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background(), "alpha", false)
	spec, _, _ := sup.last(t)
	v, _ := m.Get("alpha")
	for _, p := range serverReads(r, v.Instance, Secrets{}, spec.Args) {
		t.Errorf("%s (the command line %q)", p, spec.Args)
	}
	line := cmdline.Join(spec.Args)
	if got, ok := cmdline.Value(line, "SteamServerName"); !ok || got != display {
		t.Errorf("the server reads SteamServerName as %q (found: %v), want %q", got, ok, display)
	}
	n := 0
	for _, s := range cmdline.Switches(line) {
		if cmdline.SameName(s.Name, "SteamServerName") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the server finds SteamServerName %d times on %q", n, line)
	}
}

// An earlier version did not set -SteamServerName and let a rule or an
// extra argument name it. Such an instance keeps loading and starting: the
// stored switches are passed as stored, after ESM's own, so the server
// reads ESM's (the server name), and the instance carries the warning
// unsafe_stored_settings naming them. A change that keeps them is refused;
// once they are removed the change is taken and the warning goes.
func TestStoredSteamServerNameSwitch(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.DisplayName = "Alpha EU"
	in.Rules = map[string]string{"SteamServerName": "Old Name", "HarvestRate": "2"}
	in.ExtraArgs = []string{"-steamservername=Other", "-NoSteam"}
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()

	v, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rules.SteamServerName", "extra_args.1"}; !slices.Contains(v.Warnings, WarnUnsafeStoredSettings) || !slices.Equal(storedUnsafe(t, v), want) {
		t.Fatalf("warnings %q, settings named %q, want %q", v.Warnings, storedUnsafe(t, v), want)
	}
	m.Start(context.Background(), "alpha", false)
	spec, _, _ := sup.last(t)
	own := slices.Index(spec.Args, gameserver.SteamServerNameSwitch+"=Alpha EU")
	for _, kept := range []string{"-SteamServerName=Old Name", "-steamservername=Other"} {
		if i := slices.Index(spec.Args, kept); i < 0 || own < 0 || i < own {
			t.Errorf("%q is not passed as stored after ESM's own: %q", kept, spec.Args)
		}
	}
	if got, _ := cmdline.Value(cmdline.Join(spec.Args), "SteamServerName"); got != "Alpha EU" {
		t.Errorf("the server reads SteamServerName as %q, want the server name", got)
	}

	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Autostart: ptr(true)}}); !fault.Is(err, fault.Invalid) {
		t.Errorf("a change with the stored switches left in: %v", err)
	}
	fixed, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Autostart: ptr(true), Rules: &map[string]string{"HarvestRate": "2"},
		ExtraArgs: &[]string{"-NoSteam"}}})
	if err != nil {
		t.Fatalf("the change with the stored switches removed: %v", err)
	}
	if slices.Contains(fixed.Warnings, WarnUnsafeStoredSettings) || len(storedUnsafe(t, fixed)) != 0 {
		t.Errorf("after the fix: warnings %q, settings named %q", fixed.Warnings, storedUnsafe(t, fixed))
	}
}

// The server name is on the command line twice, so it counts twice against
// the 12000 characters a change may lead to: a character more in the name
// is two more on the line, a character beyond U+FFFF counts twice in each,
// and the quotes around a name with a space are there twice.
func TestServerNameCountsTwiceOnTheLine(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	const name = "abcde"
	if _, err := m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr(name)}}); err != nil {
		t.Fatal(err)
	}
	units := func() int { return cmdline.Units(startedLine(t, m, sup, "alpha")) }
	base := units()
	if line := startedLine(t, m, sup, "alpha"); strings.Count(line, "="+name+" ") != 2 {
		t.Fatalf("the name is not on the line twice: %q", line)
	}
	set := func(rules map[string]string, display string) error {
		_, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &rules, DisplayName: ptr(display)}})
		return err
	}
	for _, c := range []struct {
		display string
		more    int
	}{{"abcdef", 2}, {"abcd\U0001F642", 2}, {"ab de", 4}, {"ab def", 6}} {
		rules := map[string]string{}
		pad(t, rules, base, lineCap-c.more)
		if err := set(rules, name); err != nil {
			t.Fatal(err)
		}
		if err := set(rules, c.display); err != nil {
			t.Errorf("%q on a line %d short of %d: %v", c.display, c.more, lineCap, err)
		} else if got := units(); got != lineCap {
			t.Errorf("%q: the line has %d characters, want %d", c.display, got, lineCap)
		}
		pad(t, rules, base, lineCap-c.more+1)
		if err := set(rules, name); err != nil {
			t.Fatal(err)
		}
		if err := set(rules, c.display); fault.CodeOf(err) != "command_line_too_long" {
			t.Errorf("%q on a line %d short of %d: %v, want command_line_too_long", c.display, c.more-1, lineCap, err)
		}
		if err := set(map[string]string{}, name); err != nil {
			t.Fatal(err)
		}
	}
}

// The game cuts a name of 64 characters or more to 63, and Steamworks
// limits a game server name to 64 bytes including the terminator, so a
// display name set or changed from this version on has at most 63 bytes
// of UTF-8, which is at most 63 characters however they are counted: 63
// ASCII characters are taken and 64 are not, at a create and at a change,
// and the same goes for names of characters of two and four bytes. The
// refusal names the limit and never the name. Without a display name the
// server gets the instance's name, which is never longer than 32 ASCII
// characters.
func TestServerNameIsHeldTo63Bytes(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if gameserver.MaxServerNameBytes != 63 {
		t.Fatalf("the most a server name may have: %d bytes", gameserver.MaxServerNameBytes)
	}
	if _, err := m.Create(CreateRequest{Name: "alpha", Settings: Settings{BindIP: ptr("127.0.0.1")}}); err != nil {
		t.Fatal(err)
	}
	two, four := "é", "\U0001F642" // two and four bytes of UTF-8
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{strings.Repeat("Zq7", 21), true},           // 63 bytes
		{strings.Repeat("Zq7", 21) + "x", false},    // 64
		{strings.Repeat(two, 31) + "x", true},       // 63 bytes, 32 characters
		{strings.Repeat(two, 32), false},            // 64 bytes, 32 characters
		{strings.Repeat(four, 15) + "xyz", true},    // 63 bytes, 18 characters
		{strings.Repeat(four, 16), false},           // 64 bytes, 16 characters
		{strings.Repeat("Zq7 ", 16) + "Zq7", false}, // 67
	} {
		if (len(c.name) <= 63) != c.ok {
			t.Fatalf("%d bytes: the case is wrong", len(c.name))
		}
		_, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(c.name)}})
		_, cerr := m.Create(CreateRequest{Name: "beta", Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr(c.name)}})
		for what, err := range map[string]error{"a change": err, "a create": cerr} {
			switch {
			case c.ok && err != nil:
				t.Errorf("%d bytes at %s: %v", len(c.name), what, err)
			case !c.ok && !fault.Is(err, fault.Invalid):
				t.Errorf("%d bytes at %s: %v, want refused", len(c.name), what, err)
			case !c.ok && (!strings.Contains(err.Error(), "63 bytes") || strings.Contains(err.Error(), c.name) || strings.Contains(err.Error(), "Zq7")):
				t.Errorf("%d bytes at %s: the refusal does not name the limit, or repeats the name: %v", len(c.name), what, err)
			}
		}
		if cerr == nil {
			if err := m.Delete(context.Background(), "beta", true); err != nil {
				t.Fatal(err)
			}
		}
		if c.ok {
			m.Start(context.Background(), "alpha", false)
			spec, _, _ := sup.last(t)
			if !slices.Contains(spec.Args, gameserver.SteamServerNameSwitch+"="+c.name) {
				t.Errorf("%d bytes: the Steam server name is not passed whole: %q", len(c.name), spec.Args)
			}
		}
	}

	// The longest name of an instance, which the server gets without a
	// display name.
	longest := "a" + strings.Repeat("b-9", 10) + "z"
	if len(longest) != 32 || !ValidName(longest) || ValidName(longest+"x") || len(longest) > gameserver.MaxServerNameBytes {
		t.Fatalf("the longest name of an instance: %q (%d bytes, valid %v)", longest, len(longest), ValidName(longest))
	}
	if _, err := m.Create(CreateRequest{Name: longest, Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	m.Start(context.Background(), longest, false)
	spec, _, _ := sup.last(t)
	if !slices.Contains(spec.Args, gameserver.SteamServerNameSwitch+"="+longest) {
		t.Errorf("without a display name the Steam server name is not the instance's name: %q", spec.Args)
	}
}

// An earlier version took a display name of 64 bytes. Such an instance
// keeps loading and starting with its name passed as stored, and it
// carries the warning unsafe_stored_settings naming display_name, whose
// words say that the name may be cut: the game cuts a name of 64
// characters or more to 63, and the ASCII name here has 64; what Steam
// shows for the one of 32 characters of two bytes each is not known. Other changes are taken, with the name sent back unchanged;
// a change to another name of 64 bytes, or to a longer one, is refused,
// and a name of at most 63 bytes clears the warning.
func TestStoredDisplayNameOf64Bytes(t *testing.T) {
	for _, c := range []struct{ stored, other string }{
		{strings.Repeat("Zq7", 21) + "x", strings.Repeat("Zq7", 21) + "y"}, // 64 bytes, 64 characters
		{strings.Repeat("é", 32), strings.Repeat("é", 31) + "ü"},           // 64 bytes, 32 characters
	} {
		stored := c.stored
		if len(stored) != 64 || len(c.other) != 64 || c.other == stored {
			t.Fatalf("the case is wrong: %d and %d bytes", len(stored), len(c.other))
		}
		r := newRig(t, nil)
		r.install(testkit.BuildOptions{Version: "1.0"})
		r.create("alpha", nil)
		var in Instance
		if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
			t.Fatal(err)
		}
		in.DisplayName = stored
		if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
			t.Fatal(err)
		}
		sup := &recordSup{}
		m := r.newManager(sup)

		v, err := m.Get("alpha")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(v.Warnings, WarnUnsafeStoredSettings) || !slices.Equal(storedUnsafe(t, v), []string{"display_name"}) {
			t.Errorf("%d bytes: warnings %q, settings named %q", len(stored), v.Warnings, storedUnsafe(t, v))
		}
		if words := WarningWords(WarnUnsafeStoredSettings, v.UnsafeStoredSettings); !strings.Contains(words, "passed as stored and may be cut") ||
			strings.Contains(words, "cuts it") || !strings.Contains(words, "display_name") || strings.Contains(words, stored) {
			t.Errorf("the warning in words: %q", words)
		}
		m.Start(context.Background(), "alpha", false)
		spec, _, _ := sup.last(t)
		if !slices.Contains(spec.Args, gameserver.ServerNameSwitch+"="+stored) || !slices.Contains(spec.Args, gameserver.SteamServerNameSwitch+"="+stored) {
			t.Errorf("%d bytes: the stored name is not passed as stored: %q", len(stored), spec.Args)
		}
		if v, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Autostart: ptr(true), DisplayName: ptr(stored)}}); err != nil ||
			!slices.Contains(v.Warnings, WarnUnsafeStoredSettings) {
			t.Errorf("%d bytes: a change of something else, the name sent back as stored: %v, warnings %q", len(stored), err, v.Warnings)
		}
		if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(stored + "x")}}); !fault.Is(err, fault.Invalid) {
			t.Errorf("%d bytes: a longer name: %v", len(stored), err)
		}
		_, _, err = m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(c.other)}})
		if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "63 bytes") || strings.Contains(err.Error(), c.other) ||
			strings.Contains(err.Error(), "Zq7") {
			t.Errorf("%d bytes: a change to another name of %d bytes: %v", len(stored), len(c.other), err)
		}
		if v, err := m.Get("alpha"); err != nil || v.DisplayName != stored || !slices.Contains(v.Warnings, WarnUnsafeStoredSettings) {
			t.Errorf("%d bytes: after the refused change: %v, warnings %q", len(stored), err, v.Warnings)
		}
		fixed, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(stored[:len(stored)-2])}})
		if err != nil || slices.Contains(fixed.Warnings, WarnUnsafeStoredSettings) || len(storedUnsafe(t, fixed)) != 0 {
			t.Errorf("%d bytes: a name of %d bytes: %v, warnings %q, settings named %q", len(stored), len(stored)-2, err, fixed.Warnings, storedUnsafe(t, fixed))
		}
		m.Close()
	}
}
