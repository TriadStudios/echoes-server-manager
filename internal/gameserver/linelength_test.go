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
	if EngineLineLimit != cmdline.Limit || MaxLineLength >= EngineLineLimit || MaxLineLength < EngineLineLimit/2 {
		t.Errorf("the limits: %d, the engine's %d", MaxLineLength, EngineLineLimit)
	}
}
