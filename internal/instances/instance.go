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
	// MaxMaxPlayers is the largest max_players an instance may be stored
	// with (earlier versions allowed it). A new value is held to the game
	// server's limit, gameserver.MaxPlayersLimit (see checkChanges).
	MaxMaxPlayers = 200
	maxRules      = 64
	// maxRuleValue bounds a rule value; a list setting of the game's
	// catalog may be longer (gameserver.GameSetting.MaxLength).
	maxRuleValue    = 256
	maxExtraArgs    = 32
	maxRestartTimes = 24
	maxSecretLength = 128
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
	SecretDelivery string `json:"secret_delivery"`
	// AutoUpdate says what happens when the Steam source the instance's
	// build came from has a newer build: "off" (nothing; the newer build
	// shows as available), "install" (the same: the build is installed by
	// the source's checks, and the instance is flagged), or "restart" (the
	// instance is also updated, when its server has no players).
	AutoUpdate string `json:"auto_update"`
	// UpdateMaxWait is how long a "restart" update waits for an empty
	// server before ESM raises a warning (it keeps waiting).
	UpdateMaxWait  config.Duration `json:"update_max_wait"`
	PreviousBuilds []string        `json:"previous_builds"`
	Created        time.Time       `json:"created"`
	Updated        time.Time       `json:"updated"`
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
	if in.AutoUpdate == "" {
		in.AutoUpdate = AutoUpdateOff
	}
	if in.UpdateMaxWait == 0 {
		in.UpdateMaxWait = config.Duration(DefaultUpdateMaxWait)
	}
}

// Auto-update modes (Instance.AutoUpdate).
const (
	AutoUpdateOff     = "off"
	AutoUpdateInstall = "install"
	AutoUpdateRestart = "restart"
)

// DefaultUpdateMaxWait is the default of update_max_wait.
const DefaultUpdateMaxWait = 6 * time.Hour

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
	AutoUpdate     *string            `json:"auto_update,omitempty"`
	UpdateMaxWait  *config.Duration   `json:"update_max_wait,omitempty"`
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
	if s.AutoUpdate != nil {
		in.AutoUpdate = *s.AutoUpdate
	}
	if s.UpdateMaxWait != nil {
		in.UpdateMaxWait = *s.UpdateMaxWait
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

// unsafeArgument checks a setting that goes onto the server command line
// (see gameserver.UnsafeArgument). what names the setting without
// repeating its value: an extra argument may hold anything.
func unsafeArgument(field, what, value string) error {
	if why, bad := gameserver.UnsafeArgument(value); bad {
		return fault.New(fault.Invalid, "unsafe_argument", "%s: %s %s; the server reads its whole command line for switches", field, what, why)
	}
	return nil
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
	if err := unsafeArgument("display_name", "the display name", in.DisplayName); err != nil {
		return err
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
	if err := unsafeArgument("world", "the world name", in.World); err != nil {
		return err
	}
	if err := unsafeArgument("map", "the map", in.Map); err != nil {
		return err
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
		return invalid("max_players", "must be between 1 and %d, the most the game server takes (a value up to %d stored by an earlier version is kept until it is changed)", gameserver.MaxPlayersLimit, MaxMaxPlayers)
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
		if err := unsafeArgument("rules", fmt.Sprintf("key %q", k), k); err != nil {
			return err
		}
		limit := maxRuleValue
		if s, ok := gameserver.LookupGameSetting(k); ok && s.MaxLength > limit {
			limit = s.MaxLength
		}
		if v := in.Rules[k]; len(v) > limit || hasControl(v) {
			return invalid("rules", "value of %s must be at most %d printable characters", k, limit)
		}
		if err := unsafeArgument("rules", fmt.Sprintf("the value of %s", k), in.Rules[k]); err != nil {
			return err
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
	switch in.AutoUpdate {
	case AutoUpdateOff, AutoUpdateInstall, AutoUpdateRestart:
	default:
		return invalid("auto_update", "must be %q, %q or %q", AutoUpdateOff, AutoUpdateInstall, AutoUpdateRestart)
	}
	if d := in.UpdateMaxWait.D(); d < time.Minute || d > 30*24*time.Hour {
		return invalid("update_max_wait", "must be between 1m and 720h")
	}
	for _, b := range in.PreviousBuilds {
		if !builds.ValidVersion(b) {
			return invalid("previous_builds", "%q is not a valid build version", b)
		}
	}
	return nil
}

// checkChanges checks what a create or a change sets, beyond Validate.
// Only what is new or changed against old is checked (everything when old
// is nil), so a value stored by an earlier version keeps working until it
// is changed; the instance then carries the warning unsafe_stored_settings
// (see storedUnsafe). now is the time of the check (a date may not lie
// after it).
//
// The server joins its arguments into one line and finds its switches
// anywhere in it (see gameserver.CheckValue), so everything an operator
// sets that goes onto its command line follows one set of rules: the
// display name, the world name, the map, every rule value and the value
// of every extra argument pass gameserver.CheckValue. A rule key, and the
// switch name of an extra argument ("-Name" or "-Name=Value"), is ASCII
// letters and digits only (gameserver.PlainSwitchName), does not hold a
// switch the engine searches for as plain text unless it is that switch
// (gameserver.SearchedSwitch), and no other rule or extra argument names
// the same switch in any case: the server reads the first one only. A
// rule that names a setting of the game's catalog is checked by the
// setting's type and range too, and one that names a catalog setting in
// another case is refused. A map that starts with "/" needs a folder and a
// name: the server reads "/PvP" alone as the switch PvP.
func (in *Instance) checkChanges(old *Instance, now time.Time) error {
	// The server takes at most gameserver.MaxPlayersLimit players. A value
	// stored before ESM knew that stays until max_players is changed (the
	// server uses its limit; the instance carries a warning).
	if (old == nil || in.MaxPlayers != old.MaxPlayers) && in.MaxPlayers > gameserver.MaxPlayersLimit {
		return invalid("max_players", "must be between 1 and %d, the most the game server takes", gameserver.MaxPlayersLimit)
	}
	// An empty display name is no value: the server gets the instance's
	// name then.
	if (old == nil || in.DisplayName != old.DisplayName) && in.DisplayName != "" {
		if err := checkValue("display_name", "the display name", in.DisplayName); err != nil {
			return err
		}
	}
	if old == nil || in.World != old.World {
		if err := checkValue("world", "the world name", in.World); err != nil {
			return err
		}
	}
	if (old == nil || in.Map != old.Map) && in.Map != "" {
		if mapIsSwitch(in.Map) {
			return fault.New(fault.Invalid, "unsafe_argument", `map: a map that starts with "/" needs a folder and a name, like /Game/Maps/Name: the server reads "/" and a name alone as a switch of its own`)
		}
		if err := checkValue("map", "the map", in.Map); err != nil {
			return err
		}
	}
	names := in.switchNames()
	var before map[string]string
	if old != nil {
		before = old.Rules
	}
	for i, k := range slices.Sorted(maps.Keys(in.Rules)) {
		v := in.Rules[k]
		if prev, ok := before[k]; ok && prev == v {
			continue
		}
		// Validate let only letters, digits and "_" through, so the key
		// may be repeated.
		if !gameserver.PlainSwitchName(k) {
			return fault.New(fault.Invalid, "unsafe_argument", `rules: key %q may hold only ASCII letters and digits: the server would find a switch of its own after a "_"`, k)
		}
		if name, found := gameserver.SearchedSwitch(k); found {
			return fault.New(fault.Invalid, "unsafe_argument", "rules: the server's engine would find its switch %q in the key %s: it searches its whole command line for that name as plain text", name, k)
		}
		s, ok := gameserver.LookupGameSetting(k)
		if !ok {
			if f, folded := gameserver.FoldGameSetting(k); folded {
				return invalid("rules", "%s is the game setting %s: write its name as %s", k, f.Key, f.Key)
			}
		}
		if other, dup := names.repeated(i); dup {
			return fault.New(fault.Invalid, "unsafe_argument", "rules: rule %s names the same switch as %s: the server finds a switch without regard to case and reads only the first", k, other)
		}
		if ok {
			if err := s.Check(v, now); err != nil {
				return invalid("rules", "%v", err)
			}
			continue
		}
		// The server reads an empty value as the argument after it.
		if v == "" {
			return invalid("rules", "%s needs a value; leave the rule out to use the server's default", k)
		}
		if err := checkValue("rules", "the value of "+k, v); err != nil {
			return err
		}
	}
	var argsBefore []string
	if old != nil {
		argsBefore = old.ExtraArgs
	}
	for i, a := range in.ExtraArgs {
		if slices.Contains(argsBefore, a) {
			continue
		}
		pos := fmt.Sprintf("argument %d", i+1)
		name, value, hasValue := extraSwitch(a)
		if !gameserver.PlainSwitchName(name) {
			return fault.New(fault.Invalid, "unsafe_argument", "extra_args: %s must be -Name or -Name=Value with a name of ASCII letters and digits only: the server would find a switch of its own inside a name with anything else", pos)
		}
		if found, ok := gameserver.SearchedSwitch(name); ok {
			return fault.New(fault.Invalid, "unsafe_argument", "extra_args: the server's engine would find its switch %q in the name of %s: it searches its whole command line for that name as plain text", found, pos)
		}
		if other, dup := names.repeated(len(in.Rules) + i); dup {
			return fault.New(fault.Invalid, "unsafe_argument", "extra_args: %s names the same switch as %s: the server finds a switch without regard to case and reads only the first", pos, other)
		}
		// ESM leaves such an argument off the command line (see
		// gameserver.Launch.Args).
		if strings.HasSuffix(a, "=") {
			return invalid("extra_args", "%s ends in \"=\": the server would read the next argument as a value there, so ESM never passes an argument that ends in \"=\"; give it a value that does not, or leave out the \"=\"", pos)
		}
		if hasValue {
			if err := checkValue("extra_args", pos, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkValue checks text an operator set that goes onto the server command
// line as the value of a switch (gameserver.CheckValue). what names the
// setting without repeating its value. A name the engine searches for is
// named: it is the engine's, and no secret is checked here (see
// Secrets.commandLineProblem).
func checkValue(field, what, value string) error {
	why, bad := gameserver.CheckValue(value)
	if !bad {
		return nil
	}
	if name, found := gameserver.SearchedName(value); found && why == gameserver.HoldsSearchedName {
		why = fmt.Sprintf("holds %q, a name that the server's engine searches its whole command line for as plain text, in a quoted value too", name)
	}
	return fault.New(fault.Invalid, "unsafe_argument", "%s: %s %s", field, what, why)
}

// mapIsSwitch reports whether the server would read m, the map an instance
// names, as a switch: "/" and a name without another "/" ("/PvP").
func mapIsSwitch(m string) bool {
	return strings.HasPrefix(m, "/") && !strings.Contains(m[1:], "/")
}

// extraSwitch splits an extra argument into its switch name (what follows
// its "-", up to the first "=") and its value; hasValue says whether it
// has an "=".
func extraSwitch(a string) (name, value string, hasValue bool) {
	return strings.Cut(strings.TrimPrefix(a, "-"), "=")
}

// switchNames are the switches an instance's rules (sorted by key) and
// extra arguments (in order) name, as the server orders them on its
// command line, each with the words that name it in a refusal.
type switchNames []struct{ name, what string }

func (in *Instance) switchNames() switchNames {
	var out switchNames
	for _, k := range slices.Sorted(maps.Keys(in.Rules)) {
		out = append(out, struct{ name, what string }{k, "rule " + k})
	}
	for i, a := range in.ExtraArgs {
		n, _, _ := extraSwitch(a)
		out = append(out, struct{ name, what string }{n, fmt.Sprintf("extra argument %d", i+1)})
	}
	return out
}

// repeated reports whether another of the switches names the same switch
// as the one at i, and names that other one.
func (s switchNames) repeated(i int) (string, bool) {
	for j, o := range s {
		if j != i && gameserver.SameSwitch(o.name, s[i].name) {
			return o.what, true
		}
	}
	return "", false
}

// repeatedBefore reports whether a switch before the one at i names the
// same switch: the server reads that one, and not the one at i.
func (s switchNames) repeatedBefore(i int) bool {
	for j := 0; j < i; j++ {
		if gameserver.SameSwitch(s[j].name, s[i].name) {
			return true
		}
	}
	return false
}

// storedUnsafe names the settings of in, and with command-line delivery
// the secrets in s, that break the rules for the server command line (see
// checkChanges) and that ESM keeps passing as an earlier version stored
// them, so the server may read them otherwise than ESM shows: the name
// (which is in the folders on the command line, and cannot be changed),
// the display name, the world name and the map by their field, a rule as
// "rules.<key>", an extra argument by its position from 1
// ("extra_args.2", never by what it holds), and a secret by its field.
// An empty rule, and an extra argument that ends in "=", is among them; it
// is left off the command line (gameserver.Launch.Args). Of switches with
// the same name, those after the first are named.
func (in Instance) storedUnsafe(s Secrets) []string {
	var out []string
	unsafe := func(v string) bool {
		_, bad := gameserver.CheckValue(v)
		return bad
	}
	// A switch name that is not plain, that the engine would take for
	// another switch, or that ESM refuses when it is set now.
	badName := func(name string) bool {
		_, searched := gameserver.SearchedSwitch(name)
		_, refused := gameserver.RefusedSwitch(name)
		return !gameserver.PlainSwitchName(name) || searched || refused
	}
	if _, found := gameserver.SearchedName(in.Name); found {
		out = append(out, "name")
	}
	if in.DisplayName != "" && unsafe(in.DisplayName) {
		out = append(out, "display_name")
	}
	if unsafe(in.World) {
		out = append(out, "world")
	}
	if in.Map != "" && (mapIsSwitch(in.Map) || unsafe(in.Map)) {
		out = append(out, "map")
	}
	names := in.switchNames()
	for i, k := range slices.Sorted(maps.Keys(in.Rules)) {
		v := in.Rules[k]
		if g, ok := gameserver.LookupGameSetting(k); ok && g.Type == gameserver.TypeList {
			v = gameserver.ListValue(v)
		}
		if badName(k) || unsafe(v) || names.repeatedBefore(i) {
			out = append(out, "rules."+k)
		}
	}
	for i, a := range in.ExtraArgs {
		name, value, hasValue := extraSwitch(a)
		if badName(name) || hasValue && unsafe(value) || strings.HasSuffix(a, "=") || names.repeatedBefore(len(in.Rules)+i) {
			out = append(out, "extra_args."+strconv.Itoa(i+1))
		}
	}
	if in.SecretDelivery == gameserver.DeliveryCommandLine {
		for _, f := range s.fields() {
			if f.v != "" && unsafe(f.v) {
				out = append(out, f.field)
			}
		}
	}
	return out
}

// secretLike is the refusal of a rule key or an extra argument that looks
// like a secret. what names the offender without repeating any value.
func secretLike(field, what, why, instance, things string) error {
	return fault.New(fault.Invalid, "looks_like_secret",
		"%s: %s looks like a secret (its name %s). Passwords and tokens are set only with PUT /api/v1/instances/%s/secrets and are never shown; %s are shown to every role and go on the server command line",
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
	if i := strings.IndexFunc(name, func(r rune) bool { return r == ':' || gameserver.EngineSpace(r) }); i >= 0 {
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
	return unsafeArgument("extra_args", fmt.Sprintf("argument %d", pos), a)
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

// Secrets is the content of secrets.json (0600): the passwords and tokens
// the instance's server is given at its next start. Nothing else is kept:
// a server gets them in a file, so no earlier value has to be remembered
// to keep it out of anything ESM shows.
type Secrets struct {
	ServerPassword string `json:"server_password,omitempty"`
	AdminPassword  string `json:"admin_password,omitempty"`
	// AnnouncementsToken and ProblemReportToken are the game settings of
	// those names (see gameserver.GameSettings): secrets, so they are kept
	// here and never in the rules.
	AnnouncementsToken string `json:"announcements_token,omitempty"`
	ProblemReportToken string `json:"problem_report_token,omitempty"`
}

// forServer returns the passwords and tokens as the server is given them.
func (s Secrets) forServer() gameserver.Secrets {
	return gameserver.Secrets{ServerPassword: s.ServerPassword, AdminPassword: s.AdminPassword,
		AnnouncementsToken: s.AnnouncementsToken, ProblemReportToken: s.ProblemReportToken}
}

// change sets the secrets given in req.
func (s *Secrets) change(req SecretsRequest) {
	for _, c := range []struct {
		to   *string
		from *string
	}{{&s.ServerPassword, req.ServerPassword}, {&s.AdminPassword, req.AdminPassword},
		{&s.AnnouncementsToken, req.AnnouncementsToken}, {&s.ProblemReportToken, req.ProblemReportToken}} {
		if c.from != nil {
			*c.to = *c.from
		}
	}
}

// flags says which secrets are set.
func (s Secrets) flags() SecretFlags {
	return SecretFlags{ServerPasswordSet: s.ServerPassword != "", AdminPasswordSet: s.AdminPassword != "",
		AnnouncementsTokenSet: s.AnnouncementsToken != "", ProblemReportTokenSet: s.ProblemReportToken != ""}
}

// fields lists the secrets by their field in the API, with their values.
func (s Secrets) fields() []struct{ field, v string } {
	return []struct{ field, v string }{{"server_password", s.ServerPassword}, {"admin_password", s.AdminPassword},
		{"announcements_token", s.AnnouncementsToken}, {"problem_report_token", s.ProblemReportToken}}
}

// commandLineProblem reports the first secret of s that is set and that
// cannot go on the server command line (gameserver.CheckValue), by its
// field, and why: which rule it breaks, never its value or a part of it.
// With secret_delivery "file" a secret may be anything; with
// "command_line" it is a value on the command line like any other.
func (s Secrets) commandLineProblem() (field, why string, bad bool) {
	for _, f := range s.fields() {
		if f.v == "" {
			continue
		}
		if why, bad := gameserver.CheckValue(f.v); bad {
			return f.field, why, true
		}
	}
	return "", "", false
}

// named lists the secrets that are set, each with the words that name it
// in a refusal.
func (s Secrets) named() []struct{ what, v string } {
	var out []struct{ what, v string }
	for _, x := range []struct{ what, v string }{{"server password", s.ServerPassword}, {"admin password", s.AdminPassword},
		{"announcements token", s.AnnouncementsToken}, {"problem report token", s.ProblemReportToken}} {
		if x.v != "" {
			out = append(out, x)
		}
	}
	return out
}

// SecretsRequest changes secrets: nil leaves a value, "" clears it.
type SecretsRequest struct {
	ServerPassword     *string `json:"server_password,omitempty"`
	AdminPassword      *string `json:"admin_password,omitempty"`
	AnnouncementsToken *string `json:"announcements_token,omitempty"`
	ProblemReportToken *string `json:"problem_report_token,omitempty"`
}

// given returns the secrets req sets ("" for the others).
func (req SecretsRequest) given() Secrets {
	var s Secrets
	s.change(req)
	return s
}

// check checks every value req sets. Its errors never repeat a value.
func (req SecretsRequest) check() error {
	for _, pw := range []struct {
		field string
		v     *string
	}{{"server_password", req.ServerPassword}, {"admin_password", req.AdminPassword}} {
		if pw.v != nil {
			if err := checkSecret(pw.field, *pw.v); err != nil {
				return err
			}
		}
	}
	for _, tok := range []struct {
		field string
		v     *string
	}{{"announcements_token", req.AnnouncementsToken}, {"problem_report_token", req.ProblemReportToken}} {
		if tok.v != nil {
			if err := checkToken(tok.field, *tok.v); err != nil {
				return err
			}
		}
	}
	return nil
}

// SecretFlags says which secrets are set, without showing them.
type SecretFlags struct {
	ServerPasswordSet     bool `json:"server_password_set"`
	AdminPasswordSet      bool `json:"admin_password_set"`
	AnnouncementsTokenSet bool `json:"announcements_token_set"`
	ProblemReportTokenSet bool `json:"problem_report_token_set"`
}

// any says whether a password or token is set.
func (f SecretFlags) any() bool {
	return f.ServerPasswordSet || f.AdminPasswordSet || f.AnnouncementsTokenSet || f.ProblemReportTokenSet
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

// maxTokenLength bounds a token.
const maxTokenLength = 256

// checkToken checks a new token ("" clears it): MinSecretLength to
// maxTokenLength characters of printable ASCII without white space, a
// double quote, "," or ")". A token may go on the server command line,
// where the server ends a value without white space at "," or ")", so a
// token holding one would arrive cut. With secret_delivery "command_line"
// it follows the other rules for values there too (see SetSecrets). Its
// errors never repeat the value or say what it holds.
func checkToken(field, v string) error {
	if v == "" {
		return nil
	}
	if len(v) < MinSecretLength || len(v) > maxTokenLength || strings.ContainsFunc(v, func(r rune) bool {
		return r <= ' ' || r > '~' || r == '"' || r == ',' || r == ')'
	}) {
		return invalid(field, "must be %d to %d characters of printable ASCII, without white space, '\"', ',' or ')'", MinSecretLength, maxTokenLength)
	}
	return nil
}

// checkPasswordsInSettings refuses passwords and tokens that occur,
// without regard to case, in a setting that is shown to every role or that
// the server may print: the name, display name, world, map, cluster, the
// rule keys and values, and the extra arguments. Output filters mask the
// passwords and tokens, so such a setting would show as masked and give
// the secret away. The error names the secret and the setting, never a
// value.
func checkPasswordsInSettings(in Instance, s Secrets) error {
	type text struct{ field, v string }
	texts := []text{{"name", in.Name}, {"display_name", in.DisplayName}, {"world", in.World}, {"map", in.Map}, {"cluster", in.Cluster}}
	for _, k := range slices.Sorted(maps.Keys(in.Rules)) {
		texts = append(texts, text{"rules", k}, text{"rules", in.Rules[k]})
	}
	for _, a := range in.ExtraArgs {
		texts = append(texts, text{"extra_args", a})
	}
	for _, pw := range s.named() {
		low := strings.ToLower(pw.v)
		for _, t := range texts {
			if strings.Contains(strings.ToLower(t.v), low) {
				return fault.New(fault.Invalid, "password_in_settings",
					"the %s occurs in %s: a password or token may not be part of a setting, which every role sees and the server may print; change the secret or the setting",
					pw.what, t.field)
			}
		}
	}
	return nil
}
