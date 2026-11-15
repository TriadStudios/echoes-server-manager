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

// listedName asks the server of an instance on its query port for the
// name it answers with: the name Steam's server list shows, which the
// fake server takes from -SteamServerName, as the game does.
func listedName(t *testing.T, m *Manager, name string) string {
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

// The server name reaches the server as the name of its session
// (-ServerName) and as the name Steam's server list shows
// (-SteamServerName), a name with spaces whole. A new name reaches a
// running server at its next start, not before, and the change says that
// a restart is needed. Without a display name the server is listed under
// the instance's name.
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
	if got := listedName(t, r.m, "alpha"); got != first {
		t.Fatalf("the server is listed as %q, want %q", got, first)
	}
	log, _ := r.st.Abs(serverLog("alpha"))
	if text, _ := os.ReadFile(log); !strings.Contains(string(text), "Dedicated session '"+first+"' registered") {
		t.Fatalf("the session is not named %q:\n%s", first, text)
	}

	v, restart, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr(second)}})
	if err != nil || !restart || !v.Status.RestartRequired {
		t.Fatalf("a new name for a running server: restart %v, status %+v, %v", restart, v.Status, err)
	}
	if got := listedName(t, r.m, "alpha"); got != first {
		t.Fatalf("before its next start the server is listed as %q, want %q", got, first)
	}
	if st, err := r.m.Restart(ctx, "alpha", "", true); err != nil || st.RestartRequired {
		t.Fatalf("restart: %+v %v", st, err)
	}
	if got := listedName(t, r.m, "alpha"); got != second {
		t.Fatalf("after the restart the server is listed as %q, want %q", got, second)
	}

	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Restart(ctx, "alpha", "", true); err != nil {
		t.Fatal(err)
	}
	if got := listedName(t, r.m, "alpha"); got != "alpha" {
		t.Fatalf("without a display name the server is listed as %q, want the instance's name", got)
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
