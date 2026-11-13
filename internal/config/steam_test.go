package config

import (
	"path/filepath"
	"testing"
)

func TestSteamSettings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	c := Defaults(root)
	if c.SteamAppID != "" || c.StopTimeout.D() != DefaultStopTimeout {
		t.Fatalf("defaults: app id %q, stop timeout %s", c.SteamAppID, c.StopTimeout)
	}
	if got := c.SteamRuntime(); got != filepath.Join(root, "steam", "linux64") {
		t.Fatalf("default runtime folder %q", got)
	}
	c.SteamRuntimeDir = "rt/x"
	if got := c.SteamRuntime(); got != filepath.Join(root, "rt", "x") {
		t.Fatalf("relative runtime folder %q", got)
	}
	abs := filepath.Join(t.TempDir(), "steam")
	c.SteamRuntimeDir = abs
	if got := c.SteamRuntime(); got != abs {
		t.Fatalf("absolute runtime folder %q", got)
	}
	for id, ok := range map[string]bool{"": true, "480": true, "1000001": true, "4294967295": true,
		"0": false, "0480": false, "12345678901": false, "48a": false, " 480": false, "-1": false} {
		c := Defaults(root)
		c.SteamAppID = id
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("steam_app_id %q: %v, want ok=%v", id, err, ok)
		}
	}
	c = Defaults(root)
	c.SteamRuntimeDir = "a\nb"
	if c.Validate() == nil {
		t.Error("a runtime folder with a line break is accepted")
	}
}
