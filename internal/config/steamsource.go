package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
)

// DefaultSteamCheckEvery is how often a Steam source is checked for a new
// build when its check_every is not set.
const DefaultSteamCheckEvery = 15 * time.Minute

// SteamSource is a Steam app that ESM installs server builds from with
// SteamCMD: the app (a dedicated server tool app), optionally a beta branch
// (its password is an ESM secret, never in esm.json), where the server
// binary is inside the app's files for each system, the map a server loads
// when its instance names none, and the channel of the builds. Each new
// Steam build id becomes the ESM build steam-<name>-<buildid>.
type SteamSource struct {
	// Name names the source in ESM ("steam:<name>"): lowercase letters,
	// digits and "-", at most 24 characters.
	Name string `json:"name"`
	// AppID is the Steam app id (digits).
	AppID string `json:"app_id"`
	// Branch is the beta branch; empty for the app's default branch.
	Branch string `json:"branch,omitempty"`
	// Executable maps a system ("linux", "windows") to the server binary,
	// a slash path inside the app's files.
	Executable map[string]string `json:"executable"`
	// DefaultMap is written into each build's manifest (optional).
	DefaultMap string `json:"default_map,omitempty"`
	// Channel is the channel of the builds: live or test.
	Channel string `json:"channel"`
	// CheckEvery is how often ESM asks SteamCMD whether the branch has a new
	// build (and installs it): default 15m, "0" turns the checks off (builds
	// are then installed only on request).
	CheckEvery *Duration `json:"check_every,omitempty"`
}

var (
	steamNameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	steamBranchRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// ValidSteamSourceName reports whether name is a valid source name.
func ValidSteamSourceName(name string) bool {
	return steamNameRe.MatchString(name) && !strings.HasSuffix(name, "-")
}

// Every is the check interval: CheckEvery, or the default when unset.
func (s SteamSource) Every() time.Duration {
	if s.CheckEvery == nil {
		return DefaultSteamCheckEvery
	}
	return s.CheckEvery.D()
}

// Validate checks one source.
func (s SteamSource) Validate() error {
	if !ValidSteamSourceName(s.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and \"-\" (not first or last), at most 24 characters", s.Name)
	}
	if !steamAppIDRe.MatchString(s.AppID) {
		return fmt.Errorf("%s: app_id must be the app id's digits", s.Name)
	}
	if s.Branch != "" && !steamBranchRe.MatchString(s.Branch) {
		return fmt.Errorf("%s: branch may only have letters, digits, \"_\", \".\" and \"-\" (at most 64)", s.Name)
	}
	if len(s.Executable) == 0 {
		return fmt.Errorf("%s: executable must name the server binary for at least one system, like {\"linux\": \"Server/Binaries/Linux/Server\"}", s.Name)
	}
	for _, goos := range slices.Sorted(maps.Keys(s.Executable)) {
		if goos != "linux" && goos != "windows" {
			return fmt.Errorf("%s: executable: %q is not a system ESM runs servers on (linux, windows)", s.Name, goos)
		}
		rel := s.Executable[goos]
		if clean, err := builds.CleanName(rel); err != nil || clean != rel || strings.HasPrefix(rel, "steamapps/") {
			return fmt.Errorf("%s: executable for %s must be a clean slash path inside the app's files", s.Name, goos)
		}
	}
	if s.DefaultMap != "" && !builds.ValidMap(s.DefaultMap) {
		return fmt.Errorf("%s: default_map may only have %s", s.Name, builds.MapRule)
	}
	if !builds.ValidChannel(s.Channel) {
		return fmt.Errorf("%s: channel must be live or test", s.Name)
	}
	if s.CheckEvery != nil && (*s.CheckEvery < 0 || (*s.CheckEvery > 0 && s.CheckEvery.D() < time.Minute)) {
		return fmt.Errorf("%s: check_every must be 0 (off) or at least 1m", s.Name)
	}
	return nil
}

// SteamSource returns the source with this name.
func (c *Config) SteamSource(name string) (SteamSource, bool) {
	for _, s := range c.SteamSources {
		if s.Name == name {
			return s, true
		}
	}
	return SteamSource{}, false
}

// DefaultSteamCMDDir is the SteamCMD folder inside the root.
const DefaultSteamCMDDir = "steamcmd"

// SteamCMDPath returns the absolute path of SteamCMD (see SteamCMD).
func (c *Config) SteamCMDPath() string {
	p := c.SteamCMD
	if p == "" {
		name := "steamcmd.sh"
		if runtime.GOOS == "windows" {
			name = "steamcmd.exe"
		}
		p = filepath.Join(DefaultSteamCMDDir, name)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.Root, p)
	}
	return filepath.Clean(p)
}
