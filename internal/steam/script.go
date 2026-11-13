package steam

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Request is one SteamCMD run: download (or bring up to date) an app's
// branch into a folder.
type Request struct {
	InstallDir string // absolute
	AppID      string
	Branch     string // "" for the default branch
	Password   string // the branch password, "" for none
	// Validate makes SteamCMD check every local file against Steam (slow
	// for a big app; used when a person asks for an install).
	Validate bool
}

var (
	appIDRe  = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// MaxPasswordLength is the longest branch password accepted.
const MaxPasswordLength = 128

// CheckPassword says what is wrong with a branch password, or nil. A
// password is 1 to MaxPasswordLength printable ASCII characters without
// spaces or quotes: SteamCMD reads its commands as words.
func CheckPassword(pw string) error {
	if pw == "" || len(pw) > MaxPasswordLength {
		return fmt.Errorf("a branch password has 1 to %d characters", MaxPasswordLength)
	}
	for _, r := range pw {
		if r <= ' ' || r > '~' || r == '"' || r == '\'' || r == '\\' {
			return errors.New("a branch password may only have printable ASCII characters without spaces, quotes or backslashes")
		}
	}
	return nil
}

// Script returns the SteamCMD script for r. It is run with
// "+runscript <file>", so the password never appears on a command line;
// the file must be private (0600 in a 0700 folder) and removed after the
// run.
//
// UNCONFIRMED on a real host: SteamCMD reads a double-quoted
// force_install_dir path with spaces in it as one word.
func Script(r Request) ([]byte, error) {
	if !filepath.IsAbs(r.InstallDir) || strings.ContainsAny(r.InstallDir, "\"\r\n") ||
		strings.ContainsFunc(r.InstallDir, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return nil, errors.New("the install folder must be an absolute path without quotes or control characters")
	}
	if !appIDRe.MatchString(r.AppID) {
		return nil, errors.New("the app id must be digits")
	}
	if r.Branch != "" && !branchRe.MatchString(r.Branch) {
		return nil, errors.New("the branch may only have letters, digits, \"_\", \".\" and \"-\"")
	}
	update := "app_update " + r.AppID
	if r.Branch != "" {
		update += " -beta " + r.Branch
		if r.Password != "" {
			if err := CheckPassword(r.Password); err != nil {
				return nil, err
			}
			update += " -betapassword " + r.Password
		}
	} else if r.Password != "" {
		return nil, errors.New("a password needs a branch")
	}
	if r.Validate {
		update += " validate"
	}
	lines := []string{
		"@ShutdownOnFailedCommand 1",
		"@NoPromptForPassword 1",
		`force_install_dir "` + r.InstallDir + `"`,
		"login anonymous",
		update,
		"quit",
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
