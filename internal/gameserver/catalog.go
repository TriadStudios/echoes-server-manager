package gameserver

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This file is the catalog of the game settings that the server takes on
// its command line and that ESM does not set itself (ESM's own switches
// are the ones in reserved). An instance keeps them in its rules, each
// passed as -Key=Value. A rule that is not set is not passed, and the
// server uses its own default. Keys outside the catalog stay allowed as
// plain rules, for settings of server builds newer than this catalog.
//
// What the catalog says about a setting (its name, type, range and default)
// is what the server builds of the game accept as this is written. The
// server clamps a number outside its range and ignores a value it cannot
// read; ESM refuses both, so a setting does what it shows.

// Groups of game settings.
const (
	GroupAccess  = "access"
	GroupWorld   = "world"
	GroupChat    = "chat"
	GroupReports = "reports"
)

// Types of game settings.
const (
	TypeBool   = "bool"   // True or False (also true, yes, no, 1 and 0, in any case)
	TypeInt    = "int"    // a whole number
	TypeFloat  = "float"  // a decimal number like 0.25
	TypeString = "string" // text
	TypeEnum   = "enum"   // one of Choices
	TypeList   = "list"   // items separated by commas
	TypeDate   = "date"   // YYYY-MM-DD
)

// FormatURL marks a string setting that holds an http or https address.
const FormatURL = "url"

// SettingGroup is a group of game settings.
type SettingGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Note is said once for the whole group.
	Note string `json:"note"`
}

// Choice is one allowed value of an enum setting.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// GameSetting is one setting of the catalog.
type GameSetting struct {
	// Key is the switch name exactly as the server reads it.
	Key   string `json:"key"`
	Group string `json:"group"`
	Label string `json:"label"`
	Type  string `json:"type"`
	// Default is what the server uses when the setting is not passed,
	// written as ESM would pass it; "" when that is empty or when the
	// server works it out itself (Help says what it does then).
	Default string `json:"default"`
	// Min and Max bound a number (inclusive); nil is no bound.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Unit names what a number counts, like "minutes".
	Unit string `json:"unit,omitempty"`
	// Choices are the values of an enum.
	Choices []Choice `json:"choices,omitempty"`
	// MaxLength bounds a string or a list, in bytes.
	MaxLength int    `json:"max_length,omitempty"`
	Format    string `json:"format,omitempty"`
	Help      string `json:"help"`
	// NewWorldOnly marks a world rule: the game applies it only when the
	// world is first created, and a world that exists keeps the rules
	// saved with it.
	NewWorldOnly bool `json:"new_world_only"`
	// Caution marks a setting that weakens the server's protection; Help
	// says how.
	Caution bool `json:"caution"`
	// Secret marks a value that is never a rule: it is kept with the
	// instance's passwords, set with the secrets call under SecretField
	// and never shown.
	Secret      bool   `json:"secret"`
	SecretField string `json:"secret_field,omitempty"`
}

func num(f float64) *float64 { return &f }

// Rates share one range.
func rate(key, label, help string) GameSetting {
	return GameSetting{Key: key, Group: GroupWorld, Label: label, Type: TypeFloat, Default: "1", Min: num(0.1), Max: num(1000),
		Help: help + " 1 is normal.", NewWorldOnly: true}
}

func boolSetting(group, key, label, def, help string) GameSetting {
	return GameSetting{Key: key, Group: group, Label: label, Type: TypeBool, Default: def, Help: help, NewWorldOnly: group == GroupWorld}
}

var settingGroups = []SettingGroup{
	{ID: GroupAccess, Title: "Identity and access",
		Note: "Read at every start of the server."},
	{ID: GroupWorld, Title: "World rules",
		Note: "The game applies these only when the world is first created. A world that exists keeps the rules saved with it, so a change here takes effect only for a new world (for example under a new world name)."},
	{ID: GroupChat, Title: "Chat",
		Note: "Read at every start of the server."},
	{ID: GroupReports, Title: "Announcements and reports",
		Note: "Read at every start of the server. The two tokens are kept with the passwords: they are never shown, and reach the server the way the passwords do."},
}

var gameSettings = []GameSetting{
	{Key: "MOTD", Group: GroupAccess, Label: "Welcome message", Type: TypeString, MaxLength: 256,
		Help: "Sent in chat to each player who joins. Not set: the game's built-in welcome. A world that has a welcome message of its own keeps using that one."},
	boolSetting(GroupAccess, "LAN", "LAN session", "False",
		"Register the session as a LAN session, for a server on a local network."),
	{Key: "AllowNonSteamClients", Group: GroupAccess, Label: "Players without Steam", Type: TypeBool, Default: "False", Caution: true,
		Help: "Let players join without a Steam identity, for LAN and test setups. Such players cannot be told apart reliably: do not use it together with admin players."},
	{Key: "AdminPlayerIds", Group: GroupAccess, Label: "Admin players", Type: TypeList, MaxLength: 2000,
		Help: "Players who are admins without the admin password: their SteamID64 numbers (or net: ids), separated by commas."},
	{Key: "AdminLoginMaxAttempts", Group: GroupAccess, Label: "Admin login attempts", Type: TypeInt, Default: "5", Min: num(0),
		Help: "Wrong admin passwords a player may try before admin login is locked for them. 0: never locked."},
	{Key: "AdminLoginLockoutSeconds", Group: GroupAccess, Label: "Admin lockout", Type: TypeInt, Default: "60", Min: num(0), Max: num(3600), Unit: "seconds",
		Help: "Length of the first admin login lockout. Each further lockout doubles it, up to one hour."},
	{Key: "BossSummonCooldownSeconds", Group: GroupAccess, Label: "Boss summon cooldown", Type: TypeInt, Default: "0", Min: num(0), Unit: "seconds",
		Help: "Wait between two runs of a tribute dungeon, counted from the end of the last run. 0: no wait."},
	{Key: "LastWipe", Group: GroupAccess, Label: "Last wipe", Type: TypeDate,
		Help: "The date the server list shows as the last wipe (UTC, not in the future). Unlike the world rules it is read at every start. Not set: the date the world keeps."},

	rate("HarvestRate", "Harvest rate", "Multiplies what gathering yields."),
	rate("XPRate", "XP rate", "Multiplies the experience from gathering, taming and creature kills."),
	rate("TamingRate", "Taming rate", "Multiplies the taming progress of each feeding."),
	rate("TamingBiteRate", "Taming bite rate", "How often a creature eats while it is tamed."),
	rate("HatchRate", "Hatch rate", "Speed of egg incubation and gestation."),
	rate("MatureRate", "Mature rate", "Speed at which babies grow up."),
	rate("MatingRate", "Mating rate", "Shortens the wait between matings."),
	rate("CareRate", "Care rate", "How often a baby asks for care."),
	rate("ImprintRate", "Imprint rate", "Multiplies the imprint gained with each care."),
	{Key: "DayLengthMinutes", Group: GroupWorld, Label: "Day length", Type: TypeFloat, Default: "60", Min: num(5), Max: num(240), Unit: "minutes",
		Help: "Length of one day and night in real minutes.", NewWorldOnly: true},
	boolSetting(GroupWorld, "StructureDecay", "Structure decay", "True",
		"Structures nobody visits decay and are removed in time."),
	{Key: "StructureDecayMultiplier", Group: GroupWorld, Label: "Decay time multiplier", Type: TypeFloat, Default: "1", Min: num(0.1), Max: num(10),
		Help: "Multiplies the time before an unvisited structure decays.", NewWorldOnly: true},
	boolSetting(GroupWorld, "PvP", "PvP", "False",
		"Players can fight other players and damage their bases. Off: a PvE world."),
	boolSetting(GroupWorld, "PvPStructureDamage", "PvP structure damage", "True",
		"On a PvP world, whether bases can be damaged."),
	boolSetting(GroupWorld, "OfflineRaidProtection", "Offline raid protection", "False",
		"Protect the structures and tames of a tribe while all of its members are offline."),
	{Key: "ORPActivationMinutes", Group: GroupWorld, Label: "Protection delay", Type: TypeFloat, Default: "15", Min: num(0), Max: num(1440), Unit: "minutes",
		Help: "How long a tribe must be offline before its protection starts.", NewWorldOnly: true},
	{Key: "ORPStructureDamageTaken", Group: GroupWorld, Label: "Protected structure damage", Type: TypeFloat, Default: "0.25", Min: num(0), Max: num(1),
		Help: "Share of damage that protected structures take. 0: none.", NewWorldOnly: true},
	{Key: "ORPTameDamageTaken", Group: GroupWorld, Label: "Protected tame damage", Type: TypeFloat, Default: "0.25", Min: num(0), Max: num(1),
		Help: "Share of damage that protected tames take. 0: none.", NewWorldOnly: true},
	{Key: "ORPTurretDamageMultiplier", Group: GroupWorld, Label: "Offline turret damage", Type: TypeFloat, Default: "2", Min: num(0), Max: num(10),
		Help: "Multiplies the damage that the turrets of a protected tribe deal.", NewWorldOnly: true},
	{Key: "Difficulty", Group: GroupWorld, Label: "Difficulty", Type: TypeEnum, Default: "1",
		Choices: []Choice{{"0", "Easy"}, {"1", "Normal"}, {"2", "Hard"}},
		Help:    "Sets the highest level of wild creatures.", NewWorldOnly: true},
	{Key: "DeathDropMode", Group: GroupWorld, Label: "Dropped on death", Type: TypeEnum, Default: "Everything",
		Choices: []Choice{{"Everything", "Everything"}, {"InventoryOnly", "Inventory only"}, {"Nothing", "Nothing"}},
		Help:    "What a player drops on death: the inventory and what they wear and hold, the inventory only, or nothing.", NewWorldOnly: true},

	boolSetting(GroupChat, "ChatLog", "Chat log", "True",
		"Keep a daily log file of all chat on the server."),
	{Key: "ChatFilter", Group: GroupChat, Label: "Chat filter", Type: TypeEnum, Default: "Mask",
		Choices: []Choice{{"Off", "Off"}, {"Mask", "Mask"}, {"Drop", "Drop"}},
		Help:    "What happens to a message with a blocked word: Off delivers it, Mask stars the word out, Drop refuses the message."},
	{Key: "ChatBlockedWords", Group: GroupChat, Label: "Blocked words", Type: TypeList, MaxLength: 2000,
		Help: "Words the chat filter acts on, separated by commas. Whole words, in any case."},

	{Key: "AnnouncementsUrl", Group: GroupReports, Label: "Announcements address", Type: TypeString, Format: FormatURL, MaxLength: 256,
		Help: "Web address the server asks for announcements to show its players. Not set: no announcements are fetched."},
	{Key: "AnnouncementsPollSeconds", Group: GroupReports, Label: "Announcements interval", Type: TypeFloat, Default: "30", Min: num(10), Unit: "seconds",
		Help: "Time between two requests for announcements."},
	{Key: "AnnouncementsToken", Group: GroupReports, Label: "Announcements token", Type: TypeString, Secret: true, SecretField: "announcements_token",
		Help: "Token sent with each request for announcements. Not set: the problem report token is sent."},
	{Key: "ProblemReportUrl", Group: GroupReports, Label: "Problem report address", Type: TypeString, Format: FormatURL, MaxLength: 256,
		Help: "Web address problem reports are uploaded to. Not set: reports stay on the server."},
	{Key: "ProblemReportToken", Group: GroupReports, Label: "Problem report token", Type: TypeString, Secret: true, SecretField: "problem_report_token",
		Help: "Token sent with each problem report."},
}

// GameSettingGroups returns the groups of the catalog, in the order they
// are shown.
func GameSettingGroups() []SettingGroup { return slices.Clone(settingGroups) }

// GameSettings returns the catalog, by group in the order they are shown.
func GameSettings() []GameSetting { return slices.Clone(gameSettings) }

// LookupGameSetting returns the setting named exactly key.
func LookupGameSetting(key string) (GameSetting, bool) {
	for _, s := range gameSettings {
		if s.Key == key {
			return s, true
		}
	}
	return GameSetting{}, false
}

// FoldGameSetting returns the setting named key without regard to case:
// the server finds its switches that way, so "motd" is the setting MOTD.
func FoldGameSetting(key string) (GameSetting, bool) {
	for _, s := range gameSettings {
		if strings.EqualFold(s.Key, key) {
			return s, true
		}
	}
	return GameSetting{}, false
}

var (
	intRe   = regexp.MustCompile(`^-?[0-9]{1,10}$`)
	floatRe = regexp.MustCompile(`^-?(?:[0-9]{1,10}(?:\.[0-9]{1,9})?|\.[0-9]{1,9})$`)
	dateRe  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// boolWords are the bool values the server reads, in lower case.
var boolWords = []string{"true", "false", "yes", "no", "1", "0"}

// Check reports whether value is a value of s that the server reads as
// given. now is the time it is checked at (a date may not be in the
// future). The error names the setting and what it allows; it repeats no
// value.
func (s GameSetting) Check(value string, now time.Time) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf(s.Key+" "+format, args...)
	}
	if s.Secret {
		return bad("is a secret: it is set with the instance's secrets (%s), never as a rule", s.SecretField)
	}
	if hasControlChar(value) {
		return bad("may not hold control characters")
	}
	// The server reads an empty value as the argument after it. Not set
	// is the way to the server's default.
	if value == "" || (s.Type == TypeList && ListValue(value) == "") {
		return bad("needs a value; to use the server's default, leave the setting out (not set)")
	}
	switch s.Type {
	case TypeBool:
		if !slices.Contains(boolWords, strings.ToLower(value)) {
			return bad("must be True or False")
		}
		return nil
	case TypeInt:
		n, err := strconv.ParseInt(value, 10, 32)
		if !intRe.MatchString(value) || err != nil || !s.inRange(float64(n)) {
			return bad("must be a whole number %s", s.rangeWords())
		}
		return nil
	case TypeFloat:
		f, err := strconv.ParseFloat(value, 64)
		if !floatRe.MatchString(value) || err != nil || !s.inRange(f) {
			return bad("must be a number %s", s.rangeWords())
		}
		return nil
	case TypeEnum:
		var values []string
		for _, c := range s.Choices {
			if strings.EqualFold(c.Value, value) {
				return nil
			}
			values = append(values, c.Value)
		}
		return bad("must be %s", orList(values))
	case TypeDate:
		d, err := time.Parse(time.DateOnly, value)
		if !dateRe.MatchString(value) || err != nil {
			return bad("must be a date like 2026-01-31 (YYYY-MM-DD)")
		}
		if y, m, dd := now.UTC().Date(); d.After(time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)) {
			return bad("may not be in the future (the server ignores such a date)")
		}
		return nil
	case TypeList:
		if len(value) > s.MaxLength {
			return bad("must be at most %d characters", s.MaxLength)
		}
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if strings.ContainsFunc(item, unicode.IsSpace) || strings.ContainsAny(item, `")`) {
				return bad("items are separated by commas and may not hold white space, ')' or a double quote")
			}
		}
		for _, v := range []string{value, ListValue(value)} {
			if why, unsafe := UnsafeArgument(v); unsafe {
				return bad("%s", why)
			}
		}
		return nil
	case TypeString:
		if len(value) > s.MaxLength {
			return bad("must be at most %d characters", s.MaxLength)
		}
		if why, unsafe := UnsafeArgument(value); unsafe {
			return bad("%s", why)
		}
		if !strings.ContainsFunc(value, unicode.IsSpace) && strings.ContainsAny(value, ",)") {
			return bad("may not hold ',' or ')' unless it also holds a space: the server ends a value without spaces there")
		}
		if s.Format == FormatURL {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsFunc(value, unicode.IsSpace) {
				return bad("must be an http or https address like https://example.com/path")
			}
		}
		return nil
	}
	return bad("has an unknown type %q", s.Type)
}

func (s GameSetting) inRange(f float64) bool {
	return (s.Min == nil || f >= *s.Min) && (s.Max == nil || f <= *s.Max)
}

// rangeWords says the range of a number.
func (s GameSetting) rangeWords() string {
	f := func(p *float64) string { return strconv.FormatFloat(*p, 'f', -1, 64) }
	switch {
	case s.Min != nil && s.Max != nil:
		return "from " + f(s.Min) + " to " + f(s.Max)
	case s.Min != nil:
		return "of at least " + f(s.Min)
	case s.Max != nil:
		return "of at most " + f(s.Max)
	}
	return ""
}

// orList writes values as "a, b or c".
func orList(values []string) string {
	if len(values) < 2 {
		return strings.Join(values, "")
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

func hasControlChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ListValue writes a list as ESM passes it: the items without the white
// space around them, empty items left out, joined by ", ". The server ends
// a value without white space at its first comma, and reads a value with
// white space whole, so a list of two or more items reaches it whole this
// way.
func ListValue(value string) string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return strings.Join(items, ", ")
}

// switchValue is the value of the rule key as it goes onto the command
// line: a list setting of the catalog as ListValue writes it, anything
// else as it is.
func switchValue(key, value string) string {
	if s, ok := LookupGameSetting(key); ok && s.Type == TypeList {
		return ListValue(value)
	}
	return value
}
