package instances

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// storedUnsafe returns the settings the view of an instance names under the
// warning unsafe_stored_settings, as a client reads them.
func storedUnsafe(t *testing.T, v View) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		List []string `json:"unsafe_stored_settings"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.List
}

// An instance stored by an earlier version keeps loading and starting as
// before, but no stored value makes the server read something it does not
// show. An empty rule and an extra argument that ends in "=" are left off
// the command line: the server would read the next argument as their
// value, and a bare switch turns a setting of the game on. The service log
// says which, once per start. Any other stored value that breaks the rules
// for the command line, and with command-line delivery a secret that does,
// is passed as it is, and the instance carries the warning
// unsafe_stored_settings, which names the settings and never a value.
func TestStoredSettingsFromAnEarlierVersion(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var in Instance
	if err := r.st.ReadJSON(instFile("alpha"), &in); err != nil {
		t.Fatal(err)
	}
	in.Rules = map[string]string{"Foo": "", "MOTD": "", "ChatBlockedWords": " , ", "Banner": "a,b", "Mode": "x;LOG=", "HarvestRate": "2", "Note": "fine words"}
	in.ExtraArgs = []string{"-Bar=", "-PvP=", "-Keep=1,2", "-NoSteam"}
	in.SecretDelivery = gameserver.DeliveryCommandLine
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	const adminPW = "adm;Password=joinme"
	if err := r.st.WriteJSON(secretsFile("alpha"), Secrets{ServerPassword: "join me 7", AdminPassword: adminPW}, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()

	v, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rules.Banner", "rules.ChatBlockedWords", "rules.Foo", "rules.MOTD", "rules.Mode",
		"extra_args.1", "extra_args.2", "extra_args.3", "admin_password"}
	if !slices.Contains(v.Warnings, "unsafe_stored_settings") || !slices.Equal(storedUnsafe(t, v), want) {
		t.Errorf("warnings %q, settings named %q, want %q", v.Warnings, storedUnsafe(t, v), want)
	}
	if text := WarningText("unsafe_stored_settings"); text == "unsafe_stored_settings" || strings.Contains(text, "joinme") {
		t.Errorf("the warning in words: %q", text)
	}
	if words := WarningWords(WarnUnsafeStoredSettings, v.UnsafeStoredSettings); !strings.Contains(words, "rules.Banner, rules.ChatBlockedWords") ||
		!strings.Contains(words, "admin_password") || strings.Contains(words, "joinme") {
		t.Errorf("the warning with its settings: %q", words)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "joinme") || strings.Contains(string(raw), "join me") {
		t.Fatalf("the view shows a secret: %s", raw)
	}

	for start := 1; start <= 2; start++ {
		m.Start(context.Background(), "alpha", false)
		spec, _, _ := sup.last(t)
		for _, a := range spec.Args {
			for _, gone := range []string{"-Foo", "-MOTD", "-ChatBlockedWords", "-Bar", "-PvP"} {
				if a == gone || strings.HasPrefix(a, gone+"=") {
					t.Errorf("%q is on the command line %q", a, spec.Args)
				}
			}
		}
		for _, kept := range []string{"-Banner=a,b", "-Mode=x;LOG=", "-HarvestRate=2", "-Note=fine words", "-Keep=1,2", "-NoSteam",
			"-Password=join me 7", "-AdminPassword=" + adminPW} {
			if !slices.Contains(spec.Args, kept) {
				t.Errorf("%q is not passed as stored: %q", kept, spec.Args)
			}
		}
		line := cmdline.Join(spec.Args)
		if on, set := cmdline.Bool(line, "PvP"); set {
			t.Errorf("the server reads PvP as %v", on)
		}
		for _, n := range []string{"Foo", "Bar", "MOTD", "ChatBlockedWords"} {
			if v, ok := cmdline.Value(line, n); ok || cmdline.Param(line, n) {
				t.Errorf("the server finds %s (%q)", n, v)
			}
		}
		logged := r.log.String()
		if n := strings.Count(logged, "left off the server command line"); n != start {
			t.Errorf("after %d starts the service log says %d times what was left off:\n%s", start, n, logged)
		}
		for _, k := range []string{"Foo", "MOTD", "ChatBlockedWords"} {
			if !strings.Contains(logged, k) {
				t.Errorf("the service log does not name %s:\n%s", k, logged)
			}
		}
		if strings.Contains(logged, "joinme") || strings.Contains(logged, "join me") {
			t.Errorf("the service log has a secret:\n%s", logged)
		}
	}

	// A change of something else, and the stored values sent back as they
	// are, are accepted; a stored value changed is checked.
	same := in.Rules
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha"), Rules: &same, ExtraArgs: &in.ExtraArgs}}); err != nil {
		t.Fatalf("the stored values sent back with another change: %v", err)
	}
	changed := map[string]string{"Banner": "c,d"}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &changed}}); !fault.Is(err, fault.Invalid) {
		t.Fatalf("a stored value changed to another unsafe one: %v", err)
	}
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr("adm;Password=x2")}); !fault.Is(err, fault.Invalid) {
		t.Fatalf("a new admin password that cannot go on the command line: %v", err)
	}
	fixed := map[string]string{"Banner": "c, d", "Mode": "x", "HarvestRate": "2"}
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &fixed, ExtraArgs: &[]string{"-NoSteam", "-Keep=1"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SetSecrets("alpha", SecretsRequest{AdminPassword: ptr("adm1n-pw-ok")}); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Get("alpha"); slices.Contains(v.Warnings, "unsafe_stored_settings") || len(storedUnsafe(t, v)) != 0 {
		t.Fatalf("after the fix: warnings %q, settings named %q", v.Warnings, storedUnsafe(t, v))
	}
}
