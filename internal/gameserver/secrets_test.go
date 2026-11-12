package gameserver

import (
	"strings"
	"testing"
)

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
	}
	for name, word := range secret {
		why, ok := SecretLike(name)
		if !ok || !strings.Contains(why, `"`+word+`"`) {
			t.Errorf("SecretLike(%q) = %q, %v; want a refusal that names %q", name, why, ok, word)
		}
	}
	for _, name := range []string{
		"Difficulty", "PvP", "DayLength", "BuildingDecay", "MaxStructures", "Motd", "KeepInventory",
		"Keyboard", "KeyboardLayout", "KeyBindings", "Keys", "MonkeysAllowed", "Passive", "Compass",
		"BypassLimit", "AuthorName", "Tokyo", "SecureMode", "Pwned", "ShowHud",
		"FakeReadyDelay", "FakeSaveInterval", "FakeCrashAfter", "FakeIgnoreStop", "FakeNoReady",
		"FakeNoStatusFile", "FakeNoQuery", "FakeFailStart", "FakeExitCode",
	} {
		if why, ok := SecretLike(name); ok {
			t.Errorf("SecretLike(%q) refuses an ordinary name: %s", name, why)
		}
	}
}
