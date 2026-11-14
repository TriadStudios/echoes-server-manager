package instances

import (
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// No value may bring a switch of its own onto the server command line: no
// value holds "=", a value without a space, which the server reads
// unquoted, holds no "," or ")" either, and a rule key or the switch name
// of an extra argument is ASCII letters and digits only. This is checked
// when a value is set; one stored earlier keeps loading. A value with a
// space is quoted, but a name and "=" inside it is still found by some of
// the engine's code, so "abc Password=x" is refused like "a;Password=x".
func TestSwitchesCannotHideInValues(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	const hidden = "Zq7-hidden-9"
	for what, s := range map[string]Settings{
		"rule value":          {Rules: &map[string]string{"Foo": "-iniFile=/x" + hidden}},
		"rule value password": {Rules: &map[string]string{"Foo": "a;Password=" + hidden}},
		"rule key":            {Rules: &map[string]string{"x_PvP": "True"}},
		"extra value":         {ExtraArgs: &[]string{"-Foo=-iniFile=/x" + hidden}},
		"extra value colon":   {ExtraArgs: &[]string{"-Foo:-Password=" + hidden}},
		"extra name":          {ExtraArgs: &[]string{"-x_MaxPlayers=5"}},
		"extra custom config": {ExtraArgs: &[]string{"-CustomConfig=Server"}},
		"display name":        {DisplayName: ptr("Alpha,PvP=True")},
		"game setting url":    {Rules: &map[string]string{"AnnouncementsUrl": "https://example.com/news?AdminPassword=" + hidden}},
		"same switch twice":   {Rules: &map[string]string{"Mode": "a"}, ExtraArgs: &[]string{"-mode=b"}},
		"map that is a name":  {Map: ptr("/PvP")},
	} {
		s.BindIP = ptr("127.0.0.1")
		_, err := r.m.Create(CreateRequest{Name: "bad", Settings: s})
		if !fault.Is(err, fault.Invalid) || strings.Contains(err.Error(), hidden) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if len(r.m.List()) != 0 {
		t.Fatal("a refused instance was created")
	}
	if _, err := r.m.Create(CreateRequest{Name: "bad", Settings: Settings{BindIP: ptr("127.0.0.1"),
		Rules: &map[string]string{"Foo": "abc Password=" + hidden}}}); !fault.Is(err, fault.Invalid) || strings.Contains(err.Error(), hidden) {
		t.Errorf("a name and \"=\" in a value with a space: %v", err)
	}
	r.create("alpha", map[string]string{"Foo": "abc Password x", "Bar": "abcPassword"})
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{ExtraArgs: &[]string{"-PvP=True", "-Mode=Fast", "-NoSteam"},
		Map: ptr("/Game/Maps/PvP")}}); err != nil {
		t.Fatalf("an extra argument that is the switch itself: %v", err)
	}

	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.Rules["Foo"] = "-iniFile=/x"
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	m := r.newManager(&recordSup{})
	defer m.Close()
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}); err != nil {
		t.Fatalf("a stored value is refused at a change of something else: %v", err)
	}
}
