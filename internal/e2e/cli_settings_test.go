package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// esm game-settings lists the catalog: a line for each setting with its
// type, default, range and flags, or the API's object with --json. create
// --set checks a catalog value as the API does.
func TestCLIGameSettings(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	code, out, errOut := h.runCLI("game-settings")
	if code != 0 {
		t.Fatalf("game-settings: %d %q", code, errOut)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	for _, s := range gameserver.GameSettings() {
		found := false
		for _, l := range lines {
			if f := strings.Fields(l); len(f) > 1 && f[0] == s.Key {
				found = true
				if !strings.Contains(l, s.Type) {
					t.Errorf("%s: no type in %q", s.Key, l)
				}
				if s.NewWorldOnly && !strings.Contains(l, "new world only") {
					t.Errorf("%s: not marked as a world rule: %q", s.Key, l)
				}
				if s.Secret && !strings.Contains(l, "secret") {
					t.Errorf("%s: not marked as a secret: %q", s.Key, l)
				}
			}
		}
		if !found {
			t.Errorf("%s is not listed", s.Key)
		}
	}
	if !strings.Contains(out, "HarvestRate") || !strings.Contains(out, "0.1..1000") || !strings.Contains(out, "caution") {
		t.Fatalf("game-settings:\n%s", out)
	}
	code, out, _ = h.runCLI("game-settings", "--group", "chat")
	if code != 0 || strings.Contains(out, "HarvestRate") || !strings.Contains(out, "ChatFilter") {
		t.Fatalf("game-settings --group chat: %d\n%s", code, out)
	}
	if code, _, _ := h.runCLI("game-settings", "--group", "nope"); code == 0 {
		t.Fatal("an unknown group is accepted")
	}
	code, out, _ = h.runCLI("game-settings", "--json")
	var cat api.GameSettingList
	if code != 0 || json.Unmarshal([]byte(oneLine(t, "game-settings --json", out)), &cat) != nil || len(cat.Settings) != len(gameserver.GameSettings()) {
		t.Fatalf("game-settings --json: %d %q", code, out)
	}

	b := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "1.0"})
	if code, _, errOut := h.runCLI("build", "install", b); code != 0 {
		t.Fatalf("build install: %q", errOut)
	}
	code, _, errOut = h.runCLI("create", "alpha", "--bind", "127.0.0.1", "--set", "HarvestRate=5000")
	if code != 1 || !strings.Contains(errOut, "HarvestRate must be a number from 0.1 to 1000") {
		t.Fatalf("create with a rate out of range: %d %q", code, errOut)
	}
	code, _, errOut = h.runCLI("create", "alpha", "--bind", "127.0.0.1", "--set", "HarvestRate=2", "--set", "Unknown=x")
	if code != 0 {
		t.Fatalf("create with a good rate: %d %q", code, errOut)
	}
}

// esm secret sets and clears a password or token of an instance, read from
// standard input, and prints only which ones are set.
func TestCLISecret(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	b := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "1.0"})
	if code, _, errOut := h.runCLI("build", "install", b); code != 0 {
		t.Fatalf("build install: %q", errOut)
	}
	if code, _, errOut := h.runCLI("create", "alpha", "--bind", "127.0.0.1"); code != 0 {
		t.Fatalf("create: %q", errOut)
	}
	const tok = "cli-Ann0unce-55x"
	code, out, errOut := h.runCLIIn(tok+"\n", "secret", "alpha", "announcements-token")
	line := oneLine(t, "secret", out)
	if code != 0 || strings.Contains(out, tok) || !strings.Contains(line, "set the announcements token of alpha") {
		t.Fatalf("secret: %d %q %q", code, out, errOut)
	}
	var v api.InstanceChange
	code, out, _ = h.runCLI("status", "alpha", "--json")
	if code != 0 || json.Unmarshal([]byte(oneLine(t, "status", out)), &v) != nil || !v.Secrets.AnnouncementsTokenSet || strings.Contains(out, tok) {
		t.Fatalf("status after secret: %q", out)
	}
	if code, _, errOut := h.runCLIIn("", "secret", "alpha", "announcements-token"); code != 1 || !strings.Contains(errOut, "standard input") {
		t.Fatalf("no value on stdin: %d %q", code, errOut)
	}
	if code, _, errOut := h.runCLIIn("short\n", "secret", "alpha", "problem-report-token"); code != 1 || strings.Contains(errOut, "short") {
		t.Fatalf("a short token: %d %q", code, errOut)
	}
	if code, _, errOut := h.runCLIIn("x\n", "secret", "alpha", "rcon-password"); code != 1 || !strings.Contains(errOut, "announcements-token") {
		t.Fatalf("an unknown secret: %d %q", code, errOut)
	}
	code, out, _ = h.runCLIIn("", "secret", "alpha", "announcements-token", "--clear")
	if code != 0 || !strings.Contains(oneLine(t, "secret --clear", out), "removed the announcements token of alpha") {
		t.Fatalf("secret --clear: %d %q", code, out)
	}
	code, out, errOut = h.runCLIIn("a join password 1\n", "secret", "alpha", "server-password")
	if code != 0 || !strings.Contains(oneLine(t, "secret", out), "set the server password of alpha") {
		t.Fatalf("secret server-password: %d %q %q", code, out, errOut)
	}
}
