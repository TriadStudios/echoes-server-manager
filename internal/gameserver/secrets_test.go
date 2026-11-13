package gameserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The secrets file is one JSON object on one line. What is written is read
// back exactly, whatever the passwords contain.
func TestSecretsFile(t *testing.T) {
	for _, s := range []Secrets{
		{},
		{ServerPassword: "join"},
		{AdminPassword: "admin only"},
		{ServerPassword: `sp ace "quote" 'single' \back $dollar <tag> & é 密 -Port=1`, AdminPassword: "  lead and trail  "},
		{ServerPassword: ReadyLine("x", "y", 1), AdminPassword: `{"server_password":"x"}`},
	} {
		b := s.File()
		if !json.Valid(b) || !bytes.HasSuffix(b, []byte("\n")) || bytes.Count(b, []byte("\n")) != 1 {
			t.Errorf("the file for %+v is not one line of JSON: %q", s, b)
		}
		got, err := ParseSecretsFile(b)
		if err != nil || got != s {
			t.Errorf("%+v was read back as %+v (%v)", s, got, err)
		}
	}
	if got := string(Secrets{ServerPassword: "a<b>&c", AdminPassword: "d"}.File()); got != `{"server_password":"a<b>&c","admin_password":"d"}`+"\n" {
		t.Errorf("file content %q", got)
	}
	if got := string(Secrets{}.File()); got != "{}\n" {
		t.Errorf("file content without passwords %q", got)
	}
	if got := (Secrets{ServerPassword: "s", AdminPassword: "a"}).Values(); !slices.Equal(got, []string{"s", "a"}) {
		t.Errorf("values %q", got)
	}
	if got := (Secrets{AdminPassword: "a"}).Values(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("values %q", got)
	}
	// What is not such an object is refused, and the refusal does not
	// repeat the content.
	for _, bad := range []string{`{"server_password":"x","other":"Zq7-value"}`, `["Zq7-value"]`, `Zq7-value`, `{"server_password":777123}`, ``} {
		_, err := ParseSecretsFile([]byte(bad))
		if err == nil || strings.Contains(err.Error(), "Zq7") || strings.Contains(err.Error(), "777123") {
			t.Errorf("ParseSecretsFile(%q): %v", bad, err)
		}
	}
	path := filepath.Join(t.TempDir(), SecretsFileName)
	want := Secrets{ServerPassword: "from disk", AdminPassword: "too"}
	if err := os.WriteFile(path, want.File(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSecretsFile(path); err != nil || got != want {
		t.Fatalf("read %+v %v", got, err)
	}
	if _, err := ReadSecretsFile(path + ".missing"); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
	if !ValidDelivery(DeliveryFile) || !ValidDelivery(DeliveryCommandLine) || ValidDelivery("") || ValidDelivery("env") {
		t.Fatal("delivery names")
	}
}

// Names that look like they hold a secret are told apart from ordinary
// setting names.
func TestSecretLike(t *testing.T) {
	secret := map[string]string{
		"Password":           "password",
		"ServerPassword":     "password",
		"JoinPassword":       "password",
		"joinpassword":       "password",
		"JOINPASSWORD":       "password",
		"admin_password":     "password",
		"Rcon.Password":      "password",
		"Rcon-Pass-Word":     "password",
		"PasswordRequired":   "password",
		"RconPasswd":         "passwd",
		"Rcon_Pwd":           "pwd",
		"PassPhrase":         "passphrase",
		"Secret":             "secret",
		"ClientSecret":       "secret",
		"SecretsFile":        "secret",
		"SessionSecretValue": "secret",
		"Token":              "token",
		"AuthToken":          "token",
		"auth_token":         "token",
		"TokenForLogin":      "token",
		"ApiKey":             "apikey",
		"API_KEY":            "apikey",
		"api-key":            "apikey",
		"ApiKeyFile":         "apikey",
		"PrivateKey":         "privatekey",
		"private_key":        "privatekey",
		"Credentials":        "credential",
		"RconKey":            "key",
		"AdminKey":           "key",
		"rconkey":            "key",
		"LICENSE_KEY":        "key",
		"Key":                "key",
		"rconpw":             "pw",
		"Pass":               "pass",
		"Passw0rd":           "pass",
		"ServerPass":         "pass",
		"SERVER_PASS":        "pass",
		"server-pass":        "pass",
		"RCONPassCode":       "pass",
		"Passive":            "pass", // ambiguous, refused on purpose
		"RconPw":             "pw",
		"AdminPW":            "pw",
		"PWReset":            "pw",
		"rcon_pw":            "pw",
		"RconPw2":            "pw",
		"PW":                 "pw",
		"AdminPаssword":      "ASCII", // a Cyrillic letter in it
		"Ünlock":             "ASCII",
	}
	for name, word := range secret {
		why, ok := SecretLike(name)
		if !ok || !(strings.Contains(why, `"`+word+`"`) || word == "ASCII" && strings.Contains(why, word)) {
			t.Errorf("SecretLike(%q) = %q, %v; want a refusal that names %q", name, why, ok, word)
		}
	}
	for _, name := range []string{
		"Difficulty", "PvP", "DayLength", "BuildingDecay", "MaxStructures", "Motd", "KeepInventory",
		"Keyboard", "KeyboardLayout", "KeyBindings", "Keys", "MonkeysAllowed", "Compass", "ShowCompass",
		"BypassLimit", "AuthorName", "Tokyo", "SecureMode", "Pwned", "ShowHud", "PwnedCount", "Overpass2",
		"FakeReadyDelay", "FakeSaveInterval", "FakeCrashAfter", "FakeIgnoreStop", "FakeNoReady",
		"FakeNoStatusFile", "FakeNoQuery", "FakeFailStart", "FakeExitCode",
	} {
		if why, ok := SecretLike(name); ok {
			t.Errorf("SecretLike(%q) refuses an ordinary name: %s", name, why)
		}
	}
}
