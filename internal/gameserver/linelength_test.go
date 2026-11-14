package gameserver

import (
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// The length ESM counts for a command line is the length of the line the
// launcher joins it into, as the engine counts it.
func TestLineLength(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{"Map"},
		{"Map", "-unattended", "-Port=7777"},
		{"/Game/Maps/A", "-ServerName=Alpha EU", "-MOTD=Hi, all (welcome)", "two words", "-Empty=", "-NoValue but a space"},
		{"-A=\u00e9t\u00e9", "-B=\u20ac", "-C=Go \U0001F642 on", "-D=\U0001F642\U0001F642"},
		goldenLaunch().Args(),
	} {
		line := cmdline.Join(argv)
		if got, want := LineLength(argv), cmdline.Units(line); got != want {
			t.Errorf("LineLength(%q) = %d, the line %q has %d", argv, got, line, want)
		}
	}
	// ESM starts a server with a line a little shorter than the engine
	// starts with, and lets a change lead to one well shorter than that.
	if EngineLineLimit != cmdline.Limit || StartLineLimit != EngineLineLimit-StartRoom || StartRoom < 64 || StartRoom > 1024 ||
		MaxLineLength >= StartLineLimit || MaxLineLength < EngineLineLimit/2 {
		t.Errorf("the limits: %d after a change, %d at a start, the engine's %d", MaxLineLength, StartLineLimit, EngineLineLimit)
	}
}
