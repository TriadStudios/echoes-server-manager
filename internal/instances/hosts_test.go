package instances

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// hostSpaces are the characters that a host of the game server's engine
// counts as white space, besides the plain space: all of them on Windows,
// the first five on Linux. U+180E is among them although Unicode no longer
// lists it as white space.
var hostSpaces = []rune{0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x85, 0xa0, 0x1680, 0x180e, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005,
	0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}

// notText are characters that are not printable text: controls, format
// characters (soft hyphen, zero width space and joiners, direction marks
// and overrides, word joiner, byte order mark, a tag), private-use and
// unassigned code points, and a noncharacter.
var notText = []rune{0x7f, 0x80, 0x9f, 0xad, 0x61c, 0x200b, 0x200c, 0x200d, 0x200e, 0x200f, 0x202a, 0x202e, 0x2060, 0x2066, 0x2069,
	0xfeff, 0xfff9, 0xe000, 0xf8ff, 0x0378, 0xffff, 0xe0001, 0xf0000, 0x10fffd}

// valueSlots are the places an operator sets text that goes onto the server
// command line as a value, each as the change that sets v there. The
// secrets are not among them: see secretSlots.
func valueSlots(v string) map[string]Settings {
	return map[string]Settings{
		"rule value":           {Rules: &map[string]string{"Banner": v}},
		"MOTD":                 {Rules: &map[string]string{"MOTD": v}},
		"list item":            {Rules: &map[string]string{"ChatBlockedWords": "fine," + v}},
		"only list item":       {Rules: &map[string]string{"AdminPlayerIds": v}},
		"extra argument value": {ExtraArgs: &[]string{"-Banner=" + v}},
		"display name":         {DisplayName: ptr(v)},
		"world":                {World: ptr(v)},
		"map":                  {Map: ptr("Game/" + v)},
	}
}

// secretSlots are the four secrets, each as the request that sets it to v.
func secretSlots(v string) map[string]SecretsRequest {
	return map[string]SecretsRequest{
		"server_password":      {ServerPassword: ptr(v)},
		"admin_password":       {AdminPassword: ptr(v)},
		"announcements_token":  {AnnouncementsToken: ptr(v)},
		"problem_report_token": {ProblemReportToken: ptr(v)},
	}
}

// twoInstances makes the instances "file" and "line", the second with its
// secrets on the command line, on a manager that starts nothing.
func twoInstances(t *testing.T) (*rig, *Manager, *recordSup) {
	t.Helper()
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	t.Cleanup(func() { m.Close() })
	for _, n := range []string{"file", "line"} {
		s := Settings{BindIP: ptr("127.0.0.1")}
		if n == "line" {
			s.SecretDelivery = ptr(gameserver.DeliveryCommandLine)
		}
		if _, err := m.Create(CreateRequest{Name: n, Settings: s}); err != nil {
			t.Fatal(err)
		}
	}
	return r, m, sup
}

// untouched fails the test when one of the two instances kept anything of a
// change that had to be refused.
func untouched(t *testing.T, m *Manager) {
	t.Helper()
	for _, n := range []string{"file", "line"} {
		v, _ := m.Get(n)
		if len(v.Rules) != 0 || len(v.ExtraArgs) != 0 || v.DisplayName != n || v.World != n || v.Map != "" || v.Secrets.any() {
			t.Errorf("%s after the refusals: %+v", n, v.Instance)
		}
	}
}

// What white space is depends on where the server runs: its engine asks the
// host, and Windows counts a good deal more than Linux, U+180E for one,
// which is no white space to Unicode any more. After white space the server
// reads "-Name" or "/Name" as a switch of its own, and the launcher quotes
// nothing but a value with a plain space. So no value takes any white space
// of any host but the plain space, in any place it can be set, and none
// takes a character that is not printable text either: what the engine
// makes of a format character or an unassigned code point is not the same
// everywhere, and such a character does not show.
func TestWhiteSpaceOfAnyHostIsRefused(t *testing.T) {
	_, m, _ := twoInstances(t)
	refused := func(what string, err error) {
		t.Helper()
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: accepted (%v)", what, err)
		}
	}
	for _, r := range slices.Concat(hostSpaces, notText) {
		for _, v := range []string{
			"Hi" + string(r) + "-AllowNonSteamClients",
			"Hi" + string(r) + "/LAN",
			"Hi" + string(r) + "there",
			"Hi the" + string(r) + "re all",
			string(r) + "Hithere",
			"Hithere" + string(r),
		} {
			for slot, s := range valueSlots(v) {
				_, _, err := m.Patch("file", PatchRequest{Settings: s})
				refused(fmt.Sprintf("%s with U+%04X (%q)", slot, r, v), err)
			}
			for field, req := range secretSlots(v) {
				_, _, err := m.SetSecrets("line", req)
				refused(fmt.Sprintf("%s on the command line with U+%04X (%q)", field, r, v), err)
			}
		}
		// At a create too.
		_, err := m.Create(CreateRequest{Name: "more", Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr("Hi" + string(r) + "-LAN")}})
		refused(fmt.Sprintf("create with U+%04X in the display name", r), err)
	}
	untouched(t, m)
	if len(m.List()) != 2 {
		t.Fatal("a refused instance was created")
	}

	// The plain space, and text in any script, stay what they were.
	for _, v := range []string{"Hi there", "Caf\u00e9 Z\u00fcrich", "\u65e5\u672c\u8a9e \u30b5\u30fc\u30d0\u30fc", "\u041f\u0440\u0438\u0432\u0435\u0442",
		"e\u0301t\u00e9", "Go \U0001F642 on", "A \u2014 B", "\u00bfQu\u00e9?", "50 \u20ac"} {
		for slot, s := range map[string]Settings{
			"rule value":           {Rules: &map[string]string{"Banner": v}, ExtraArgs: &[]string{}},
			"MOTD":                 {Rules: &map[string]string{"MOTD": v}, ExtraArgs: &[]string{}},
			"extra argument value": {Rules: &map[string]string{}, ExtraArgs: &[]string{"-Banner=" + v}},
			"display name":         {DisplayName: ptr(v)},
		} {
			if _, _, err := m.Patch("file", PatchRequest{Settings: s}); err != nil {
				t.Errorf("%s %q: %v", slot, v, err)
			}
		}
		if _, _, err := m.SetSecrets("line", SecretsRequest{AdminPassword: ptr("pw " + v + " 12345")}); err != nil {
			t.Errorf("password with %q on the command line: %v", v, err)
		}
	}
}

// An en dash anywhere on its command line stops the server's engine at
// start: it takes it for a "-" spoilt on its way through a mail program.
// So no value takes one.
func TestEnDashIsRefused(t *testing.T) {
	_, m, _ := twoInstances(t)
	for _, v := range []string{"Alpha \u2013 EU", "Alpha\u2013EU", "\u2013Alpha12", "Alpha123\u2013"} {
		for slot, s := range valueSlots(v) {
			if _, _, err := m.Patch("file", PatchRequest{Settings: s}); !fault.Is(err, fault.Invalid) {
				t.Errorf("%s %q: accepted (%v)", slot, v, err)
			}
		}
		for field, req := range secretSlots(v) {
			if _, _, err := m.SetSecrets("line", req); !fault.Is(err, fault.Invalid) {
				t.Errorf("%s %q on the command line: accepted (%v)", field, v, err)
			}
		}
	}
	untouched(t, m)
	_, _, err := m.Patch("file", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha \u2013 EU")}})
	if err == nil || !strings.Contains(err.Error(), "en dash") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// The engine searches its command line for some names as plain text: in
// the whole line or in each argument, inside quotes too, and mostly in the
// middle of a word. A name that ends in "=" is kept out by the rule that no
// value holds "="; the others ESM knows one by one (testkit/cmdline lists
// what the engine searches for), and refuses a value that holds one, in any
// case of its letters, wherever a value can be set. The refusal of a
// setting names the name; a secret's never does.
func TestNamesTheEngineSearchesForAreRefused(t *testing.T) {
	_, m, sup := twoInstances(t)
	worldRe := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	mapRe := regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	for _, name := range cmdline.Searched {
		for _, n := range []string{name, strings.ToUpper(name)} {
			for _, v := range []string{"Zq7 " + n + "5 Jx9", "Zq7 x" + n + "5 Jx9", "x" + n + "5"} {
				for slot, s := range valueSlots(v) {
					switch {
					case slot == "world" && !worldRe.MatchString(v), slot == "map" && !mapRe.MatchString(v):
						continue
					}
					_, _, err := m.Patch("file", PatchRequest{Settings: s})
					if !fault.Is(err, fault.Invalid) {
						t.Errorf("%s %q: accepted (%v)", slot, v, err)
						if err == nil {
							m.Start(context.Background(), "file", false)
							spec, _, _ := sup.last(t)
							t.Logf("  the server's engine finds %q on %q", cmdline.Found(spec.Args), spec.Args)
						}
						continue
					}
					// After a space, a name that starts with "-" breaks an
					// older rule first, whose refusal has no name to give.
					spaceDash := strings.HasPrefix(name, "-") && strings.HasPrefix(v, "Zq7 -")
					if !strings.Contains(name, "=") && !spaceDash && fault.CodeOf(err) == "unsafe_argument" &&
						!strings.Contains(strings.ToLower(err.Error()), strings.TrimSuffix(strings.TrimSuffix(name, "2"), "3")) {
						t.Errorf("%s %q: the refusal does not name the name: %v", slot, v, err)
					}
				}
				for field, req := range secretSlots("Zq7x" + n + "5Jx9") {
					_, _, err := m.SetSecrets("line", req)
					if !fault.Is(err, fault.Invalid) {
						t.Errorf("%s with %q on the command line: accepted (%v)", field, n, err)
					} else if strings.Contains(strings.ToLower(err.Error()), name) {
						t.Errorf("%s: the refusal names what the secret holds: %v", field, err)
					}
				}
			}
		}
	}
	untouched(t, m)

	// The instance's name is in the folders ESM puts on the command line.
	for _, name := range []string{"x-ansimalloc", "a-stompmalloc-b", "my-binnedmalloc2", "xsteamsocketsrelays1", "eu-tracehost"} {
		_, err := m.Create(CreateRequest{Name: name, Settings: Settings{BindIP: ptr("127.0.0.1"), DisplayName: ptr("Alpha"), World: ptr("alpha")}})
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("instance %s: created (%v)", name, err)
		}
	}
	// A rule key or an extra argument that holds the name of such a switch
	// would be read as that switch; one that is the switch itself is what
	// it says.
	for what, s := range map[string]Settings{
		"rule key ansimallocx":         {Rules: &map[string]string{"ansimallocx": "1"}},
		"rule key StompMallocNow":      {Rules: &map[string]string{"StompMallocNow": "1"}},
		"extra argument -binnedmalloc": {ExtraArgs: &[]string{"-binnedmalloc9"}},
		"extra argument -TraceHostile": {ExtraArgs: &[]string{"-TraceHostile=1"}},
		"rule key xSteamSocketsRelays": {Rules: &map[string]string{"xSteamSocketsRelays": "1"}},
	} {
		if _, _, err := m.Patch("file", PatchRequest{Settings: s}); !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: accepted (%v)", what, err)
		}
	}
	if len(m.List()) != 2 {
		t.Fatalf("a refused instance was created: %d instances", len(m.List()))
	}
	untouched(t, m)
	if _, _, err := m.Patch("file", PatchRequest{Settings: Settings{ExtraArgs: &[]string{"-ansimalloc", "-vmapoolscale=2", "-BinnedMalloc2"},
		Rules: &map[string]string{"Mode": "simple", "Banner": "well-met friends, type /help", "MOTD": "Malloc and mimic"}}}); err != nil {
		t.Errorf("the engine's own switches as extra arguments, and plain words: %v", err)
	}
}

// -CmdLineFile= makes the engine put the content of a file where the
// switch stands on its command line: switches ESM never sees. It is refused
// as a rule and as an extra argument, in any case.
func TestCommandLineFileSwitchIsRefused(t *testing.T) {
	_, m, _ := twoInstances(t)
	for what, s := range map[string]Settings{
		"rule":                 {Rules: &map[string]string{"CmdLineFile": "more.txt"}},
		"rule in another case": {Rules: &map[string]string{"cmdlinefile": "more.txt"}},
		"extra argument":       {ExtraArgs: &[]string{"-CmdLineFile=more.txt"}},
		"extra argument upper": {ExtraArgs: &[]string{"-CMDLINEFILE=more.txt"}},
		"extra argument bare":  {ExtraArgs: &[]string{"-CmdLineFile"}},
		"in a value":           {Rules: &map[string]string{"MOTD": "see a-CmdLineFile=more.txt now"}},
		"in a value, unspaced": {Rules: &map[string]string{"Banner": "a;-CmdLineFile"}},
	} {
		_, _, err := m.Patch("file", PatchRequest{Settings: s})
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: accepted (%v)", what, err)
		}
		_, err = m.Create(CreateRequest{Name: "more", Settings: func() Settings { s.BindIP = ptr("127.0.0.1"); return s }()})
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("%s at a create: accepted (%v)", what, err)
		}
	}
	if why, refused := gameserver.RefusedSwitch("-CmdLineFile"); !refused || !strings.Contains(why, "is refused") {
		t.Errorf("RefusedSwitch: %q %v", why, refused)
	}
	untouched(t, m)
}

// What an earlier version stored and the rules now refuse keeps loading
// and is passed as it is stored, with the warning unsafe_stored_settings
// naming it: an instance whose name holds a name the engine searches for
// (it is in the folders on the command line), a map and a value that hold
// one, a value with white space of a Windows host that Unicode does not
// list or with "=", a switch that would be read as another, and the
// switch that makes the engine read a file. The warning goes when they are
// changed; the name stays, it cannot be.
func TestStoredSettingsTheNewRulesRefuse(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	const nbName = "Hi" + string(rune(0x180e)) + "there"
	old := in
	old.Name, old.DisplayName, old.World = "x-ansimalloc", nbName, "w-stompmalloc"
	old.Map = "Game/x-binnedmalloc"
	old.Rules = map[string]string{"Banner": "well x-tracehost met", "Motto": "a b=c", "ansimallocx": "1", "Note": "fine words", "ansimalloc": "1"}
	old.ExtraArgs = []string{"-TraceHosts=1", "-CmdLineFile=more.txt", "-vmapoolscale=2", "-NoSteam"}
	old.Ports = Ports{Game: in.Ports.Game + 2, Query: in.Ports.Query + 2}
	for _, d := range []string{dataDir(old.Name), instDir(old.Name) + "/run"} {
		if err := r.st.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.st.WriteJSON(instFile(old.Name), old, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()

	v, err := m.Get(old.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"name", "display_name", "world", "map", "rules.Banner", "rules.Motto", "rules.ansimallocx", "extra_args.1", "extra_args.2"}
	if !slices.Contains(v.Warnings, WarnUnsafeStoredSettings) || !slices.Equal(storedUnsafe(t, v), want) {
		t.Fatalf("warnings %q, settings named %q, want %q", v.Warnings, storedUnsafe(t, v), want)
	}
	if text := WarningWords(WarnUnsafeStoredSettings, v.UnsafeStoredSettings); !strings.Contains(text, "name of an instance") {
		t.Errorf("the warning does not say what to do about a name: %q", text)
	}
	if v, _ := m.Get("alpha"); len(storedUnsafe(t, v)) != 0 {
		t.Errorf("alpha: %q", storedUnsafe(t, v))
	}

	// Passed as stored.
	m.Start(context.Background(), old.Name, false)
	spec, _, _ := sup.last(t)
	for _, kept := range []string{"Game/x-binnedmalloc", "-ServerName=" + nbName, "-WorldName=w-stompmalloc", "-Banner=well x-tracehost met",
		"-Motto=a b=c", "-ansimallocx=1", "-ansimalloc=1", "-Note=fine words", "-TraceHosts=1", "-CmdLineFile=more.txt", "-vmapoolscale=2", "-NoSteam"} {
		if !slices.Contains(spec.Args, kept) {
			t.Errorf("%q is not passed as stored: %q", kept, spec.Args)
		}
	}
	if !strings.Contains(r.log.String(), "rules.Banner") {
		t.Errorf("the service log does not name the settings:\n%s", r.log.String())
	}

	// A change must leave out the switch ESM refuses; then the stored
	// values sent back as they are do not stand in the way of it.
	if _, _, err := m.Patch(old.Name, PatchRequest{Settings: Settings{Autostart: ptr(true)}}); !fault.Is(err, fault.Invalid) {
		t.Errorf("a change with the stored -CmdLineFile= left in: %v", err)
	}
	args := []string{"-TraceHosts=1", "-vmapoolscale=2", "-NoSteam"}
	if _, _, err := m.Patch(old.Name, PatchRequest{Settings: Settings{Autostart: ptr(true), ExtraArgs: &args}}); err != nil {
		t.Fatalf("the stored values sent back without -CmdLineFile=: %v", err)
	}
	// Changed, a stored value is checked like a new one.
	for what, s := range map[string]Settings{
		"display name":   {DisplayName: ptr(nbName + "2")},
		"world":          {World: ptr("w-stompmalloc2")},
		"map":            {Map: ptr("Game/x-binnedmalloc2")},
		"rule value":     {Rules: &map[string]string{"Banner": "well x-tracehost met again", "Motto": "a b=c", "ansimallocx": "1", "Note": "fine words", "ansimalloc": "1"}},
		"rule key":       {Rules: &map[string]string{"Banner": "well x-tracehost met", "Motto": "a b=c", "ansimallocx": "2", "Note": "fine words", "ansimalloc": "1"}},
		"extra argument": {ExtraArgs: &[]string{"-TraceHosts=2", "-vmapoolscale=2", "-NoSteam"}},
	} {
		if _, _, err := m.Patch(old.Name, PatchRequest{Settings: s}); !fault.Is(err, fault.Invalid) {
			t.Errorf("%s changed to another value the rules refuse: %v", what, err)
		}
	}
	fixed, _, err := m.Patch(old.Name, PatchRequest{Settings: Settings{DisplayName: ptr("Hi there"), World: ptr("w2"), Map: ptr("Game/Main"),
		Rules: &map[string]string{"Banner": "well met", "Note": "fine words", "ansimalloc": "1"}, ExtraArgs: &[]string{"-vmapoolscale=2", "-NoSteam"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := storedUnsafe(t, fixed); !slices.Equal(got, []string{"name"}) || !slices.Contains(fixed.Warnings, WarnUnsafeStoredSettings) {
		t.Errorf("after the fix: settings named %q, warnings %q", got, fixed.Warnings)
	}
}
