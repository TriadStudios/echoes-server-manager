package instances

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A rule that names a game setting of the catalog is checked by the
// setting's type and range when it is set; every rule value follows the
// rules for values on the server command line (gameserver.CheckValue).
func TestGameSettingsAreCheckedWhenSet(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	create := func(name string, rules map[string]string) error {
		_, err := r.m.Create(CreateRequest{Name: name, Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(rules)}})
		return err
	}
	for what, c := range map[string]struct {
		rules map[string]string
		say   []string
	}{
		"rate too high":     {map[string]string{"HarvestRate": "5000"}, []string{"HarvestRate", "0.1", "1000"}},
		"not a bool":        {map[string]string{"PvP": "on"}, []string{"PvP", "True or False"}},
		"unknown choice":    {map[string]string{"Difficulty": "hard"}, []string{"Difficulty", "0, 1 or 2"}},
		"future wipe":       {map[string]string{"LastWipe": "2999-01-01"}, []string{"LastWipe", "future"}},
		"key in other case": {map[string]string{"harvestrate": "2"}, []string{"harvestrate", "HarvestRate"}},
		"cut at a comma":    {map[string]string{"MOTD": "Hi,all"}, []string{"MOTD"}},
		"free rule cut":     {map[string]string{"SomethingNew": "any,thing)"}, []string{"SomethingNew", "no space"}},
	} {
		err := create("bad", c.rules)
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("%s: %v, want refused", what, err)
			continue
		}
		for _, s := range c.say {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: the refusal does not say %q: %v", what, s, err)
			}
		}
	}
	if len(r.m.List()) != 0 {
		t.Fatal("a refused instance was created")
	}
	words := strings.TrimSuffix(strings.Repeat("word,", 80), ",")
	if err := create("alpha", map[string]string{
		"HarvestRate": "2.5", "PvP": "True", "Difficulty": "2", "ChatFilter": "Drop", "LastWipe": "2026-01-01",
		"MOTD": "Welcome, traveller", "ChatBlockedWords": words, "AnnouncementsUrl": "https://example.com/news",
		"SomethingNew": "any, thing)", "Motto": "a value the catalog does not know",
	}); err != nil {
		t.Fatalf("good game settings and free rules: %v", err)
	}
	if len(words) <= 256 {
		t.Fatal("the list is meant to be longer than a plain rule may be")
	}
	if err := create("long", map[string]string{"SomethingNew": strings.Repeat("x", 257)}); !fault.Is(err, fault.Invalid) {
		t.Errorf("a plain rule longer than 256: %v", err)
	}

	// The catalog values reach the server as -Key=Value, a list whole.
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	m.Start(context.Background(), "alpha", false)
	spec, _, _ := sup.last(t)
	for _, want := range []string{"-HarvestRate=2.5", "-PvP=True", "-Difficulty=2", "-ChatFilter=Drop", "-LastWipe=2026-01-01",
		"-MOTD=Welcome, traveller", "-ChatBlockedWords=" + strings.ReplaceAll(words, ",", ", "),
		"-AnnouncementsUrl=https://example.com/news", "-SomethingNew=any, thing)", "-Motto=a value the catalog does not know"} {
		if !slices.Contains(spec.Args, want) {
			t.Errorf("%.60q is not on the command line", want)
		}
	}
}

// Rules stored before the catalog keep working: an instance loads with
// them, and a change leaves them as they are unless it changes them.
func TestStoredGameSettingsKeepWorking(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.Rules["HarvestRate"] = "5000"
	in.Rules["Difficulty"] = "hard"
	in.Rules["Motd"] = "hello"
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	m := r.newManager(&recordSup{})
	defer m.Close()
	v, err := m.Get("alpha")
	if err != nil || v.Rules["HarvestRate"] != "5000" {
		t.Fatalf("loaded: %+v %v", v.Rules, err)
	}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}); err != nil {
		t.Fatalf("a change of something else: %v", err)
	}
	same := v.Rules
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &same}}); err != nil {
		t.Fatalf("the same rules again: %v", err)
	}
	more := map[string]string{"HarvestRate": "5000", "Difficulty": "hard", "Motd": "hello", "PvP": "False"}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &more}}); err != nil {
		t.Fatalf("a rule added beside the stored ones: %v", err)
	}
	changed := map[string]string{"HarvestRate": "6000", "Difficulty": "hard", "Motd": "hello"}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &changed}}); !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "HarvestRate") {
		t.Fatalf("a changed value out of range: %v", err)
	}
	if got, _ := m.Get("alpha"); got.Rules["HarvestRate"] != "5000" || got.Rules["PvP"] != "False" {
		t.Fatalf("after the refused change: %+v", got.Rules)
	}
}
