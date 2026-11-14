package cmdline

import (
	"slices"
	"testing"
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
