package gameserver

import "strings"

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
