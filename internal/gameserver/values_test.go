package gameserver

import (
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// What a value on the command line may hold, beyond what TestCheckValue
// says: no white space of any host of the engine but the plain space, no
// character that is not printable text, no en dash, no "=" at all (the
// engine finds a name and "=" inside quotes too), no "-" and a letter at
// the start of a word, and no name the engine searches the line for.
func TestCheckValueOnEveryHost(t *testing.T) {
	spaces := []rune{0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x85, 0xa0, 0x1680, 0x180e, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
		0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	for _, r := range spaces {
		for _, v := range []string{"Hi" + string(r) + "-PvP", "Hi" + string(r) + "/LAN", "Hi" + string(r) + "x", "a b" + string(r) + "c d",
			string(r) + "x", "x" + string(r)} {
			if why, bad := CheckValue(v); !bad || !strings.Contains(why, "white space") {
				t.Errorf("U+%04X in %q: %v %q", r, v, bad, why)
			}
		}
	}
	// Every character that some host counts as white space, by the reader's
	// own list of them, and everything Unicode calls white space.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if (cmdline.Space(r) || unicode.IsSpace(r)) && r != ' ' {
			if _, bad := CheckValue("a" + string(r) + "b"); !bad {
				t.Errorf("U+%04X: accepted", r)
			}
		}
	}
	// Not printable text: controls, format characters, line and paragraph
	// separators, every space but the plain one and private-use code
	// points, and with them whatever is no letter, mark, number,
	// punctuation or symbol: the code points Unicode has not assigned. (A
	// surrogate is no character of a valid text.)
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if 0xd800 <= r && r <= 0xdfff || r == ' ' {
			continue
		}
		named := unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Zs, unicode.Co)
		if named || !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S) {
			if why, bad := CheckValue("a" + string(r) + "b c"); !bad || why == "" {
				t.Fatalf("U+%04X: accepted", r)
			}
		}
	}
	// Code points that are not assigned, whatever table of Unicode this
	// was built with: gaps that have been there for long, and
	// noncharacters.
	for _, r := range []rune{0x0378, 0x0380, 0xfffe, 0xffff, 0x1fffe, 0x1ffff, 0xe0080, 0xeffff, 0x10ffff} {
		if why, bad := CheckValue("a" + string(r) + "b c"); !bad || !strings.Contains(why, "not printable") {
			t.Errorf("U+%04X: %v %q", r, bad, why)
		}
	}
	// A space followed by "-" and more, with nothing else wrong.
	for _, v := range []string{"a -b", "Alpha -EU west", "x -5", "well met -", "a - -b"} {
		why, bad := CheckValue(v)
		if want := v != "well met -"; bad != want || bad && !strings.Contains(why, `a space followed by "-"`) {
			t.Errorf("%q: %v %q", v, bad, why)
		}
	}
	for _, v := range []string{
		"x = y", "Hi, PvP=True all", "a b,c)d=e", "see PLUGIN=/tmp/x now", "well x-sigdfl=5 met", "a-LogCmds=global off", "=", "a =",
		"Alpha \u2013 EU", "Alpha\u2013EU",
		"-fast", "a;-b", "(-PvP-)", "[EU]-Alpha", "Caf\u00e9-bar", "x_-y", "a--b", "Hi there;-PvP now",
		"x-ansimalloc", "My-AnsiMalloc Server", "a -StompMalloc b", "well x-binnedmalloc2 met", "SteamSocketsRelays1", "a;SteamSocketsRelays0",
		"see ddpi:Linux: x", "join +connect 1.2.3.4", "type /CleanCrashReports now", "x-EnvAfterHere y", "csv.TargetFrameRateOverride30",
	} {
		why, bad := CheckValue(v)
		if !bad || why == "" {
			t.Errorf("%q: accepted", v)
		}
		for _, name := range cmdline.Searched {
			if bad && strings.Contains(strings.ToLower(why), name) {
				t.Errorf("%q: the answer repeats a part of the value: %s", v, why)
			}
		}
	}
	for _, v := range []string{
		"Alpha", "Alpha EU - west", "Welcome, traveller", "Be nice (please)", "Caf\u00e9", "\u212aelvin", "-5", "-.5", "a-5",
		"https://example.com/path", "https://my-site.example.com/a-b", "rude, ruder", "q7;x", "Password", "/Game/x", "well-met friends",
		"Alpha-EU", "24-7", "type /help for the commands", "a - b", "use multihome", "MULTIHOME", "Malloc and mimic", "x - y - z",
		"\u041c\u043e\u0439-\u0421\u0435\u0440\u0432\u0435\u0440", "A \u2014 B", "Go \U0001F642 on", "e\u0301t\u00e9", "100% (really)", "a+b", "x: y; z",
	} {
		if why, bad := CheckValue(v); bad {
			t.Errorf("%q refused: %s", v, why)
		}
	}
}

// White space is one list in ESM: what some host of the engine counts as
// white space. It is the same list the engine's reader in the tests has,
// and holds everything Unicode calls white space.
func TestEngineSpace(t *testing.T) {
	want := []rune{0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x180e, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005,
		0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	var got []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if EngineSpace(r) {
			got = append(got, r)
		}
		if EngineSpace(r) != cmdline.Space(r) {
			t.Errorf("U+%04X: white space to ESM %v, to the engine's reader %v", r, EngineSpace(r), cmdline.Space(r))
		}
		if unicode.IsSpace(r) && !EngineSpace(r) {
			t.Errorf("U+%04X is white space to Unicode and not to ESM", r)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("white space:\n got %U\nwant %U", got, want)
	}
	// The older check of every setting uses the same list.
	for _, r := range want {
		_, unsafe := UnsafeArgument("x" + string(r) + "-Port")
		if !unsafe {
			t.Errorf("U+%04X before a dash is not white space to UnsafeArgument", r)
		}
	}
	if _, unsafe := UnsafeArgument("well-met - friends"); unsafe {
		t.Error("a dash in a word, and one alone, are refused")
	}
}

// The names ESM knows the engine to search for as plain text are the ones
// the engine's reader in the tests lists without an "="; the ones with an
// "=" are kept out by the rule that no value holds "=". A value that holds
// one, in any case and anywhere, is refused without the answer saying
// which; SearchedName says which. A switch name that holds one of them
// is refused too, the switch itself is not.
func TestSearchedNames(t *testing.T) {
	var bare []string
	for _, name := range cmdline.Searched {
		if !strings.Contains(name, "=") {
			bare = append(bare, name)
			continue
		}
		if why, bad := CheckValue("Zq7 x" + name + "5 Jx9"); !bad || !strings.Contains(why, `"="`) {
			t.Errorf("a value with %q: %v %q", name, bad, why)
		}
	}
	got := SearchedNames()
	slices.Sort(bare)
	slices.Sort(got)
	if !slices.Equal(got, bare) {
		t.Fatalf("the names ESM knows:\n got %q\nwant %q", got, bare)
	}
	for _, name := range bare {
		for _, v := range []string{"Zq7 x" + name + "5 Jx9", "x" + name, name + "x", strings.ToUpper("a b" + name), "Zq7 x" + strings.ToUpper(name[:2]) + name[2:] + " Jx9"} {
			why, bad := CheckValue(v)
			if !bad {
				t.Errorf("%q: accepted", v)
				continue
			}
			if strings.Contains(strings.ToLower(why), name) {
				t.Errorf("%q: the answer says which name: %s", v, why)
			}
			if found, ok := SearchedName(v); !ok || !strings.Contains(name, found) {
				t.Errorf("SearchedName(%q) = %q %v", v, found, ok)
			}
		}
	}
	if why, bad := CheckValue("Zq7 x-ansimalloc Jx9"); !bad || why != HoldsSearchedName {
		t.Errorf("the answer for a name: %q", why)
	}
	for _, v := range []string{"", "malloc", "ansi malloc", "an-simalloc", "x-an\u017fimalloc", "connect", "trace host", "nullrhi", "ddpi"} {
		if name, ok := SearchedName(v); ok {
			t.Errorf("SearchedName(%q) = %q", v, name)
		}
	}
	// None of the switches ESM writes itself, and no setting of the
	// catalog, holds one.
	for _, name := range reserved {
		if found, ok := SearchedSwitch(name); ok {
			t.Errorf("ESM's own switch %s holds %q", name, found)
		}
	}
	for _, g := range GameSettings() {
		if found, ok := SearchedSwitch(g.Key); ok {
			t.Errorf("the game setting %s holds %q", g.Key, found)
		}
	}
	for name, want := range map[string]string{
		"ansimalloc": "", "AnsiMalloc": "", "ansimallocx": "-ansimalloc", "StompMallocNow": "-stompmalloc",
		"binnedmalloc": "", "BinnedMalloc2": "", "binnedmalloc3": "", "binnedmalloc22": "-binnedmalloc", "binnedmalloc9": "-binnedmalloc",
		"tracehost": "", "TraceHosts": "-tracehost", "SteamSocketsRelays": "", "xSteamSocketsRelays": "steamsocketsrelays",
		"SteamSocketsRelays1": "steamsocketsrelays", "metalshadervalidation": "", "PvP": "", "Mode": "", "": "", "x": "",
	} {
		got, found := SearchedSwitch(name)
		if got != want || found != (want != "") {
			t.Errorf("SearchedSwitch(%q) = %q %v, want %q", name, got, found, want)
		}
	}
}
