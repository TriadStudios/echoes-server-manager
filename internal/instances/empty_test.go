package instances

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A rule or an extra argument without a value is refused when it is set:
// the server would read the next argument as the value. One stored
// earlier keeps loading and is never passed as -Key=.
func TestEmptyValuesAreRefusedWhenSet(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	for what, s := range map[string]Settings{
		"empty free rule":    {Rules: &map[string]string{"Foo": ""}},
		"empty game setting": {Rules: &map[string]string{"MOTD": ""}},
		"empty list":         {Rules: &map[string]string{"ChatBlockedWords": " , "}},
		"argument ending =":  {ExtraArgs: &[]string{"-Bar="}},
	} {
		s.BindIP = ptr("127.0.0.1")
		_, err := r.m.Create(CreateRequest{Name: "bad", Settings: s})
		if !fault.Is(err, fault.Invalid) || !strings.Contains(err.Error(), "value") {
			t.Errorf("%s: %v", what, err)
		}
	}

	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.Rules["Foo"], in.Rules["MOTD"] = "", ""
	in.ExtraArgs = []string{"-Bar="}
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha")}}); err != nil {
		t.Fatalf("a change of something else: %v", err)
	}
	same := in.Rules
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &same, ExtraArgs: &[]string{"-Bar="}}}); err != nil {
		t.Fatalf("the stored values sent back unchanged: %v", err)
	}
	m.Start(context.Background(), "alpha", false)
	spec, _, _ := sup.last(t)
	for _, a := range spec.Args {
		if strings.HasSuffix(a, "=") || strings.HasPrefix(a, "-MOTD") {
			t.Errorf("%q is on the command line", a)
		}
	}
	if !slices.Contains(spec.Args, "-Foo") || !slices.Contains(spec.Args, "-Bar") {
		t.Errorf("the stored free rule and argument are not passed bare: %q", spec.Args)
	}
}
