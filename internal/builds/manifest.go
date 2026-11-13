package builds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// ManifestName is the manifest file at the top of every build.
const ManifestName = "manifest.json"

// Channels.
const (
	ChannelLive = "live"
	ChannelTest = "test"
)

// Manifest is the content of manifest.json. Every file of the build except
// the manifest itself is listed in Files with its SHA-256; the server
// binary to run on each operating system is named in Executables (keys are
// Go OS names: "linux", "windows"; values are slash paths inside the build,
// wherever the game keeps the binary). The server runs with the build's
// top folder as its working folder. DefaultMap is the map a server of this
// build loads when its instance names none (optional).
type Manifest struct {
	Version     string            `json:"version"`
	Commit      string            `json:"commit"`
	Channel     string            `json:"channel"`
	DefaultMap  string            `json:"default_map,omitempty"`
	Executables map[string]string `json:"executables"`
	Files       map[string]string `json:"files"`
}

var (
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	mapRe     = regexp.MustCompile(`^[A-Za-z0-9_/][A-Za-z0-9_./-]{0,127}$`)
)

// ValidMap reports whether m is an acceptable map name: a plain name or an
// asset path of letters, digits, "_", ".", "/" and "-" (not first), up to
// 128 characters, without "..".
func ValidMap(m string) bool { return mapRe.MatchString(m) && !strings.Contains(m, "..") }

// MapRule describes ValidMap for messages.
const MapRule = `letters, digits, "_", ".", "/" and "-" (not first), at most 128 characters, and no ".."`

// ValidVersion reports whether v is an acceptable build version.
func ValidVersion(v string) bool { return versionRe.MatchString(v) && v != "." && v != ".." }

// ValidChannel reports whether c is "live" or "test".
func ValidChannel(c string) bool { return c == ChannelLive || c == ChannelTest }

// ValidSHA256 reports whether s is 64 lowercase hex characters.
func ValidSHA256(s string) bool { return sha256Re.MatchString(s) }

// ParseManifest decodes a manifest strictly (unknown fields are refused).
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fault.New(fault.Invalid, "bad_manifest", "manifest.json: %v", err)
	}
	return &m, nil
}

// Check validates the manifest for running on goos.
func (m *Manifest) Check(goos string) error {
	bad := func(format string, args ...any) error {
		return fault.New(fault.Invalid, "bad_manifest", "manifest.json: "+format, args...)
	}
	if !ValidVersion(m.Version) {
		return bad("version %q must match %s", m.Version, versionRe)
	}
	if !ValidChannel(m.Channel) {
		return bad("channel must be %q or %q, not %q", ChannelLive, ChannelTest, m.Channel)
	}
	if len(m.Commit) > 128 || strings.ContainsFunc(m.Commit, isControl) {
		return bad("commit must be at most 128 printable characters")
	}
	if m.DefaultMap != "" && !ValidMap(m.DefaultMap) {
		return bad("default_map may only have %s", MapRule)
	}
	if len(m.Files) == 0 {
		return bad("no files listed")
	}
	for rel, sum := range m.Files {
		clean, err := CleanName(rel)
		if err != nil || clean != rel {
			return bad("file name %q is not a clean relative path", rel)
		}
		if rel == ManifestName {
			return bad("the manifest must not list itself")
		}
		if !ValidSHA256(sum) {
			return bad("sha256 of %s must be 64 lowercase hex characters", rel)
		}
	}
	for goosKey, rel := range m.Executables {
		if _, ok := m.Files[rel]; !ok {
			return bad("executable %q for %s is not listed in files", rel, goosKey)
		}
	}
	if _, ok := m.Executables[goos]; !ok {
		return fault.New(fault.Invalid, "no_executable", "the build has no executable for %s", goos)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// CleanName checks an archive or manifest member name and returns it in
// clean slash form (a leading "./" and a trailing "/" are dropped). Names
// that are absolute, contain "..", backslashes, colons, NUL or control
// characters, empty or "." elements, or that are not local on this OS
// (such as Windows device names) are refused.
func CleanName(name string) (string, error) {
	n := strings.TrimPrefix(name, "./")
	n = strings.TrimSuffix(n, "/")
	switch {
	case n == "":
		return "", fmt.Errorf("empty name")
	case strings.ContainsFunc(n, isControl):
		return "", fmt.Errorf("name %q has control characters", name)
	case strings.ContainsAny(n, `\:`):
		return "", fmt.Errorf("name %q has a backslash or colon", name)
	case path.IsAbs(n):
		return "", fmt.Errorf("name %q is an absolute path", name)
	}
	for _, el := range strings.Split(n, "/") {
		switch el {
		case "..":
			return "", fmt.Errorf("name %q leaves the build folder", name)
		case "", ".":
			return "", fmt.Errorf("name %q has empty or '.' elements", name)
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(n)) {
		return "", fmt.Errorf("name %q is not a local path on this system", name)
	}
	return n, nil
}
