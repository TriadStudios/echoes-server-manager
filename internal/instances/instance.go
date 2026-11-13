// Package instances owns ESM's game server instances: their settings
// (instance.json), secrets (secrets.json), ports, life cycle (start, ready
// detection, stop, crash back-off), updates with rollback, backups and
// restores, scheduled restarts and backups, and log tails.
//
// Layout under the ESM root:
//
//	instances/<name>/instance.json  settings
//	instances/<name>/secrets.json   server and admin passwords (0600)
//	instances/<name>/data/          the server's data folder
//	instances/<name>/console.log    the server's stdout and stderr
//	instances/<name>/run/           pid file, state.json and, while a server
//	                                runs, the secrets file it was started
//	                                with (0600)
//	backups/<name>/<id>/            save files and meta.json
package instances

import (
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// Defaults for new instances.
const (
	DefaultMaxPlayers = 10
	DefaultBackupKeep = 10
	MaxMaxPlayers     = 200
	maxRules          = 64
	maxExtraArgs      = 32
	maxRestartTimes   = 24
	maxSecretLength   = 128
	// MinSecretLength is the shortest password accepted, in characters.
	MinSecretLength = 8
)

// Ports is the game and query port of an instance.
type Ports struct {
	Game  int `json:"game"`
	Query int `json:"query"`
}

// Schedule holds the daily restart times ("HH:MM", local time).
type Schedule struct {
	RestartAt []string `json:"restart_at"`
}

// BackupPolicy controls scheduled backups. Every 0 means no scheduled
// backups; Keep is how many backups of the instance are kept.
type BackupPolicy struct {
	Every config.Duration `json:"every"`
	Keep  int             `json:"keep"`
}

// Limits are resource limits. They are applied only by the systemd
// supervisor.
type Limits struct {
	MemoryMax       string `json:"memory_max,omitempty"` // like "8G"; K, M, G, T are powers of 1024
	CPUQuotaPercent int    `json:"cpu_quota_percent,omitempty"`
}

// Instance is the content of instance.json.
type Instance struct {
	Name           string            `json:"name"`
	DisplayName    string            `json:"display_name"`
	Cluster        string            `json:"cluster"`
	Channel        string            `json:"channel"`
	Build          string            `json:"build"`
	Map            string            `json:"map"`
	World          string            `json:"world"`
	Ports          Ports             `json:"ports"`
	BindIP         string            `json:"bind_ip"`
	MaxPlayers     int               `json:"max_players"`
	Rules          map[string]string `json:"rules"`
	Listed         bool              `json:"listed"`
	Autostart      bool              `json:"autostart"`
	RestartOnCrash bool              `json:"restart_on_crash"`
	Schedule       Schedule          `json:"schedule"`
	Backup         BackupPolicy      `json:"backup"`
	Limits         Limits            `json:"limits"`
	ExtraArgs      []string          `json:"extra_args"`
	// SecretDelivery is how the server gets its passwords: "file" (the
	// default: in a private file, nothing on the command line) or
	// "command_line" (as command-line switches, for server builds that
	// cannot read the file; unsafe on a shared box, see
	// gameserver.DeliveryCommandLine).
	SecretDelivery string    `json:"secret_delivery"`
	PreviousBuilds []string  `json:"previous_builds"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
}

// normalize replaces nil maps and slices with empty ones so the JSON form
// is always an object or array, never null.
func (in *Instance) normalize() {
	if in.Rules == nil {
		in.Rules = map[string]string{}
	}
	if in.ExtraArgs == nil {
		in.ExtraArgs = []string{}
	}
	if in.PreviousBuilds == nil {
		in.PreviousBuilds = []string{}
	}
	if in.Schedule.RestartAt == nil {
		in.Schedule.RestartAt = []string{}
	}
}

func (in Instance) clone() Instance {
	out := in
	out.Rules = maps.Clone(in.Rules)
	out.ExtraArgs = slices.Clone(in.ExtraArgs)
	out.PreviousBuilds = slices.Clone(in.PreviousBuilds)
	out.Schedule.RestartAt = slices.Clone(in.Schedule.RestartAt)
	out.normalize()
	return out
}

// Settings are the changeable fields of an instance. A nil field is left
// as it is (or takes its default on create).
type Settings struct {
	DisplayName    *string            `json:"display_name,omitempty"`
	Cluster        *string            `json:"cluster,omitempty"`
	Channel        *string            `json:"channel,omitempty"`
	Map            *string            `json:"map,omitempty"`
	World          *string            `json:"world,omitempty"`
	Ports          *Ports             `json:"ports,omitempty"`
	BindIP         *string            `json:"bind_ip,omitempty"`
	MaxPlayers     *int               `json:"max_players,omitempty"`
	Rules          *map[string]string `json:"rules,omitempty"`
	Listed         *bool              `json:"listed,omitempty"`
	Autostart      *bool              `json:"autostart,omitempty"`
	RestartOnCrash *bool              `json:"restart_on_crash,omitempty"`
	Schedule       *Schedule          `json:"schedule,omitempty"`
	Backup         *BackupPolicy      `json:"backup,omitempty"`
	Limits         *Limits            `json:"limits,omitempty"`
	ExtraArgs      *[]string          `json:"extra_args,omitempty"`
	SecretDelivery *string            `json:"secret_delivery,omitempty"`
}

// CreateRequest is the body of a create call. Ports left at 0 are picked
// from the configured ranges.
type CreateRequest struct {
	Name  string `json:"name"`
	Build string `json:"build,omitempty"` // empty: the newest installed build of the channel
	Settings
}

// PatchRequest is the body of a change call. The build is changed only by
// an update, so Build must stay empty.
type PatchRequest struct {
	Build *string `json:"build,omitempty"`
	Settings
}

// apply copies the given settings into in. It reports whether the ports or
// bind address were given.
func (s Settings) apply(in *Instance) (portsGiven bool) {
	if s.DisplayName != nil {
		in.DisplayName = *s.DisplayName
	}
	if s.Cluster != nil {
		in.Cluster = *s.Cluster
	}
	if s.Channel != nil {
		in.Channel = *s.Channel
	}
	if s.Map != nil {
		in.Map = *s.Map
	}
	if s.World != nil {
		in.World = *s.World
	}
	if s.Ports != nil {
		in.Ports = *s.Ports
		portsGiven = true
	}
	if s.BindIP != nil {
		in.BindIP = *s.BindIP
		portsGiven = true
	}
	if s.MaxPlayers != nil {
		in.MaxPlayers = *s.MaxPlayers
	}
	if s.Rules != nil {
		in.Rules = maps.Clone(*s.Rules)
	}
	if s.Listed != nil {
		in.Listed = *s.Listed
	}
	if s.Autostart != nil {
		in.Autostart = *s.Autostart
	}
	if s.RestartOnCrash != nil {
		in.RestartOnCrash = *s.RestartOnCrash
	}
	if s.Schedule != nil {
		in.Schedule = Schedule{RestartAt: slices.Clone(s.Schedule.RestartAt)}
	}
	if s.Backup != nil {
		in.Backup = *s.Backup
		if in.Backup.Keep == 0 {
			in.Backup.Keep = DefaultBackupKeep
		}
	}
	if s.Limits != nil {
		in.Limits = *s.Limits
	}
	if s.ExtraArgs != nil {
		in.ExtraArgs = slices.Clone(*s.ExtraArgs)
	}
	if s.SecretDelivery != nil {
		in.SecretDelivery = *s.SecretDelivery
	}
	in.normalize()
	return portsGiven
}

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	clusterRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	worldRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	ruleKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	hhmmRe    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	memRe     = regexp.MustCompile(`^([0-9]{1,15})([KMGT]?)$`)
)

// ValidName reports whether name is a valid instance name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func invalid(field, format string, args ...any) error {
	return fault.New(fault.Invalid, "invalid", field+": "+format, args...)
}

// Validate checks every field of an instance (except ports against other
// instances and the network, which the manager does).
func (in *Instance) Validate() error {
	if !ValidName(in.Name) {
		return invalid("name", "must match %s", nameRe)
	}
	if len(in.DisplayName) > 64 || hasControl(in.DisplayName) {
		return invalid("display_name", "must be at most 64 printable characters")
	}
	if in.Cluster != "" && !clusterRe.MatchString(in.Cluster) {
		return invalid("cluster", "must match %s", clusterRe)
	}
	if !builds.ValidChannel(in.Channel) {
		return invalid("channel", "must be live or test")
	}
	if !builds.ValidVersion(in.Build) {
		return invalid("build", "is not a valid build version")
	}
	if in.Map != "" && !builds.ValidMap(in.Map) {
		return invalid("map", "may only have %s", builds.MapRule)
	}
	if !worldRe.MatchString(in.World) {
		return invalid("world", "must match %s", worldRe)
	}
	if in.Ports.Game < 0 || in.Ports.Game > 65535 || in.Ports.Query < 0 || in.Ports.Query > 65535 {
		return invalid("ports", "must be between 1 and 65535")
	}
	if in.Ports.Game != 0 && in.Ports.Game == in.Ports.Query {
		return invalid("ports", "game and query port must differ")
	}
	if in.BindIP != "" && net.ParseIP(in.BindIP) == nil {
		return invalid("bind_ip", "%q is not an IP address", in.BindIP)
	}
	if in.MaxPlayers < 1 || in.MaxPlayers > MaxMaxPlayers {
		return invalid("max_players", "must be between 1 and %d", MaxMaxPlayers)
	}
	if len(in.Rules) > maxRules {
		return invalid("rules", "at most %d rules", maxRules)
	}
	for _, k := range slices.Sorted(maps.Keys(in.Rules)) {
		// A key that is not a plain name is not repeated: it may hold
		// anything.
		if !ruleKeyRe.MatchString(k) {
			return invalid("rules", "every key must match %s", ruleKeyRe)
		}
		if why, ok := gameserver.SecretLike(k); ok {
			return secretLike("rules", fmt.Sprintf("key %q", k), why, in.Name, "rules")
		}
		if why, refused := gameserver.RefusedSwitch(k); refused {
			return invalid("rules", "%q is refused: %s", k, why)
		}
		if gameserver.IsReservedSwitch(k) {
			return invalid("rules", "%q is set by ESM itself", k)
		}
		if v := in.Rules[k]; len(v) > 256 || hasControl(v) {
			return invalid("rules", "value of %s must be at most 256 printable characters", k)
		}
	}
	if len(in.Schedule.RestartAt) > maxRestartTimes {
		return invalid("schedule.restart_at", "at most %d times", maxRestartTimes)
	}
	seen := map[string]bool{}
	for _, t := range in.Schedule.RestartAt {
		if !hhmmRe.MatchString(t) {
			return invalid("schedule.restart_at", "%q is not a time like 04:30", t)
		}
		if seen[t] {
			return invalid("schedule.restart_at", "%s is listed twice", t)
		}
		seen[t] = true
	}
	if in.Backup.Every < 0 || (in.Backup.Every > 0 && in.Backup.Every.D() < time.Minute) {
		return invalid("backup.every", "must be 0 (off) or at least 1m")
	}
	if in.Backup.Keep < 1 || in.Backup.Keep > 1000 {
		return invalid("backup.keep", "must be between 1 and 1000")
	}
	if _, err := ParseMemory(in.Limits.MemoryMax); err != nil {
		return invalid("limits.memory_max", "%v", err)
	}
	if in.Limits.CPUQuotaPercent < 0 || in.Limits.CPUQuotaPercent > 100000 {
		return invalid("limits.cpu_quota_percent", "must be between 0 (no limit) and 100000")
	}
	if len(in.ExtraArgs) > maxExtraArgs {
		return invalid("extra_args", "at most %d arguments", maxExtraArgs)
	}
	for i, a := range in.ExtraArgs {
		if err := checkExtraArg(in.Name, i+1, a); err != nil {
			return err
		}
	}
	if !gameserver.ValidDelivery(in.SecretDelivery) {
		return invalid("secret_delivery", "must be %q or %q", gameserver.DeliveryFile, gameserver.DeliveryCommandLine)
	}
	for _, b := range in.PreviousBuilds {
		if !builds.ValidVersion(b) {
			return invalid("previous_builds", "%q is not a valid build version", b)
		}
	}
	return nil
}

// secretLike is the refusal of a rule key or an extra argument that looks
// like a secret. what names the offender without repeating any value.
func secretLike(field, what, why, instance, things string) error {
	return fault.New(fault.Invalid, "looks_like_secret",
		"%s: %s looks like a secret (its name %s). Passwords are set only with PUT /api/v1/instances/%s/secrets and are never shown; %s are shown to every role and go on the server command line",
		field, what, why, instance, things)
}

// checkExtraArg checks the extra argument at position pos (counted from 1).
// Its errors name the argument by position and never repeat it: an
// argument may hold a value that must not be shown.
func checkExtraArg(instance string, pos int, a string) error {
	if !strings.HasPrefix(a, "-") || len(a) < 2 {
		return invalid("extra_args", "argument %d must start with '-'", pos)
	}
	if len(a) > 256 || hasControl(a) {
		return invalid("extra_args", "argument %d must be at most 256 printable characters", pos)
	}
	key, _, _ := strings.Cut(a, "=")
	// The switch name is what follows the dashes up to "=", ":" or white
	// space ("-Name=value", "-Name:value" and "-Name value" forms); it must
	// be plain ASCII. Every plain run of characters before "=" is checked
	// for secret words too.
	name := strings.TrimLeft(key, "-")
	if i := strings.IndexFunc(name, func(r rune) bool { return r == ':' || unicode.IsSpace(r) }); i >= 0 {
		name = name[:i]
	}
	plain := strings.FieldsFunc(key, func(r rune) bool {
		return !(r == '_' || r == '.' || r == '-' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z')
	})
	for _, n := range append([]string{name}, plain...) {
		if why, ok := gameserver.SecretLike(n); ok {
			return secretLike("extra_args", fmt.Sprintf("argument %d", pos), why, instance, "extra arguments")
		}
	}
	if why, refused := gameserver.RefusedSwitch(name); refused {
		return invalid("extra_args", "argument %d is refused: %s", pos, why)
	}
	if gameserver.IsReservedSwitch(name) {
		return invalid("extra_args", "argument %d: %s is set by ESM itself", pos, "-"+name)
	}
	return nil
}

// ParseMemory parses a size like "512M" or "8G" (powers of 1024) into
// bytes. An empty string is 0 (no limit).
func ParseMemory(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	m := memRe.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size like 512M or 8G", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[m[2]]
	if n <= 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is out of range", s)
	}
	return n << shift, nil
}

// Secrets is the content of secrets.json (0600): the passwords the
// instance's server is given at its next start. Nothing else is kept: a
// server gets its passwords in a file, so no earlier value has to be
// remembered to keep it out of anything ESM shows.
type Secrets struct {
	ServerPassword string `json:"server_password,omitempty"`
	AdminPassword  string `json:"admin_password,omitempty"`
}

// forServer returns the passwords as the server is given them.
func (s Secrets) forServer() gameserver.Secrets {
	return gameserver.Secrets{ServerPassword: s.ServerPassword, AdminPassword: s.AdminPassword}
}

// change sets the passwords given in req.
func (s *Secrets) change(req SecretsRequest) {
	if req.ServerPassword != nil {
		s.ServerPassword = *req.ServerPassword
	}
	if req.AdminPassword != nil {
		s.AdminPassword = *req.AdminPassword
	}
}

// SecretsRequest changes secrets: nil leaves a value, "" clears it.
type SecretsRequest struct {
	ServerPassword *string `json:"server_password,omitempty"`
	AdminPassword  *string `json:"admin_password,omitempty"`
}

// SecretFlags says which secrets are set, without showing them.
type SecretFlags struct {
	ServerPasswordSet bool `json:"server_password_set"`
	AdminPasswordSet  bool `json:"admin_password_set"`
}

// checkSecret checks a new password ("" clears it). Its errors never
// repeat the value.
func checkSecret(field, v string) error {
	if len(v) > maxSecretLength || hasControl(v) {
		return invalid(field, "must be at most %d printable characters", maxSecretLength)
	}
	if v != "" && utf8.RuneCountInString(v) < MinSecretLength {
		return fault.New(fault.Invalid, "password_too_short", "%s: must be at least %d characters", field, MinSecretLength)
	}
	return nil
}

// checkPasswordsInSettings refuses passwords that occur, without regard to
// case, in a setting that is shown to every role or that the server may
// print: the name, display name, world, map, cluster, the rule keys and
// values, and the extra arguments. Output filters mask the passwords, so
// such a setting would show as masked and give the password away. The
// error names the password and the setting, never a value.
func checkPasswordsInSettings(in Instance, s Secrets) error {
	type text struct{ field, v string }
	texts := []text{{"name", in.Name}, {"display_name", in.DisplayName}, {"world", in.World}, {"map", in.Map}, {"cluster", in.Cluster}}
	for _, k := range slices.Sorted(maps.Keys(in.Rules)) {
		texts = append(texts, text{"rules", k}, text{"rules", in.Rules[k]})
	}
	for _, a := range in.ExtraArgs {
		texts = append(texts, text{"extra_args", a})
	}
	for _, pw := range []struct{ what, v string }{{"server password", s.ServerPassword}, {"admin password", s.AdminPassword}} {
		if pw.v == "" {
			continue
		}
		low := strings.ToLower(pw.v)
		for _, t := range texts {
			if strings.Contains(strings.ToLower(t.v), low) {
				return fault.New(fault.Invalid, "password_in_settings",
					"the %s occurs in %s: a password may not be part of a setting, which every role sees and the server may print; change the password or the setting",
					pw.what, t.field)
			}
		}
	}
	return nil
}
