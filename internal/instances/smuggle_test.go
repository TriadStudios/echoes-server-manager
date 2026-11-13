package instances

import (
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// No value may hold a switch the server reads ("<not a letter or
// digit>Name="): the server would take it as that switch. This is checked
// when a value is set; one stored earlier keeps loading.
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
	r.create("alpha", map[string]string{"Foo": "abcPassword=x"})
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{ExtraArgs: &[]string{"-PvP=True", "-Mode=Fast"}}}); err != nil {
		t.Fatalf("an extra argument that is the switch itself: %v", err)
	}
	for _, tok := range []string{"abc;Password=x1", "tok,PvP=True1", "x-Port=7000aa"} {
		_, _, err := r.m.SetSecrets("alpha", SecretsRequest{ProblemReportToken: ptr(tok)})
		if !fault.Is(err, fault.Invalid) {
			t.Errorf("token %q: %v", tok, err)
		}
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
