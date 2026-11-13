package instances

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The game server takes at most 70 players. A new instance, or a change
// of max_players, is held to that; an instance stored with more keeps
// loading and starting as before (the server uses 70), and says so in a
// warning, until its max_players is changed.
func TestMaxPlayersIsTheGamesLimit(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	_, err := r.m.Create(CreateRequest{Name: "big", Settings: Settings{BindIP: ptr("127.0.0.1"), MaxPlayers: ptr(71)}})
	if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "max_players") || !strings.Contains(err.Error(), "70") {
		t.Fatalf("create with 71 players: %v", err)
	}
	if _, err := r.m.Create(CreateRequest{Name: "full", Settings: Settings{BindIP: ptr("127.0.0.1"), MaxPlayers: ptr(70)}}); err != nil {
		t.Fatalf("create with 70 players: %v", err)
	}
	if v, _ := r.m.Get("full"); len(v.Warnings) != 0 {
		t.Fatalf("warnings at 70: %q", v.Warnings)
	}

	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.MaxPlayers = 100
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	v, err := m.Get("alpha")
	if err != nil || v.MaxPlayers != 100 || !slices.Contains(v.Warnings, WarnMaxPlayersAboveGame) {
		t.Fatalf("stored with 100 players: %d %q %v", v.MaxPlayers, v.Warnings, err)
	}
	if !strings.Contains(WarningText(WarnMaxPlayersAboveGame), "70") {
		t.Fatalf("warning text: %q", WarningText(WarnMaxPlayersAboveGame))
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha"), MaxPlayers: ptr(100)}}); err != nil {
		t.Fatalf("a change that leaves 100 players: %v", err)
	}
	m.Start(context.Background(), "alpha", false)
	if spec, _, _ := sup.last(t); !slices.Contains(spec.Args, "-MaxPlayers=100") {
		t.Fatalf("the stored value is no longer passed: %q", spec.Args)
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{MaxPlayers: ptr(90)}}); !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "70") {
		t.Fatalf("a change to 90 players: %v", err)
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{MaxPlayers: ptr(40)}}); err != nil {
		t.Fatalf("a change to 40 players: %v", err)
	}
	if v, _ := m.Get("alpha"); slices.Contains(v.Warnings, WarnMaxPlayersAboveGame) {
		t.Fatal("the warning stays at 40 players")
	}
}

// An override of the engine's configuration files (-ini:..., -iniFile=)
// could change what ESM sets itself (ports, networking, where the server
// keeps its files), so it is refused as an extra argument and as a rule.
func TestEngineConfigOverridesAreRefused(t *testing.T) {
	for _, arg := range []string{
		"-ini:Engine:[URL]:Port=7000",
		"-ini:Game:[/Script/Example.Settings]:Rate=2",
		"-INI:engine:[/Script/Engine.Engine]:NetDriverDefinitions=x",
		"--ini:Engine:[URL]:Port=7000",
		"-iniFile=/tmp/override.ini",
		"-ini",
	} {
		in := validInstance()
		in.ExtraArgs = []string{"-NoSteam", arg}
		err := in.Validate()
		if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "argument 2") || !strings.Contains(err.Error(), "-ini") {
			t.Errorf("%q: %v", arg, err)
		}
	}
	for _, key := range []string{"ini", "IniFile"} {
		in := validInstance()
		in.Rules = map[string]string{key: "x"}
		if err := in.Validate(); !fault.Is(err, fault.Invalid) {
			t.Errorf("rule %s: %v", key, err)
		}
	}
	ok := validInstance()
	ok.ExtraArgs = []string{"-Initial=1", "-Mini:x", "-NoIni"}
	ok.Rules = map[string]string{"Initial": "1", "Mini": "x"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("names that only start with ini: %v", err)
	}
}

// The refusal of a max_players outside what may be stored says both
// limits: 70 for a new value, 200 for one stored by an earlier version.
func TestMaxPlayersRefusalWords(t *testing.T) {
	in := validInstance()
	in.MaxPlayers = 201
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "1 and 70") || !strings.Contains(err.Error(), "200") {
		t.Fatalf("201 players: %v", err)
	}
}
