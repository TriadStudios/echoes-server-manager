package instances

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// switchWant is a switch with a value that ESM means to pass, and the value
// it shows for it.
type switchWant struct{ name, value string }

// lowerASCII lowers the ASCII capitals of s, and nothing else: the server
// compares switch names that way.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// meant lists the switches with a value that ESM means to pass for in,
// with sec as its secrets: its own, the rules and the extra arguments that
// have a value, and the secrets file, or with command-line delivery the
// secrets that are set.
func meant(r *rig, in Instance, sec Secrets) []switchWant {
	data, _ := r.st.Abs(dataDir(in.Name))
	log, _ := r.st.Abs(serverLog(in.Name))
	file, _ := r.st.Abs(runSecretsFile(in.Name))
	display := in.DisplayName
	if display == "" {
		display = in.Name
	}
	listed := "False"
	if in.Listed {
		listed = "True"
	}
	out := []switchWant{{"Port", strconv.Itoa(in.Ports.Game)}, {"QueryPort", strconv.Itoa(in.Ports.Query)},
		{"ServerName", display}, {"WorldName", in.World}, {"MaxPlayers", strconv.Itoa(in.MaxPlayers)},
		{"ListInBrowser", listed}, {"UserDir", data}, {"abslog", log}}
	for k, v := range in.Rules {
		if s, ok := gameserver.LookupGameSetting(k); ok && s.Type == gameserver.TypeList {
			v = gameserver.ListValue(v)
		}
		if v != "" {
			out = append(out, switchWant{k, v})
		}
	}
	for _, a := range in.ExtraArgs {
		if n, v, ok := extraSwitch(a); ok {
			out = append(out, switchWant{n, v})
		}
	}
	if in.SecretDelivery != gameserver.DeliveryCommandLine {
		return append(out, switchWant{"SecretsFile", file})
	}
	for _, s := range []switchWant{{"Password", sec.ServerPassword}, {"AdminPassword", sec.AdminPassword},
		{"AnnouncementsToken", sec.AnnouncementsToken}, {"ProblemReportToken", sec.ProblemReportToken}} {
		if s.value != "" {
			out = append(out, s)
		}
	}
	return out
}

// stray returns the names the engine searches its command line for as
// plain text (cmdline.Searched) that stand in an argument of argv without
// being meant. An argument may be one of those switches itself, because a
// rule or an extra argument names it: it starts with the name, after its
// "-" when the name has none, and its own name ends where that name does
// (at the end of the argument or at its "="). That part ESM shows as what
// it is; a name anywhere after it is not meant.
func stray(argv []string) []string {
	var out []string
	for _, a := range argv {
		rest := a
		for _, s := range cmdline.Searched {
			head := a
			if !strings.HasPrefix(s, "-") {
				head = strings.TrimPrefix(a, "-")
			}
			if len(head) < len(s) || !cmdline.SameName(head[:len(s)], s) {
				continue
			}
			if after := head[len(s):]; (strings.Contains(s, "=") || after == "" || after[0] == '=') && len(after) < len(rest) {
				rest = after
			}
		}
		out = append(out, cmdline.Found([]string{rest})...)
	}
	return out
}

// serverReads reads argv, the command line ESM built for in with the
// secrets sec, the way the server does, and says where that differs from
// what ESM shows: outside quotes the only switches with a value are the
// ones ESM means to pass, each once and each read with the whole value ESM
// shows; no switch of the game that is on or off is set unless a rule or
// an extra argument sets it; every "=" on the line, in quotes or not, is
// the one ESM wrote after the name of a switch; no name that the engine
// searches the line for as plain text is on it, unless a rule or an extra
// argument is that switch; and the engine starts with the line at all.
func serverReads(r *rig, in Instance, sec Secrets, argv []string) []string {
	line := cmdline.Join(argv)
	want := meant(r, in, sec)
	var problems, wantNames, gotNames []string
	if n := strings.Count(line, "="); n != len(want) {
		problems = append(problems, fmt.Sprintf("the line has %d \"=\", ESM passes %d switches with a value", n, len(want)))
	}
	for _, name := range stray(argv) {
		problems = append(problems, fmt.Sprintf("the server's engine finds %q on the line, which no rule or extra argument is", name))
	}
	if why := cmdline.Refuses(line); why != "" {
		problems = append(problems, "the server's engine does not start: "+why)
	}
	for _, w := range want {
		wantNames = append(wantNames, lowerASCII(w.name))
	}
	for _, s := range cmdline.Switches(line) {
		gotNames = append(gotNames, lowerASCII(s.Name))
	}
	slices.Sort(wantNames)
	slices.Sort(gotNames)
	if !slices.Equal(wantNames, gotNames) {
		problems = append(problems, fmt.Sprintf("the server finds the switches %q, ESM passes %q", gotNames, wantNames))
	}
	for _, w := range want {
		if got, ok := cmdline.Value(line, w.name); !ok || got != w.value {
			problems = append(problems, fmt.Sprintf("the server reads %s as %q (found: %v), ESM shows %q", w.name, got, ok, w.value))
		}
	}
	for _, s := range gameserver.GameSettings() {
		if s.Type != gameserver.TypeBool || in.Rules[s.Key] != "" || slices.ContainsFunc(in.ExtraArgs, func(a string) bool {
			n, _, _ := extraSwitch(a)
			return cmdline.SameName(n, s.Key)
		}) {
			continue
		}
		if on, set := cmdline.Bool(line, s.Key); set {
			problems = append(problems, fmt.Sprintf("the server reads %s as %v, which nothing ESM shows sets", s.Key, on))
		}
	}
	return problems
}

// What ESM accepts reaches the server as ESM shows it. Settings are set
// one at a time from random pieces (the same pieces on every run), the way
// an operator would set them; after each change ESM accepts, the command
// line it builds is read the way the server reads it (testkit/cmdline)
// and must hold exactly the switches ESM shows, with their whole values,
// for either way of passing the secrets, and for values with and without
// a space.
func TestAcceptedSettingsReachTheServerAsShown(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	ctx := context.Background()
	const name = "alpha"
	if _, err := m.Create(CreateRequest{Name: name, Settings: Settings{BindIP: ptr("127.0.0.1")}}); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(20261113, 14))
	pieces := []string{"a", "Z", "q7", "9", "=", "=", ",", ")", "(", ";", " ", " ", " ", "-", "_", ".", ":", "/", "?", "#", `"`,
		"\t", "\n", "\u00a0", "\u212A", "\u0130", "é", "PvP", "LAN", "Password", "AdminPassword", "LOG", "Port", "ini", "x"}
	// The white space of every host of the engine, characters that are not
	// printable text, an en dash, and the names the engine searches its
	// line for.
	pieces = append(pieces, "-", "+", "5", "PLUGIN", string(rune(0x2013)))
	for _, r := range slices.Concat(hostSpaces, notText) {
		pieces = append(pieces, string(r))
	}
	pieces = append(pieces, cmdline.Searched...)
	// Half of the values come from fewer pieces, which ESM accepts more
	// often: a value with a space and "," or ")" in it, for one.
	fewer := []string{"a", "Z", "q7", " ", ",", ")", ";", "-", "/", "PvP", "Password"}
	gen := func(lo, hi int) string {
		from := pieces
		if rng.IntN(2) == 0 {
			from = fewer
		}
		var b strings.Builder
		for n := lo + rng.IntN(hi-lo+1); n > 0; n-- {
			b.WriteString(from[rng.IntN(len(from))])
		}
		// Now and then a name the engine searches for, in the middle.
		if rng.IntN(12) == 0 {
			name := cmdline.Searched[rng.IntN(len(cmdline.Searched))]
			if rng.IntN(2) == 0 {
				name = strings.ToUpper(name)
			}
			s := b.String()
			at := rng.IntN(len(s) + 1)
			for at < len(s) && !utf8.RuneStart(s[at]) {
				at++
			}
			return s[:at] + name + s[at:]
		}
		return b.String()
	}
	keys := []string{"Banner", "banner", "BANNER", "Mode", "Motto", "Tag", "PvP", "pvp", "LAN", "x_PvP", "Foo.LAN", "Foo-Port", "Foo:Port",
		"A1", "LOG", "Rate2", "ansimalloc", "AnsiMallocs", "vmapoolscale", "binnedmalloc2", "binnedmalloc22", "PLUGIN", "CmdLineFile",
		"SteamSocketsRelays", "xSteamSocketsRelays", "tracehost", "TraceHosts"}
	key := func() string {
		if rng.IntN(3) == 0 {
			return gen(1, 3)
		}
		return keys[rng.IntN(len(keys))]
	}
	catalog := []string{"MOTD", "AdminPlayerIds", "ChatBlockedWords", "AnnouncementsUrl"}
	var sec Secrets
	reset := func() {
		t.Helper()
		if _, _, err := m.Patch(name, PatchRequest{Settings: Settings{Rules: &map[string]string{}, ExtraArgs: &[]string{},
			DisplayName: ptr(name), Map: ptr(""), SecretDelivery: ptr(gameserver.DeliveryFile)}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.SetSecrets(name, SecretsRequest{ServerPassword: ptr(""), AdminPassword: ptr(""),
			AnnouncementsToken: ptr(""), ProblemReportToken: ptr("")}); err != nil {
			t.Fatal(err)
		}
		sec = Secrets{}
	}
	accepted := map[string]int{}
	failures := 0
	for i := 0; i < 2500; i++ {
		v, _ := m.Get(name)
		rules, args := maps.Clone(v.Rules), slices.Clone(v.ExtraArgs)
		if len(rules) > 6 {
			rules = map[string]string{}
		}
		if len(args) > 4 {
			args = []string{}
		}
		change := Settings{Rules: &rules, ExtraArgs: &args}
		var what, slot, value string
		var err error
		next := sec
		switch rng.IntN(8) {
		case 0:
			k := key()
			value = gen(1, 6)
			rules[k] = value
			slot, what = "rule", fmt.Sprintf("rule %q = %q", k, value)
		case 1:
			k := catalog[rng.IntN(len(catalog))]
			value = gen(1, 6)
			if k == "AnnouncementsUrl" {
				value = "https://example.com/" + value
			}
			rules[k] = value
			slot, what = "game setting", fmt.Sprintf("game setting %s = %q", k, value)
		case 2:
			var a string
			switch rng.IntN(5) {
			case 0:
				a = "-" + key()
			case 1:
				a = "-" + gen(1, 4)
			default:
				value = gen(1, 5)
				a = "-" + key() + "=" + value
			}
			args = append(args, a)
			slot, what = "extra argument", fmt.Sprintf("extra argument %q", a)
		case 3:
			value = gen(1, 6)
			change.DisplayName = &value
			slot, what = "display name", fmt.Sprintf("display name %q", value)
		case 4:
			mp := []string{"/" + key(), "/Game/Maps/" + key(), gen(1, 3), "Game/" + gen(1, 2)}[rng.IntN(4)]
			change.Map = &mp
			slot, what = "map", fmt.Sprintf("map %q", mp)
		case 5, 6:
			value = gen(3, 9)
			var req SecretsRequest
			switch rng.IntN(4) {
			case 0:
				req.ServerPassword, next.ServerPassword = &value, value
			case 1:
				req.AdminPassword, next.AdminPassword = &value, value
			case 2:
				req.AnnouncementsToken, next.AnnouncementsToken = &value, value
			default:
				req.ProblemReportToken, next.ProblemReportToken = &value, value
			}
			slot, what = "secret ("+v.SecretDelivery+")", fmt.Sprintf("secret %q with secret_delivery %s", value, v.SecretDelivery)
			_, _, err = m.SetSecrets(name, req)
		case 7:
			to := gameserver.DeliveryCommandLine
			if v.SecretDelivery == to {
				to = gameserver.DeliveryFile
			}
			// Half of the time the secrets are cleared first: the
			// passwords of file delivery often cannot go on the command
			// line, and then neither can the instance.
			if to == gameserver.DeliveryCommandLine && rng.IntN(2) == 0 {
				if _, _, err := m.SetSecrets(name, SecretsRequest{ServerPassword: ptr(""), AdminPassword: ptr(""),
					AnnouncementsToken: ptr(""), ProblemReportToken: ptr("")}); err != nil {
					t.Fatal(err)
				}
				sec, next = Secrets{}, Secrets{}
			}
			change.SecretDelivery = &to
			slot, what = "secret_delivery "+to, "secret_delivery "+to
		}
		if !strings.HasPrefix(slot, "secret (") {
			_, _, err = m.Patch(name, PatchRequest{Settings: change})
		}
		if err != nil {
			if !fault.Is(err, fault.Invalid) && !fault.Is(err, fault.Conflict) {
				t.Fatalf("%s: %v", what, err)
			}
			continue
		}
		sec = next
		accepted[slot]++
		if value != "" {
			if strings.Contains(value, " ") {
				accepted[slot+" with a space"]++
			} else {
				accepted[slot+" without a space"]++
			}
		}
		m.Start(ctx, name, false)
		spec, _, _ := sup.last(t)
		got, _ := m.Get(name)
		problems := serverReads(r, got.Instance, sec, spec.Args)
		for _, p := range problems {
			t.Errorf("after the change %d, %s: %s", i, what, p)
		}
		if len(problems) > 0 {
			t.Logf("  the command line: %q", spec.Args)
			if failures++; failures >= 25 {
				t.FailNow()
			}
			reset()
		}
	}
	t.Logf("accepted: %v", accepted)
	for _, slot := range []string{"rule with a space", "rule without a space", "game setting with a space", "game setting without a space",
		"extra argument with a space", "extra argument without a space", "display name with a space", "display name without a space",
		"map", "secret (file) with a space", "secret (file) without a space", "secret (command_line) with a space",
		"secret (command_line) without a space", "secret_delivery command_line", "secret_delivery file"} {
		if accepted[slot] == 0 {
			t.Errorf("no change of the kind %q was accepted: the test does not cover it", slot)
		}
	}
}

// The strings that made the server read something else than ESM shows, in
// every place ESM takes them, are refused when they are set. A secret
// passed in a file may be anything, as before; an instance holding one that
// cannot go on the command line is not switched to the command line, and
// the refusal names the secret, never its value. The one exception is a
// list setting, where "abc,def123" is two items: ESM passes it as
// "abc, def123", quoted whole, and the server reads both.
func TestAttackStringsAreRefused(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	for _, n := range []string{"file", "line"} {
		s := Settings{BindIP: ptr("127.0.0.1")}
		if n == "line" {
			s.SecretDelivery = ptr(gameserver.DeliveryCommandLine)
		}
		if _, err := m.Create(CreateRequest{Name: n, Settings: s}); err != nil {
			t.Fatal(err)
		}
	}
	attacks := []string{"x;LOG=", "Hi=", "\u212AAdminPassword=evil", "\u0130LAN=1", "secret,AdminPassword=evil", "secret;AdminPassword=",
		"my\"pass99", "abc,def123", "pass)word99", "adm;Password=joinme", "https://e.com/n?AdminPassword=x", "-iniFile=/x", "Hi;LOG=x.log",
		// Not in the list of strings that slipped through: a bare switch after
		// white space the server does not quote, if its engine counts it as
		// white space.
		"x\u00a0/PvP", "x\u2028/LAN",
		// White space of a Windows host that Unicode does not list, a format
		// character, names the engine searches for inside a quoted value,
		// its plugin folder switch, an en dash, and a plain "=" in a value
		// with a space.
		"Hi\u180e-AllowNonSteamClients", "Hi\u180e/LAN", "Hi\u200b-PvP", "well x-sigdfl=5 met", "x-vmapoolscale=4 y", "a x-stompmalloc b",
		"see PLUGIN=/tmp/x now", "Alpha \u2013 EU", "a b=c", "My-AnsiMalloc Server", "a-CmdLineFile=x.txt b",
		// A space followed by "-" and more, and "-" with a letter at the
		// start of a word, with nothing else wrong.
		"Zq7Jx -Kv9 pass", "Zq7Jx;-Kv9Wy"}
	refused := func(what string, err error) {
		t.Helper()
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: accepted (%v)", what, err)
		}
	}
	for _, a := range attacks {
		for what, s := range map[string]Settings{
			"rule value":            {Rules: &map[string]string{"Banner": a}},
			"MOTD":                  {Rules: &map[string]string{"MOTD": a}},
			"AdminPlayerIds":        {Rules: &map[string]string{"AdminPlayerIds": a}},
			"ChatBlockedWords":      {Rules: &map[string]string{"ChatBlockedWords": a}},
			"AnnouncementsUrl":      {Rules: &map[string]string{"AnnouncementsUrl": a}},
			"extra argument value":  {ExtraArgs: &[]string{"-Banner=" + a}},
			"extra argument":        {ExtraArgs: &[]string{a}},
			"display name":          {DisplayName: ptr(a)},
			"world":                 {World: ptr(a)},
			"rule value, with more": {DisplayName: ptr("Alpha"), Rules: &map[string]string{"Mode": a}},
		} {
			_, _, err := m.Patch("file", PatchRequest{Settings: s})
			if g, list := gameserver.LookupGameSetting(what); err == nil && list && g.Type == gameserver.TypeList {
				if a != "abc,def123" {
					t.Errorf("%s %q: accepted", what, a)
				}
				m.Start(context.Background(), "file", false)
				spec, _, _ := sup.last(t)
				v, _ := m.Get("file")
				line := cmdline.Join(spec.Args)
				if got, _ := cmdline.Value(line, what); got != gameserver.ListValue(a) || len(serverReads(r, v.Instance, Secrets{}, spec.Args)) > 0 {
					t.Errorf("the list %s %q reaches the server as %q: %q", what, a, got, serverReads(r, v.Instance, Secrets{}, spec.Args))
				}
				if _, _, err := m.Patch("file", PatchRequest{Settings: Settings{Rules: &map[string]string{}}}); err != nil {
					t.Fatal(err)
				}
				continue
			}
			refused(fmt.Sprintf("%s %q", what, a), err)
		}
		for field, req := range map[string]SecretsRequest{
			"server_password":      {ServerPassword: ptr(a)},
			"admin_password":       {AdminPassword: ptr(a)},
			"announcements_token":  {AnnouncementsToken: ptr(a)},
			"problem_report_token": {ProblemReportToken: ptr(a)},
		} {
			_, _, err := m.SetSecrets("line", req)
			refused(fmt.Sprintf("%s %q on the command line", field, a), err)
			if err != nil && strings.Contains(err.Error(), a) {
				t.Errorf("%s: the refusal repeats the value: %v", field, err)
			}
			if _, _, err := m.SetSecrets("file", req); err != nil {
				continue
			}
			_, _, err = m.Patch("file", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}})
			refused(fmt.Sprintf("secret_delivery command_line with the %s %q", field, a), err)
			if err != nil && (strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), field)) {
				t.Errorf("%s: the refusal repeats the value or does not name the secret: %v", field, err)
			}
			if err == nil {
				if _, _, err := m.Patch("file", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryFile)}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := m.SetSecrets("file", SecretsRequest{ServerPassword: ptr(""), AdminPassword: ptr(""),
				AnnouncementsToken: ptr(""), ProblemReportToken: ptr("")}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for what, s := range map[string]Settings{
		"rule key x_PvP":            {Rules: &map[string]string{"x_PvP": "True"}},
		"extra argument -x_PvP":     {ExtraArgs: &[]string{"-x_PvP=True"}},
		"extra argument -iniFile=":  {ExtraArgs: &[]string{"-iniFile=/x"}},
		"extra argument -Foo:x=1":   {ExtraArgs: &[]string{"-Foo:LAN=1"}},
		"extra argument --PvP=True": {ExtraArgs: &[]string{"--PvP=True"}},
	} {
		_, _, err := m.Patch("file", PatchRequest{Settings: s})
		refused(what, err)
	}
	for _, n := range []string{"file", "line"} {
		v, _ := m.Get(n)
		if len(v.Rules) != 0 || len(v.ExtraArgs) != 0 || v.DisplayName != n || v.Secrets.any() {
			t.Errorf("%s after the refusals: %+v", n, v.Instance)
		}
	}
}
