package instances

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

const (
	testAnnounceToken = "ann0unce-Tkn-7f3a9"
	testReportToken   = "rep0rt-Tkn-c41d2"
)

// The announcements and problem report tokens are kept with the
// passwords: set with the secrets, never shown, never a rule. They reach
// the server the way the passwords do: in the secrets file, or with
// command-line delivery on the command line, where they are masked in what
// ESM shows like the passwords.
func TestTokensAreKeptWithThePasswords(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	sup := &recordSup{}
	m := r.newManager(sup)
	defer m.Close()
	secret := pieces(testAnnounceToken, testReportToken)

	flags, _, err := m.SetSecrets("alpha", SecretsRequest{AnnouncementsToken: ptr(testAnnounceToken), ProblemReportToken: ptr(testReportToken)})
	if err != nil || !flags.AnnouncementsTokenSet || !flags.ProblemReportTokenSet || flags.ServerPasswordSet {
		t.Fatalf("set the tokens: %+v %v", flags, err)
	}
	v, _ := m.Get("alpha")
	raw, _ := json.Marshal(v)
	noPieces(t, "instance view", string(raw), secret)
	if !v.Secrets.AnnouncementsTokenSet || !strings.Contains(string(raw), `"problem_report_token_set":true`) {
		t.Fatalf("the view does not say the tokens are set: %s", raw)
	}
	for _, key := range []string{"AnnouncementsToken", "ProblemReportToken"} {
		_, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{Rules: &map[string]string{key: "something-else"}}})
		if fault.CodeOf(err) != "looks_like_secret" {
			t.Errorf("rule %s: %v, want looks_like_secret", key, err)
		}
	}

	// File delivery: in the secrets file, nowhere on the command line.
	m.Start(context.Background(), "alpha", false)
	spec, file, _ := sup.last(t)
	for _, a := range spec.Args {
		noPieces(t, "command line", a, secret)
		if strings.Contains(strings.ToLower(a), "token") {
			t.Errorf("a token switch is on the command line: %q", a)
		}
	}
	if want := (gameserver.Secrets{AnnouncementsToken: testAnnounceToken, ProblemReportToken: testReportToken}).File(); string(file) != string(want) {
		t.Fatalf("the secrets file: %q, want %q", file, want)
	}

	// Command-line delivery: on the command line, and masked in the error
	// of a supervisor that repeats it.
	if _, _, err := m.Patch("alpha", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}}); err != nil {
		t.Fatal(err)
	}
	_, err = m.Start(context.Background(), "alpha", false)
	if fault.CodeOf(err) != "start_failed" {
		t.Fatalf("start: %v", err)
	}
	noPieces(t, "start error", err.Error(), secret)
	spec, _, _ = sup.last(t)
	for _, want := range []string{"-AnnouncementsToken=" + testAnnounceToken, "-ProblemReportToken=" + testReportToken} {
		if !slices.Contains(spec.Args, want) {
			t.Errorf("%q is not on the command line", want)
		}
	}
	if !slices.Contains(spec.SecretValues, testAnnounceToken) || !slices.Contains(spec.SecretValues, testReportToken) {
		t.Error("the supervisor is not told to mask the tokens")
	}
	st, _ := m.Status("alpha")
	noPieces(t, "state reason", st.Reason, secret)
	evs, _ := r.bus.Since(0, events.Filter{})
	evRaw, _ := json.Marshal(evs)
	noPieces(t, "events", string(evRaw), secret)
	state, _ := r.st.ReadFile(stateFile("alpha"))
	noPieces(t, "state.json", string(state), secret)
	noPieces(t, "service log", r.log.String(), secret)
	inst, _ := r.st.ReadFile(instFile("alpha"))
	noPieces(t, "instance.json", string(inst), secret)

	// Cleared, a token is passed no more.
	if flags, _, err := m.SetSecrets("alpha", SecretsRequest{AnnouncementsToken: ptr("")}); err != nil || flags.AnnouncementsTokenSet || !flags.ProblemReportTokenSet {
		t.Fatalf("clear a token: %+v %v", flags, err)
	}
	m.Start(context.Background(), "alpha", false)
	spec, _, _ = sup.last(t)
	if slices.ContainsFunc(spec.Args, func(a string) bool { return strings.HasPrefix(a, "-AnnouncementsToken=") }) {
		t.Errorf("a cleared token is on the command line: %q", spec.Args)
	}
}

// A token is plain printable ASCII without white space, a double quote,
// "," or ")" (the server would end it there), of 8 to 256 characters, and
// may not occur in a setting. No refusal repeats it.
func TestTokenRules(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", map[string]string{"Banner": "welcome-Xy7"})
	for _, bad := range []string{"short-1", "with space-123", "comma,token-1", "paren)token-1", `quote"token-1`, "tab\ttoken-12", "nön-ascii-token", strings.Repeat("x", 257)} {
		for _, req := range []SecretsRequest{{AnnouncementsToken: ptr(bad)}, {ProblemReportToken: ptr(bad)}} {
			_, _, err := r.m.SetSecrets("alpha", req)
			if !fault.Is(err, fault.Invalid) {
				t.Errorf("token %q: %v, want refused", bad, err)
			} else if len(bad) > 3 && strings.Contains(err.Error(), bad) {
				t.Errorf("token %q: the refusal repeats it: %v", bad, err)
			}
		}
	}
	_, _, err := r.m.SetSecrets("alpha", SecretsRequest{AnnouncementsToken: ptr("WELCOME-xy7")})
	if fault.CodeOf(err) != "password_in_settings" || !strings.Contains(err.Error(), "announcements token") || strings.Contains(strings.ToLower(err.Error()), "welcome-xy7") {
		t.Errorf("a token in a rule: %v", err)
	}
	if _, _, err := r.m.SetSecrets("alpha", SecretsRequest{ProblemReportToken: ptr(testReportToken)}); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.m.Patch("alpha", PatchRequest{Settings: Settings{DisplayName: ptr("Alpha " + testReportToken)}})
	if fault.CodeOf(err) != "password_in_settings" || !strings.Contains(err.Error(), "problem report token") {
		t.Errorf("a setting that holds a token: %v", err)
	}
	if fl := r.m.secretFlags("alpha"); fl.AnnouncementsTokenSet || !fl.ProblemReportTokenSet {
		t.Fatalf("flags: %+v", fl)
	}
}
