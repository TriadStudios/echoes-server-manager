package gameserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
)

// Ways to hand a server its passwords (and its tokens: see Secrets).
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

// The switches that carry the announcements and problem report tokens
// with DeliveryCommandLine: the server's own (see GameSettings).
const (
	AnnouncementsTokenSwitch = "-AnnouncementsToken"
	ProblemReportTokenSwitch = "-ProblemReportToken"
)

// Secrets are the passwords and tokens a server is started with, and the
// content of the secrets file: one JSON object with the members
// "server_password", "admin_password", "announcements_token" and
// "problem_report_token", each a string and each left out when it is not
// set. The file is UTF-8 and ends with a line end. UNCONFIRMED, see
// SecretDelivery.
type Secrets struct {
	ServerPassword     string `json:"server_password,omitempty"`
	AdminPassword      string `json:"admin_password,omitempty"`
	AnnouncementsToken string `json:"announcements_token,omitempty"`
	ProblemReportToken string `json:"problem_report_token,omitempty"`
}

// Values returns the passwords and tokens that are set.
func (s Secrets) Values() []string {
	var out []string
	for _, v := range []string{s.ServerPassword, s.AdminPassword, s.AnnouncementsToken, s.ProblemReportToken} {
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

// secretEndings make a name look like a secret when the name ends with one
// (RconKey, AdminKey, ApiKey; rconpw).
var secretEndings = []string{"key", "pw"}

// SecretLike reports whether name, a rule key or the name of a switch,
// looks like it names a secret, and says why in a few words. Rules and
// extra arguments are shown to every role and go on the server command
// line, so ESM refuses such names there; the server and admin passwords
// have their own place.
//
// A name with any character that is not plain ASCII is refused: a letter
// from another script can stand in for one of the words below.
// Otherwise the name is compared without case and without "_", "-" and
// ".". It looks like a secret when it contains "password", "passwd",
// "passphrase", "pwd", "secret", "token", "apikey", "privatekey" or
// "credential", when it ends with "key" or "pw", or when one of its words
// starts with "pass" or is "pw". Words are split at anything but letters
// and digits, at a change from lower to upper case, between letters and
// digits, and before the last capital of a run of capitals that is
// followed by a small letter: "ServerPass", "SERVER_PASS", "Passw0rd",
// "RconPw" and "AdminPW" are refused, "Compass" and "BypassLimit" are not.
// Ambiguous words are refused too: "Passive" starts with "pass". A name
// written in one case without separators has no words to split, so
// "serverpass" passes (unlike "serverpassword" or "rconpw").
// UNCONFIRMED: which switches of the real server take secrets is not known;
// names that are refused wrongly get an exception here once it is.
func SecretLike(name string) (why string, ok bool) {
	for _, r := range name {
		if r > unicode.MaxASCII {
			return "has a character that is not plain ASCII", true
		}
	}
	n := strings.ToLower(name)
	n = strings.NewReplacer("_", "", "-", "", ".", "").Replace(n)
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return `contains "` + w + `"`, true
		}
	}
	for _, e := range secretEndings {
		if strings.HasSuffix(n, e) {
			return `ends with "` + e + `"`, true
		}
	}
	for _, w := range nameWords(name) {
		if strings.HasPrefix(w, "pass") {
			return `has a word starting with "pass"`, true
		}
		if w == "pw" {
			return `has the word "pw"`, true
		}
	}
	return "", false
}

// nameWords splits an ASCII name into its words, in lower case (see
// SecretLike).
func nameWords(name string) []string {
	lower := func(c byte) bool { return 'a' <= c && c <= 'z' }
	upper := func(c byte) bool { return 'A' <= c && c <= 'Z' }
	digit := func(c byte) bool { return '0' <= c && c <= '9' }
	var words []string
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !lower(c) && !upper(c) && !digit(c) {
			flush()
			continue
		}
		if len(cur) > 0 {
			p := cur[len(cur)-1]
			switch {
			case lower(p) && upper(c), digit(p) != digit(c):
				flush()
			case upper(p) && upper(c) && i+1 < len(name) && lower(name[i+1]):
				flush()
			}
		}
		cur = append(cur, c)
	}
	flush()
	return words
}
