package instances

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// The server's engine stops at once with an en dash (U+2013) anywhere on
// its command line. No change stores one any more, but an instance stored
// by an earlier version may hold one: "Alpha - EU" as its display name,
// with an en dash for the "-", say. ESM does not start such a server. The
// start is refused with 409 command_line_unsafe, which names the setting
// and repeats nothing of its value, and the supervisor is never asked. The
// instance keeps its warning unsafe_stored_settings, and once the setting
// is changed it starts.
func TestStoredEnDashIsNotStarted(t *testing.T) {
	dash := string(rune(0x2013))
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	var stored Instance
	if err := r.st.ReadJSON(instFile("alpha"), &stored); err != nil {
		t.Fatal(err)
	}
	// pieces are the parts of the stored values; no answer may hold one.
	pieces := []string{"Zq7", "Wx4", "Jx9", "Kv2", "Tm5", "Rb8", "Pw3", "Yn6", "Hd1"}
	cases := []struct {
		setting string
		store   func(in *Instance, s *Secrets)
	}{
		{"display_name", func(in *Instance, _ *Secrets) { in.DisplayName = "Zq7 " + dash + " Wx4" }},
		{"rules.Motto", func(in *Instance, _ *Secrets) { in.Rules["Motto"] = "Jx9" + dash + "Kv2" }},
		{"extra_args.2", func(in *Instance, _ *Secrets) { in.ExtraArgs = []string{"-NoSteam", "-Motto=Tm5 " + dash + " Rb8"} }},
		{"admin_password", func(in *Instance, s *Secrets) {
			in.SecretDelivery = gameserver.DeliveryCommandLine
			s.AdminPassword = "Pw3" + dash + "Yn6Hd1"
		}},
	}
	for _, c := range cases {
		in := stored
		in.Rules = maps.Clone(stored.Rules)
		in.ExtraArgs = nil
		var sec Secrets
		c.store(&in, &sec)
		if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := r.st.WriteJSON(secretsFile("alpha"), sec, 0o600); err != nil {
			t.Fatal(err)
		}
		sup := &recordSup{}
		m := r.newManager(sup)

		v, err := m.Get("alpha")
		if err != nil {
			t.Fatalf("%s: %v", c.setting, err)
		}
		if !slices.Contains(v.Warnings, WarnUnsafeStoredSettings) || !slices.Contains(storedUnsafe(t, v), c.setting) {
			t.Errorf("%s: warnings %q, settings named %q", c.setting, v.Warnings, storedUnsafe(t, v))
		}
		refused := func(what string, err error) {
			t.Helper()
			if fault.CodeOf(err) != "command_line_unsafe" || !fault.Is(err, fault.Conflict) {
				t.Errorf("%s, %s: %v, want 409 command_line_unsafe", c.setting, what, err)
				return
			}
			msg := err.Error()
			if !strings.Contains(msg, c.setting) || !strings.Contains(msg, "en dash") || !strings.Contains(msg, "U+2013") {
				t.Errorf("%s, %s: the refusal does not say what and where: %v", c.setting, what, err)
			}
			if strings.Contains(msg, dash) {
				t.Errorf("%s, %s: the refusal holds the en dash itself: %v", c.setting, what, err)
			}
			for _, p := range pieces {
				if strings.Contains(msg, p) {
					t.Errorf("%s, %s: the refusal repeats %q of a value: %v", c.setting, what, p, err)
				}
			}
		}
		_, err = m.Start(context.Background(), "alpha", false)
		refused("start", err)
		_, err = m.Start(context.Background(), "alpha", true)
		refused("start and wait", err)
		_, err = m.Restart(context.Background(), "alpha", "", false)
		refused("restart", err)
		sup.mu.Lock()
		asked := len(sup.specs)
		sup.mu.Unlock()
		if asked != 0 {
			t.Errorf("%s: the supervisor was asked %d times to start a server whose engine stops at once", c.setting, asked)
		}
		if st, _ := m.Status("alpha"); st.State.active() {
			t.Errorf("%s: alpha is %s", c.setting, st.State)
		}
		if _, ok := r.runSecrets("alpha"); ok {
			t.Errorf("%s: a start that did not happen left a secrets file", c.setting)
		}
		m.Close()
	}

	// Changed, the setting no longer stops the start.
	in := stored
	in.DisplayName = "Zq7 " + dash + " Wx4"
	if err := r.st.WriteJSON(instFile("alpha"), in, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteJSON(secretsFile("alpha"), Secrets{}, 0o600); err != nil {
		t.Fatal(err)
	}
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	if _, err := m.Start(context.Background(), "alpha", false); fault.CodeOf(err) != "command_line_unsafe" {
		t.Fatalf("start with the stored en dash: %v", err)
	}
	v, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Zq7 - Wx4")}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(v.Warnings, WarnUnsafeStoredSettings) {
		t.Errorf("after the change: warnings %q, settings named %q", v.Warnings, storedUnsafe(t, v))
	}
	m.Start(context.Background(), "alpha", false)
	spec, _, _ := sup.last(t)
	if why := cmdline.Refuses(cmdline.Join(spec.Args)); why != "" {
		t.Fatalf("the line after the change: %s", why)
	}
	if !slices.Contains(spec.Args, gameserver.ServerNameSwitch+"=Zq7 - Wx4") {
		t.Errorf("the server name is not the changed one: %q", spec.Args)
	}
}

// What holds an en dash is named by where it is on the line: a setting by
// its field, ESM's own paths as the folders of the instance. Something the
// server is not given (an extra argument that ends in "=", a secret with
// the secrets file) is not named, and a start is refused exactly when the
// line ESM passes holds an en dash.
func TestEnDashIsNamedWhereItIs(t *testing.T) {
	dash := string(rune(0x2013))
	base := func() (Instance, gameserver.Launch) {
		cfg := Instance{Name: "alpha", Build: "1.0", World: "alpha"}
		l := gameserver.Launch{Map: "TestMap", GamePort: 7777, QueryPort: 27015, MaxPlayers: 10, ServerName: "alpha", WorldName: "alpha",
			Rules: map[string]string{"HarvestRate": "2"}, UserDir: "/srv/esm/instances/alpha/data", LogFile: "/srv/esm/instances/alpha/logs/server.log",
			Delivery: gameserver.DeliveryFile, SecretsFile: "/srv/esm/instances/alpha/data/secrets.json"}
		return cfg, l
	}
	for _, c := range []struct {
		what string
		set  func(*Instance, *gameserver.Launch)
		want []string
	}{
		{"nothing", func(*Instance, *gameserver.Launch) {}, nil},
		{"display name", func(c *Instance, l *gameserver.Launch) {
			c.DisplayName = "A" + dash + "B"
			l.ServerName = c.DisplayName
		}, []string{"display_name"}},
		{"rule", func(_ *Instance, l *gameserver.Launch) { l.Rules["Motto"] = "a " + dash + " b" }, []string{"rules.Motto"}},
		{"extra argument", func(_ *Instance, l *gameserver.Launch) { l.ExtraArgs = []string{"-A=1", "-B=x" + dash + "y"} }, []string{"extra_args.2"}},
		{"extra argument left off", func(_ *Instance, l *gameserver.Launch) { l.ExtraArgs = []string{"-B=x" + dash + "y="} }, nil},
		{"secret on the line", func(_ *Instance, l *gameserver.Launch) {
			l.Delivery, l.Secrets = gameserver.DeliveryCommandLine, gameserver.Secrets{ProblemReportToken: "t" + dash + "k"}
		}, []string{"problem_report_token"}},
		{"secret in the file", func(_ *Instance, l *gameserver.Launch) {
			l.Secrets = gameserver.Secrets{ServerPassword: "p" + dash + "w"}
		}, nil},
		{"root", func(_ *Instance, l *gameserver.Launch) {
			l.UserDir, l.LogFile = "/srv/e"+dash+"m/instances/alpha/data", "/srv/e"+dash+"m/instances/alpha/logs/server.log"
		}, []string{"the folders of the instance (the path of ESM's root)"}},
		{"secrets file only", func(_ *Instance, l *gameserver.Launch) { l.SecretsFile = "/srv/e" + dash + "m/secrets.json" },
			[]string{"the folders of the instance (the path of ESM's root)"}},
		{"secrets file not passed", func(_ *Instance, l *gameserver.Launch) {
			l.Delivery, l.SecretsFile = gameserver.DeliveryCommandLine, "/srv/e"+dash+"m/secrets.json"
		}, nil},
		{"default map", func(_ *Instance, l *gameserver.Launch) { l.Map = "Test" + dash + "Map" }, []string{"the default map of build 1.0"}},
		{"map", func(c *Instance, l *gameserver.Launch) { c.Map = "Test" + dash + "Map"; l.Map = c.Map }, []string{"map"}},
	} {
		cfg, l := base()
		c.set(&cfg, &l)
		got := enDashIn(cfg, l)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: named %q, want %q", c.what, got, c.want)
		}
		if onLine := slices.ContainsFunc(l.Args(), gameserver.HasEnDash); onLine != (len(got) > 0) {
			t.Errorf("%s: an en dash on the line: %v, named: %q", c.what, onLine, got)
		}
	}
}
