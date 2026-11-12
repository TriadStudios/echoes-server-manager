package gameserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Ways to hand a server its passwords.
const (
	// DeliveryFile writes the passwords to the secrets file and points the
	// server at it with SecretsFileSwitch. Nothing secret is on the command
	// line.
	DeliveryFile = "file"
	// DeliveryCommandLine puts the passwords on the server command line
	// (ServerPasswordSwitch, AdminPasswordSwitch). Every account on the box
	// can read the command line of a running process, and the server writes
	// its command line into its own log, so this is UNSAFE ON A SHARED BOX.
	// It exists only for server builds that cannot read the secrets file.
	DeliveryCommandLine = "command_line"
)

// SecretDelivery is how a server gets its passwords unless its instance
// says otherwise.
//
// THE REAL SERVER DOES NOT SUPPORT THIS YET. Reading its passwords from a
// per-instance file is a planned item on the game side; until it ships, a
// real server ignores the secrets file and runs without passwords, and an
// instance that needs them has to be set to DeliveryCommandLine.
// UNCONFIRMED: the switch name (SecretsFileSwitch) and the file format
// (Secrets).
const SecretDelivery = DeliveryFile

// ValidDelivery reports whether d is one of the delivery ways.
func ValidDelivery(d string) bool { return d == DeliveryFile || d == DeliveryCommandLine }

// SecretsFileSwitch is the command-line switch that names the secrets file:
// -SecretsFile=<absolute path>. UNCONFIRMED, see SecretDelivery.
const SecretsFileSwitch = "-SecretsFile"

// SecretsFileName is the name ESM gives the secrets file of an instance.
const SecretsFileName = "server-secrets.json"

// The switches that carry the passwords with DeliveryCommandLine.
// UNCONFIRMED.
const (
	ServerPasswordSwitch = "-ServerPassword"
	AdminPasswordSwitch  = "-AdminPassword"
)

// Secrets are the passwords a server is started with, and the content of
// the secrets file: one JSON object with the members "server_password" and
// "admin_password", each a string and each left out when that password is
// not set. The file is UTF-8 and ends with a line end. UNCONFIRMED, see
// SecretDelivery.
type Secrets struct {
	ServerPassword string `json:"server_password,omitempty"`
	AdminPassword  string `json:"admin_password,omitempty"`
}

// Values returns the passwords that are set.
func (s Secrets) Values() []string {
	var out []string
	for _, v := range []string{s.ServerPassword, s.AdminPassword} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// File returns the content of the secrets file for s.
func (s Secrets) File() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Two strings always encode.
		panic(fmt.Sprintf("gameserver: encode secrets file: %v", err))
	}
	return b.Bytes()
}

// ParseSecretsFile reads the content of a secrets file. Unknown members
// are refused.
func ParseSecretsFile(content []byte) (Secrets, error) {
	var s Secrets
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		// The cause is not repeated: it may quote the content.
		return Secrets{}, fmt.Errorf("gameserver: the secrets file is not the JSON object it should be")
	}
	return s, nil
}

// ReadSecretsFile reads the secrets file at path.
func ReadSecretsFile(path string) (Secrets, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Secrets{}, err
	}
	return ParseSecretsFile(b)
}

// secretWords are the words that make a rule key or a switch name look like
// it names a secret, wherever they stand in the name.
var secretWords = []string{"password", "passwd", "passphrase", "pwd", "secret", "token", "apikey", "privatekey", "credential"}

// secretEnding makes a name look like a secret when the name ends with it
// (RconKey, AdminKey, ApiKey).
const secretEnding = "key"

// SecretLike reports whether name, a rule key or the name of a switch,
// looks like it names a secret, and says why in a few words. Rules and
// extra arguments are shown to every role and go on the server command
// line, so ESM refuses such names there; the server and admin passwords
// have their own place.
//
// The name is compared without case and without "_", "-" and ".". It looks
// like a secret when it contains "password", "passwd", "passphrase", "pwd",
// "secret", "token", "apikey", "privatekey" or "credential", or when it ends
// with "key".
// UNCONFIRMED: which switches of the real server take secrets is not known;
// names that are refused wrongly get an exception here once it is.
func SecretLike(name string) (why string, ok bool) {
	n := strings.ToLower(name)
	n = strings.NewReplacer("_", "", "-", "", ".", "").Replace(n)
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return `contains "` + w + `"`, true
		}
	}
	if strings.HasSuffix(n, secretEnding) {
		return `ends with "` + secretEnding + `"`, true
	}
	return "", false
}
