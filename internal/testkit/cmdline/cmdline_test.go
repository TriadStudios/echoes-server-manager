package cmdline

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

func TestJoin(t *testing.T) {
	got := Join([]string{"/Game/Maps/A", "-unattended", "-ServerName=Alpha EU", "-Motd Hi", "two words", "-Empty=", "-A=b=c d"})
	want := ` /Game/Maps/A -unattended -ServerName="Alpha EU" "-Motd Hi" "two words" -Empty= -A="b=c d"`
	if got != want {
		t.Fatalf("Join:\n got %s\nwant %s", got, want)
	}
	// Only the plain space makes the launcher quote.
	if got := Join([]string{"-A=x\ty", "-B=x\u00a0y"}); got != " -A=x\ty -B=x\u00a0y" {
		t.Fatalf("Join with other white space: %q", got)
	}
}

func TestValue(t *testing.T) {
	line := Join([]string{"Map", "-Port=7777", "-QueryPort=27015", "-ServerName=Alpha EU", "-MOTD=Hi,PvP=True", "-Mode=x)y",
		"-Note=a b,c)d=e", "-Empty=", "-Next=after"})
	for name, want := range map[string]string{
		"Port":       "7777",
		"queryport":  "27015",
		"ServerName": "Alpha EU",
		"MOTD":       "Hi",
		"PvP":        "True",
		"Mode":       "x",
		"Note":       "a b,c)d=e",
		"Empty":      "-Next=after",
		"Next":       "after",
	} {
		if got, ok := Value(line, name); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"d", "e", "c", "Query", "Name", "Server"} {
		if got, ok := Value(line, name); ok {
			t.Errorf("%s found: %q", name, got)
		}
	}
	// Only ASCII letters are compared without case, and a character
	// beyond ASCII is not a letter in front of a name.
	line = Join([]string{"-A=\u212AAdminPassword=evil", "-B=\u0130LAN=1"})
	if got, ok := Value(line, "AdminPassword"); !ok || got != "evil" {
		t.Errorf("after U+212A: %q %v", got, ok)
	}
	if got, ok := Value(line, "LAN"); !ok || got != "1" {
		t.Errorf("after U+0130: %q %v", got, ok)
	}
	if _, ok := Value(line, "kAdminPassword"); ok {
		t.Error("U+212A matched k")
	}
	// A double quote in a value turns the rest of the line inside out.
	line = Join([]string{`-Motd=my"pass`, "-Password=real", `-X=y"`, "-AdminPassword=late"})
	if _, ok := Value(line, "Password"); ok {
		t.Error("a switch after an odd quote was found")
	}
	if got, ok := Value(line, "AdminPassword"); !ok || got != "late" {
		t.Errorf("after the quotes are even again: %q %v", got, ok)
	}
	// A quoted value runs to the next quote, also to the end.
	if got, ok := Value(` -A="x y`, "A"); !ok || got != "x y" {
		t.Errorf("open quote: %q %v", got, ok)
	}
	// An empty value at the end is empty.
	if got, ok := Value(" -A=", "A"); !ok || got != "" {
		t.Errorf("empty at the end: %q %v", got, ok)
	}
}

func TestParamAndBool(t *testing.T) {
	line := Join([]string{"/PvP", "-unattended", "-LAN", "-MOTD=-ChatLog", "-Mode=x /Foo", "-Pvx", "-x-Bar"})
	for name, want := range map[string]bool{
		"PvP": true, "unattended": true, "lan": true, "ChatLog": false, "Foo": false, "Pvx": true, "Bar": false, "Mode": false,
	} {
		if got := Param(line, name); got != want {
			t.Errorf("Param %s: %v, want %v", name, got, want)
		}
	}
	line = Join([]string{"-PvP", "-LAN=False", "-ChatLog=1", "-StructureDecay=no", "-AllowNonSteamClients=", "-X=y"})
	for name, want := range map[string][2]bool{
		"PvP": {true, true}, "LAN": {false, true}, "ChatLog": {true, true}, "StructureDecay": {false, true},
		// The empty value takes the next argument, "-X=y", which is no number.
		"AllowNonSteamClients": {false, true}, "OfflineRaidProtection": {false, false},
	} {
		if on, set := Bool(line, name); on != want[0] || set != want[1] {
			t.Errorf("Bool %s: %v %v, want %v", name, on, set, want)
		}
	}
}

// White space is what some host of the engine counts as such: the ASCII
// ones everywhere, and on Windows the rest of this list, U+180E among them,
// which is no white space to Unicode any more. A bare switch after any of
// them is found; one after another character is not.
func TestSpaceIsWhatAnyHostCounts(t *testing.T) {
	want := []rune{0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x180e, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005,
		0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000}
	var got []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if Space(r) {
			got = append(got, r)
		}
		// Unicode's own white space is in it.
		if unicode.IsSpace(r) && !Space(r) {
			t.Errorf("U+%04X is white space to Unicode and not here", r)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("white space:\n got %U\nwant %U", got, want)
	}
	for _, r := range want {
		if r == ' ' {
			continue
		}
		for _, lead := range []string{"-", "/"} {
			line := Join([]string{"-MOTD=Hi" + string(r) + lead + "AllowNonSteamClients", "-B=1"})
			if strings.Contains(line, `"`) {
				t.Fatalf("U+%04X made the launcher quote: %q", r, line)
			}
			if on, set := Bool(line, "AllowNonSteamClients"); !on || !set {
				t.Errorf("the bare switch after U+%04X and %q is not found", r, lead)
			}
		}
		// After the name too: the switch ends at white space.
		if line := Join([]string{"-PvP" + string(r) + "x"}); !Param(line, "PvP") {
			t.Errorf("a bare switch followed by U+%04X is not found", r)
		}
	}
	for _, r := range []rune{'x', ';', 0x200b, 0x2060, 0xfeff, 0x00ad, 0x2010} {
		if line := Join([]string{"-MOTD=Hi" + string(r) + "-PvP"}); Param(line, "PvP") {
			t.Errorf("a bare switch is found after U+%04X", r)
		}
	}
}

// The engine stops at a line that is too long for it, counted in 16-bit
// units, and at an en dash.
func TestRefuses(t *testing.T) {
	head := " -MOTD="
	fits := head + strings.Repeat("x", Limit-len(head))
	if Units(fits) != Limit || Refuses(fits) != "" {
		t.Errorf("a line of %d: %q", Units(fits), Refuses(fits))
	}
	if why := Refuses(fits + "x"); !strings.Contains(why, "longer") {
		t.Errorf("a line of %d: %q", Limit+1, why)
	}
	// A character beyond U+FFFF takes two units, any other one.
	if n := Units("a\u20ac\U0001F600"); n != 4 {
		t.Errorf("units: %d", n)
	}
	wide := head + strings.Repeat("\U0001F600", (Limit-len(head))/2+1)
	if why := Refuses(wide); !strings.Contains(why, "longer") {
		t.Errorf("a line of %d units: %q", Units(wide), why)
	}
	if why := Refuses(" -ServerName=\"Alpha \u2013 EU\""); !strings.Contains(why, "en dash") {
		t.Errorf("en dash: %q", why)
	}
	if why := Refuses(" -ServerName=\"Alpha - EU \u2014 x\""); why != "" {
		t.Errorf("a hyphen and an em dash: %q", why)
	}
}

// The names the engine searches for as plain text are found wherever they
// stand in an argument: inside a value with a space, which the launcher
// quotes, in the middle of a word, and in any case of ASCII letters.
func TestFound(t *testing.T) {
	for _, name := range Searched {
		if name != strings.ToLower(name) || strings.ContainsAny(name, " \"") {
			t.Errorf("%q: the names are in lower case, without a space or a quote", name)
		}
		for _, argv := range [][]string{
			{"Map", "-MOTD=well " + name + "5 met"},
			{"Map", "-MOTD=x" + strings.ToUpper(name) + "y"},
			{"Map", "-A=1", "-UserDir=/srv/x" + name + "/data"},
		} {
			if got := Found(argv); !slices.Contains(got, name) {
				t.Errorf("%q is not found in %q (found %q)", name, argv, got)
			}
		}
	}
	if got := Found([]string{"Map", "-unattended", "-Port=7777", "-MOTD=well-met friends, type /help", "-Mode=simple"}); len(got) != 0 {
		t.Errorf("found %q in a plain command line", got)
	}
	// Only ASCII letters are compared without case.
	if got := Found([]string{"-MOTD=x-an\u017fimalloc y", "-B=\u212aplugin\u212a=1"}); len(got) != 0 {
		t.Errorf("found %q", got)
	}
	if got := Found([]string{"-ansimalloc", "-MOTD=a -StompMalloc b"}); !slices.Equal(got, []string{"-ansimalloc", "-stompmalloc"}) {
		t.Errorf("found %q", got)
	}
}

func TestSwitches(t *testing.T) {
	line := Join([]string{"Map", "-Port=1", "-x_PvP=True", "-Foo=a;LOG=b", "-Bar=c d=e", "-Baz=Hi=", "-Qux=\u212AAdminPassword=evil", `-Q="in"side=1`})
	var names []string
	for _, s := range Switches(line) {
		names = append(names, s.Name+"="+s.Value)
	}
	want := []string{"Port=1", "PvP=True", "Foo=a;LOG=b", "LOG=b", "Bar=c d=e", "Baz=Hi=", "Hi=-Qux=\u212AAdminPassword=evil",
		"Qux=\u212AAdminPassword=evil", "AdminPassword=evil", "Q=in", "side=1"}
	if !slices.Equal(names, want) {
		t.Fatalf("Switches:\n got %q\nwant %q", names, want)
	}
	if !SameName("PvP", "pvp") || SameName("PvP", "PvPx") || SameName("k", "\u212A") {
		t.Fatal("SameName")
	}
}
