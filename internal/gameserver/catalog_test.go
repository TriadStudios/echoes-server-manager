package gameserver

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The game settings ESM knows by name: every switch the server takes on
// its command line that ESM does not set itself.
var wantSettings = []string{
	"MOTD", "LAN", "AllowNonSteamClients", "AdminPlayerIds", "AdminLoginMaxAttempts", "AdminLoginLockoutSeconds",
	"BossSummonCooldownSeconds", "LastWipe",
	"HarvestRate", "XPRate", "TamingRate", "TamingBiteRate", "HatchRate", "MatureRate", "MatingRate", "CareRate",
	"ImprintRate", "DayLengthMinutes", "StructureDecay", "StructureDecayMultiplier", "PvP", "PvPStructureDamage",
	"OfflineRaidProtection", "ORPActivationMinutes", "ORPStructureDamageTaken", "ORPTameDamageTaken",
	"ORPTurretDamageMultiplier", "Difficulty", "DeathDropMode",
	"ChatLog", "ChatFilter", "ChatBlockedWords",
	"AnnouncementsUrl", "AnnouncementsPollSeconds", "AnnouncementsToken", "ProblemReportUrl", "ProblemReportToken",
}

func TestCatalogHasEveryGameSetting(t *testing.T) {
	var got []string
	for _, s := range GameSettings() {
		got = append(got, s.Key)
	}
	if !slices.Equal(got, wantSettings) {
		t.Fatalf("catalog keys:\n got %v\nwant %v", got, wantSettings)
	}
}

// Every entry is complete: a known group and type, a label, help text, a
// default that the entry itself accepts, and a range that holds it.
func TestCatalogEntriesAreComplete(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range GameSettingGroups() {
		if g.ID == "" || g.Title == "" || groups[g.ID] {
			t.Errorf("group %+v: no id or title, or listed twice", g)
		}
		groups[g.ID] = true
	}
	for _, id := range []string{GroupAccess, GroupWorld, GroupChat, GroupReports} {
		if !groups[id] {
			t.Errorf("group %s is missing", id)
		}
	}
	keyRe := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	seen := map[string]bool{}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, s := range GameSettings() {
		what := s.Key
		if !keyRe.MatchString(s.Key) || seen[strings.ToLower(s.Key)] {
			t.Errorf("%s: not a plain name, or listed twice", what)
		}
		seen[strings.ToLower(s.Key)] = true
		if !groups[s.Group] {
			t.Errorf("%s: unknown group %q", what, s.Group)
		}
		if strings.TrimSpace(s.Label) == "" || len(strings.TrimSpace(s.Help)) < 10 || !strings.HasSuffix(s.Help, ".") {
			t.Errorf("%s: no label, or no help sentence: %q", what, s.Help)
		}
		switch s.Type {
		case TypeBool, TypeInt, TypeFloat, TypeString, TypeEnum, TypeList, TypeDate:
		default:
			t.Errorf("%s: unknown type %q", what, s.Type)
		}
		if s.Secret {
			if s.Type != TypeString || s.Default != "" || s.SecretField == "" {
				t.Errorf("%s: a secret is a string without a default, with the field it is set by", what)
			}
			if _, ok := SecretLike(s.Key); !ok {
				t.Errorf("%s: a secret whose name does not look like one would be accepted as a rule", what)
			}
			continue
		}
		if why, ok := SecretLike(s.Key); ok {
			t.Errorf("%s: looks like a secret (%s), so it could never be set as a rule", what, why)
		}
		if IsReservedSwitch(s.Key) {
			t.Errorf("%s: ESM sets this switch itself", what)
		}
		if _, refused := RefusedSwitch(s.Key); refused {
			t.Errorf("%s: a refused switch", what)
		}
		if s.Default != "" {
			if err := s.Check(s.Default, now); err != nil {
				t.Errorf("%s: its own default %q is refused: %v", what, s.Default, err)
			}
		}
		switch s.Type {
		case TypeBool:
			if s.Default != "True" && s.Default != "False" {
				t.Errorf("%s: bool default %q", what, s.Default)
			}
		case TypeInt, TypeFloat:
			if s.Min == nil || s.Default == "" {
				t.Errorf("%s: a number needs a minimum and a default", what)
			}
			if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
				t.Errorf("%s: minimum above maximum", what)
			}
		case TypeEnum:
			if len(s.Choices) < 2 || s.Default == "" {
				t.Errorf("%s: an enum needs choices and a default", what)
			}
			for _, c := range s.Choices {
				if c.Value == "" || c.Label == "" {
					t.Errorf("%s: choice %+v", what, c)
				}
			}
		case TypeString, TypeList:
			if s.MaxLength <= 0 {
				t.Errorf("%s: no maximum length", what)
			}
		}
		if (s.Group == GroupWorld) != s.NewWorldOnly {
			t.Errorf("%s: new_world_only must be set exactly for the world rules", what)
		}
	}
	if s, ok := LookupGameSetting("LastWipe"); !ok || s.NewWorldOnly || s.Group == GroupWorld {
		t.Error("LastWipe is read at every start: not a world rule")
	}
	if s, ok := LookupGameSetting("AllowNonSteamClients"); !ok || !s.Caution {
		t.Error("AllowNonSteamClients carries a caution")
	}
	if _, ok := LookupGameSetting("harvestrate"); ok {
		t.Error("lookup is by the exact name")
	}
	if s, ok := FoldGameSetting("harvestrate"); !ok || s.Key != "HarvestRate" {
		t.Error("the case-blind lookup finds HarvestRate")
	}
	if _, ok := LookupGameSetting("FakeReadyDelay"); ok {
		t.Error("a key outside the catalog is found")
	}
}

func TestGameSettingCheck(t *testing.T) {
	now := time.Date(2026, 11, 13, 23, 30, 0, 0, time.UTC)
	ok := map[string][]string{
		"PvP":                       {"True", "False", "true", "FALSE", "yes", "No", "1", "0"},
		"AdminLoginLockoutSeconds":  {"0", "60", "3600"},
		"AdminLoginMaxAttempts":     {"0", "5", "2147483647"},
		"HarvestRate":               {"0.1", "1", "1.5", "1000", ".5", "999.999"},
		"DayLengthMinutes":          {"5", "240", "60"},
		"ORPTameDamageTaken":        {"0", "1", "0.25"},
		"AnnouncementsPollSeconds":  {"10", "30", "86400"},
		"Difficulty":                {"0", "1", "2"},
		"ChatFilter":                {"Off", "Mask", "Drop", "mask"},
		"DeathDropMode":             {"Everything", "InventoryOnly", "Nothing", "nothing"},
		"LastWipe":                  {"2026-11-13", "2024-02-29", "2000-01-01"},
		"ChatBlockedWords":          {"rude", "rude,ruder", "rude, ruder , rudest", "a,,b", strings.Repeat("abcd,", 400)[:1999]},
		"AdminPlayerIds":            {"76561190000000001", "76561190000000001,76561190000000002", "net:76561190000000001"},
		"MOTD":                      {"Welcome!", "Welcome, traveller", "Be nice (please)", "well-met friends", strings.Repeat("x", 256)},
		"AnnouncementsUrl":          {"https://example.com/api", "http://127.0.0.1:8080/x"},
		"ProblemReportUrl":          {"https://example.com/reports"},
		"BossSummonCooldownSeconds": {"0", "600"},
	}
	for key, values := range ok {
		s, found := LookupGameSetting(key)
		if !found {
			t.Fatalf("%s is not in the catalog", key)
		}
		for _, v := range values {
			if err := s.Check(v, now); err != nil {
				t.Errorf("%s=%q refused: %v", key, v, err)
			}
		}
	}
	bad := map[string][]string{
		"PvP":                      {"", "on", "off", "2", "-1", "truee", "True "},
		"AdminLoginLockoutSeconds": {"-1", "3601", "1.5", "", "60s", "1e2"},
		"AdminLoginMaxAttempts":    {"-1", "2147483648", "five"},
		"HarvestRate":              {"0.09", "1000.1", "0", "-1", "1e3", "abc", "", "1,5", "1.", "+1", "NaN", "Inf", "0x10"},
		"DayLengthMinutes":         {"4", "241"},
		"ORPTameDamageTaken":       {"1.01", "-0.1"},
		"AnnouncementsPollSeconds": {"9", "0"},
		"Difficulty":               {"3", "-1", "hard", "Normal", "1.0", ""},
		"ChatFilter":               {"Masked", "", "Off Mask"},
		"DeathDropMode":            {"All", "Some", ""},
		"LastWipe":                 {"", "1999-12-31", "0000-01-01", "2026-11-14", "2026-02-30", "13.11.2026", "2026-1-1", "2026-11-13T00:00:00Z", "yesterday"},
		"ChatBlockedWords":         {"", ",", " , ", "two words", "close)", `quote"`, strings.Repeat("abcd,", 401)[:2001]},
		"AdminPlayerIds":           {"", "one two"},
		"MOTD":                     {"", "Hi,all", "Hi, all", "Welcome:)", strings.Repeat("x", 257), "a\nb", `say "hi"`, "go -Port=1"},
		"AnnouncementsUrl":         {"", "example.com/api", "ftp://example.com/x", "https://", "https://example.com/a b", "https://example.com/a,b", "https://example.com/(x)"},
	}
	for key, values := range bad {
		s, _ := LookupGameSetting(key)
		for _, v := range values {
			err := s.Check(v, now)
			if err == nil {
				t.Errorf("%s=%q accepted", key, v)
				continue
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%q: the refusal does not name the setting: %v", key, v, err)
			}
		}
	}
	// A refusal says what is allowed.
	s, _ := LookupGameSetting("HarvestRate")
	if err := s.Check("5000", now); err == nil || !strings.Contains(err.Error(), "0.1") || !strings.Contains(err.Error(), "1000") {
		t.Errorf("HarvestRate=5000: %v", err)
	}
	s, _ = LookupGameSetting("ChatFilter")
	if err := s.Check("Shout", now); err == nil || !strings.Contains(err.Error(), "Off, Mask or Drop") {
		t.Errorf("ChatFilter=Shout: %v", err)
	}
	s, _ = LookupGameSetting("LastWipe")
	if err := s.Check("2026-11-14", now); err == nil || !strings.Contains(err.Error(), "future") {
		t.Errorf("LastWipe in the future: %v", err)
	}
	// The limits of the numbers are the game's.
	for key, want := range map[string][2]string{
		"HarvestRate": {"0.1", "1000"}, "DayLengthMinutes": {"5", "240"}, "StructureDecayMultiplier": {"0.1", "10"},
		"ORPActivationMinutes": {"0", "1440"}, "ORPStructureDamageTaken": {"0", "1"}, "ORPTurretDamageMultiplier": {"0", "10"},
		"AdminLoginLockoutSeconds": {"0", "3600"},
	} {
		s, _ := LookupGameSetting(key)
		if s.Min == nil || s.Max == nil || strconv.FormatFloat(*s.Min, 'f', -1, 64) != want[0] || strconv.FormatFloat(*s.Max, 'f', -1, 64) != want[1] {
			t.Errorf("%s: range %v..%v, want %s..%s", key, s.Min, s.Max, want[0], want[1])
		}
	}
}

// A list is written with a space after each comma: the server ends a value
// without white space at its first comma, and reads a value with white
// space whole. Other values are written as they are.
func TestLaunchArgsWriteListsWhole(t *testing.T) {
	l := goldenLaunch()
	l.Rules = map[string]string{
		"ChatBlockedWords": "rude,ruder , ,rudest",
		"AdminPlayerIds":   "76561190000000001",
		"MOTD":             "Welcome, traveller",
		"HarvestRate":      "2.5",
		"SomethingElse":    "a,b",
	}
	argv := l.Args()
	for _, want := range []string{"-ChatBlockedWords=rude, ruder, rudest", "-AdminPlayerIds=76561190000000001",
		"-MOTD=Welcome, traveller", "-HarvestRate=2.5", "-SomethingElse=a,b"} {
		if !slices.Contains(argv, want) {
			t.Errorf("%q is not on the command line %q", want, argv)
		}
	}
}

// A switch with an empty value is never passed, neither as "-Key=" (the
// server reads an empty unquoted value as the argument that follows it,
// which could be another switch) nor as the bare switch (which turns a
// setting like PvP on). A rule stored with an empty value is left out, and
// so is an extra argument that ends in "="; LeftOut names them.
func TestLaunchArgsLeaveEmptyValuesOut(t *testing.T) {
	l := goldenLaunch()
	l.ServerName = ""
	l.Rules = map[string]string{"MOTD": "", "PvP": "", "ChatBlockedWords": " , ", "Foo": "", "HarvestRate": "2"}
	l.ExtraArgs = []string{"-Bar=", "-Baz==", "-Keep=1", "-NoSteam"}
	for _, delivery := range []string{DeliveryFile, DeliveryCommandLine} {
		l.Delivery = delivery
		argv := l.Args()
		for _, a := range argv {
			if strings.HasSuffix(a, "=") {
				t.Errorf("%s: %q ends in =", delivery, a)
			}
			for _, gone := range []string{"-MOTD", "-PvP", "-ChatBlockedWords", "-Foo", "-Bar", "-Baz", "-ServerName"} {
				if a == gone || strings.HasPrefix(a, gone+"=") {
					t.Errorf("%s: %q is passed although it is empty", delivery, a)
				}
			}
		}
		for _, want := range []string{"-Keep=1", "-NoSteam", "-HarvestRate=2"} {
			if !slices.Contains(argv, want) {
				t.Errorf("%s: %q is not on the command line %q", delivery, want, argv)
			}
		}
	}
	if got, want := l.LeftOut(), []string{"ChatBlockedWords", "Foo", "MOTD", "PvP", "extra argument 1", "extra argument 2"}; !slices.Equal(got, want) {
		t.Errorf("LeftOut: %q, want %q", got, want)
	}
	// A value stored before the rules of CheckValue is passed unchanged.
	l.Rules = map[string]string{"Mode": "x;LOG="}
	if argv := l.Args(); !slices.Contains(argv, "-Mode=x;LOG=") {
		t.Errorf("a stored value is not passed as it is: %q", argv)
	}
}

// The server finds a switch anywhere in its command line where "Name="
// follows something that is not an ASCII letter or digit: outside quotes
// by its usual lookup, and inside them too by some of its code. And it
// ends a value without quotes at "," or ")". So no value holds "=", and a
// value without a space, which the server does not quote, holds no ","
// or ")" either; one with a space is quoted whole and may. These rules do
// not look for names: a name of any switch, in any script, is caught the
// same way.
func TestCheckValue(t *testing.T) {
	for _, v := range []string{
		"", " ", " lead", "trail ", "a\tb", "a\nb", "a\u00a0b", "a\u2028b", "a\u0085b", "a\x7fb", `my"pass99`, "x -Port=1",
		"x;LOG=", "Hi=", "\u212AAdminPassword=evil", "\u0130LAN=1", "secret,AdminPassword=evil", "secret;AdminPassword=",
		"abc,def123", "pass)word99", "adm;Password=joinme", "https://e.com/n?AdminPassword=x", "-iniFile=/x", "Hi;LOG=x.log",
		"a=b", "PORT=1", "(motd=hi)", "1,2", "x = y", "Hi, PvP=True all", "a b,c)d=e",
		"Welcome x-Port=1 to all", "a, b=c (d)", "\xff",
	} {
		if why, bad := CheckValue(v); !bad || why == "" || strings.Contains(why, v) && v != "" && v != " " {
			t.Errorf("%q: %v %q", v, bad, why)
		}
	}
	for _, v := range []string{
		"Alpha", "Alpha EU - west", "Welcome, traveller", "Be nice (please)", "x - y", "Hi, PvP True all", "a b,c)d;e",
		"Café", "\u212Aelvin", "-5", "https://example.com/path", "rude, ruder", "q7;x", "Password", "/Game/x",
	} {
		if why, bad := CheckValue(v); bad {
			t.Errorf("%q refused: %s", v, why)
		}
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for key, values := range map[string][]string{
		"MOTD":             {"-PvP=True", "join,Password=x", "Hi;LOG=x.log", "\u212AAdminPassword=evil"},
		"AdminPlayerIds":   {"123;Password=x", "x_LAN=1", "1,2,x_LAN=1", "secret,AdminPassword=evil"},
		"ChatBlockedWords": {"PvP=1", "rude,PvP=1"},
		"AnnouncementsUrl": {"https://example.com/news?AdminPassword=x", "https://example.com/news?x=1", "https://example.com/news#top", "https://example.com/a=b"},
		"ProblemReportUrl": {"https://example.com/r?token=1"},
	} {
		s, _ := LookupGameSetting(key)
		for _, v := range values {
			if err := s.Check(v, now); err == nil {
				t.Errorf("%s=%q accepted", key, v)
			}
		}
	}
	// Quoted whole, a value with a space may hold "," and ")". A list of
	// two or more items is written with ", ", so it is quoted whole too.
	for key, values := range map[string][]string{
		"MOTD":             {"Welcome x-Port 1 to all", "a, b;c (d)"},
		"AdminPlayerIds":   {"1,2", "abc,def123"},
		"ChatBlockedWords": {"rude;x,ruder"},
	} {
		s, _ := LookupGameSetting(key)
		for _, v := range values {
			if err := s.Check(v, now); err != nil {
				t.Errorf("%s=%q refused: %v", key, v, err)
			}
		}
	}
	if why, refused := RefusedSwitch("-CustomConfig"); !refused || !strings.Contains(why, "are refused") {
		t.Errorf("-CustomConfig: %q %v", why, refused)
	}
	for name, want := range map[string]bool{"PvP": true, "A1": true, "x_PvP": false, "Foo.LAN": false, "Foo-Port": false, "Foo:Port": false,
		"": false, "\u212A": false, "Café": false, "a b": false} {
		if got := PlainSwitchName(name); got != want {
			t.Errorf("PlainSwitchName(%q) = %v", name, got)
		}
	}
	if !SameSwitch("PvP", "pvp") || SameSwitch("k", "\u212A") || SameSwitch("PvP", "PvPx") {
		t.Error("SameSwitch")
	}
}

func TestCatalogWords(t *testing.T) {
	if s, _ := LookupGameSetting("AdminLoginLockoutSeconds"); !strings.Contains(s.Help, "0: no lockout") {
		t.Errorf("AdminLoginLockoutSeconds help: %q", s.Help)
	}
}
