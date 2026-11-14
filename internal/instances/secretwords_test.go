package instances

import (
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// repeated returns a part of secret, three characters or longer, that text
// repeats, without regard to case; "" when there is none.
func repeated(text, secret string) string {
	low, rs := strings.ToLower(text), []rune(strings.ToLower(secret))
	for i := 0; i+3 <= len(rs); i++ {
		if part := string(rs[i : i+3]); strings.Contains(low, part) {
			return part
		}
	}
	return ""
}

// A refusal of a secret says which secret and which rule, and nothing of
// what the secret holds: no three characters of it in a row. The secrets
// here are made of pieces no refusal has in its own words (Zq7Jx, Kv9Wy)
// around the one thing that breaks a rule. A secret that holds the word
// MULTIHOME is among them: that word is no reason to refuse a secret, and
// saying that a secret "names MULTIHOME" gave nine of its characters away.
func TestSecretRefusalsSayNothingOfTheSecret(t *testing.T) {
	_, m, _ := twoInstances(t)
	breaks := []string{`"`, "=", ",", ")", "\t", "\u00a0", "\u180e", "\u200b", "\u2013", "\ue000", " -Q", ";-Q", "-Q",
		"multihome", "MULTIHOME", "-MultiHome", "x;MULTIHOME"}
	var secrets []string
	for _, b := range breaks {
		secrets = append(secrets, "Zq7Jx"+b+"Kv9Wy", b+"Zq7JxKv9Wy", "Zq7JxKv9Wy"+b)
	}
	secrets = append(secrets, " Zq7JxKv9Wy", "Zq7JxKv9Wy ", "Zq7", strings.Repeat("Zq7JxKv9Wy", 30))
	clearAll := SecretsRequest{ServerPassword: ptr(""), AdminPassword: ptr(""), AnnouncementsToken: ptr(""), ProblemReportToken: ptr("")}
	check := func(what, field, secret string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		if !fault.Is(err, fault.Invalid) {
			t.Fatalf("%s %q: %v", what, secret, err)
		}
		if part := repeated(err.Error(), secret); part != "" {
			t.Errorf("%s: the refusal repeats %q of the secret %q: %v", what, part, secret, err)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%s: the refusal does not name %s: %v", what, field, err)
		}
	}
	for _, secret := range secrets {
		for field, req := range secretSlots(secret) {
			_, _, err := m.SetSecrets("line", req)
			check("set on the command line", field, secret, err)
			if _, _, err := m.SetSecrets("line", clearAll); err != nil {
				t.Fatal(err)
			}
			_, _, err = m.SetSecrets("file", req)
			check("set in the file", field, secret, err)
			if err != nil {
				continue
			}
			_, _, err = m.Patch("file", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryCommandLine)}})
			check("change to the command line", field, secret, err)
			if err == nil {
				if _, _, err := m.Patch("file", PatchRequest{Settings: Settings{SecretDelivery: ptr(gameserver.DeliveryFile)}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := m.SetSecrets("file", clearAll); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The word alone breaks no rule: such a password and token are taken,
	// in the file and on the command line.
	for _, secret := range []string{"Zq7JxmultihomeKv9Wy", "MULTIHOME-Zq7JxKv9Wy"} {
		for field, req := range secretSlots(secret) {
			for _, name := range []string{"file", "line"} {
				if _, _, err := m.SetSecrets(name, req); err != nil {
					t.Errorf("%s %q on %s: %v", field, secret, name, err)
				}
			}
		}
	}
	for _, name := range []string{"file", "line"} {
		if _, _, err := m.SetSecrets(name, clearAll); err != nil {
			t.Fatal(err)
		}
	}

	// A secret that holds a name the engine searches for is refused on the
	// command line in the same words whatever the name is, so the refusal
	// does not say which one it holds.
	words := map[string]string{}
	for _, name := range cmdline.Searched {
		if strings.Contains(name, "=") {
			continue
		}
		secret := "Zq7Jx" + name + "Kv9Wy"
		for field, req := range secretSlots(secret) {
			_, _, err := m.SetSecrets("line", req)
			if !fault.Is(err, fault.Invalid) {
				t.Errorf("%s with %q on the command line: accepted (%v)", field, name, err)
				continue
			}
			text := strings.ToLower(err.Error())
			if strings.Contains(text, name) || repeated(text, "Zq7Jx") != "" || repeated(text, "Kv9Wy") != "" {
				t.Errorf("%s: the refusal says what the secret holds: %v", field, err)
			}
			if words[field] == "" {
				words[field] = err.Error()
			} else if err.Error() != words[field] {
				t.Errorf("%s: the refusal differs with the name the secret holds:\n%s\n%s", field, err, words[field])
			}
		}
	}
	untouched(t, m)
}
