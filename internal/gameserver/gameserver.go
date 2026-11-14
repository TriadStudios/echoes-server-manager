// Package gameserver is the real-server adapter.
//
// Everything ESM assumes about the Echoes of Durinn dedicated server is in
// this package. Values marked UNCONFIRMED are not yet proven on a real host.
// Values marked CONFIRMED were seen on a real Linux host running one
// server.
//
// The fake server (cmd/esm-fakeserver) follows exactly these assumptions, so
// tests that run the fake exercise this code. When a real host proves a value
// wrong, it is changed here and in the fake, and nowhere else.
//
// Nothing game-specific is written into ESM: the path of the server binary
// inside a build and the build's default map come from the build manifest,
// and the map of an instance from its settings.
package gameserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Mask stands in for a secret value in text that ESM shows.
const Mask = "********"

// The switches ESM puts on every server command line. CONFIRMED: a real
// server started with exactly these (and no others) ran, was listed and
// was joined.
const (
	UnattendedSwitch    = "-unattended"
	PortSwitch          = "-Port"
	QueryPortSwitch     = "-QueryPort"
	ServerNameSwitch    = "-ServerName"
	WorldNameSwitch     = "-WorldName"
	MaxPlayersSwitch    = "-MaxPlayers"
	ListInBrowserSwitch = "-ListInBrowser"
	// UserDirSwitch points the server at its own folder: it keeps its saves
	// (and its other written files) below it, so a new build never touches
	// them. CONFIRMED.
	UserDirSwitch = "-UserDir"
	// LogFileSwitch names the file the server writes its log to (an
	// absolute path). CONFIRMED.
	LogFileSwitch = "-abslog"
)

// MultihomeSwitch is the engine's bind-address switch. ESM never passes it
// and refuses it as a rule or an extra argument. CONFIRMED on a real host:
// with it the server advertises the bind address where its Steam identity
// belongs, and players who pick it from the server list cannot join.
// Without it the query socket listens on every address of the box; the
// box's firewall decides where the query port is reachable from.
const MultihomeSwitch = "-MULTIHOME"

// Launch is everything needed to build a server command line.
type Launch struct {
	// Map is the map the server loads: the instance's map, or the build's
	// default map. The command line needs one; ESM refuses to start an
	// instance without either.
	Map        string
	GamePort   int
	QueryPort  int
	MaxPlayers int
	ServerName string
	WorldName  string
	// Listed says whether the server shows in the in-game server list.
	Listed bool
	// UserDir is the absolute path of the instance's data folder.
	UserDir string
	// LogFile is the absolute path of the server log file.
	LogFile string
	// Rules are game settings; each becomes -Key=Value, sorted by key. A
	// list setting of the catalog is written as ListValue writes it.
	// CONFIRMED: every key of the game's server settings works as a switch.
	// UNCONFIRMED: whether a loose settings file next to the build would be
	// read instead; ESM does not rely on one.
	Rules map[string]string
	// Delivery is how the passwords reach the server: DeliveryFile or
	// DeliveryCommandLine. Empty means SecretDelivery.
	Delivery string
	// SecretsFile is the absolute path of the secrets file. With
	// DeliveryFile the server is pointed at it and gets no password on its
	// command line.
	SecretsFile string
	// Secrets are the passwords and tokens. They are put on the command
	// line with DeliveryCommandLine, and never otherwise.
	Secrets Secrets
	// ExtraArgs are appended as they are, one argv entry each, except one
	// that ends in "=" (see Args).
	ExtraArgs []string
}

// Args builds the argument list (without the executable). It never builds a
// shell string. Order: the map, -unattended, -Port, -QueryPort,
// -ServerName, -WorldName, -MaxPlayers, -ListInBrowser, -UserDir, -abslog,
// the rules sorted by key, the passwords and tokens (the secrets file
// switch; or, with DeliveryCommandLine, -Password, -AdminPassword,
// -AnnouncementsToken and -ProblemReportToken when set), then the extra
// args. -MULTIHOME is never part of it: a bind address configured
// for an instance is not given to the server.
//
// A switch with an empty value is never passed, as "-Key=" or as the bare
// switch: the server reads an empty value that is not quoted as the
// argument after it ("-MOTD=" would take the next switch for its value),
// and reads a bare switch as on ("-PvP"). So a rule with an empty value
// (one stored before ESM refused them) is left out, and so is an extra
// argument that ends in "="; LeftOut names them. Other values are passed
// as they are: one stored before ESM checked values as CheckValue does is
// still passed unchanged, and may end in "=".
func (l Launch) Args() []string {
	argv := []string{l.Map, UnattendedSwitch}
	for _, s := range [][2]string{
		{PortSwitch, strconv.Itoa(l.GamePort)},
		{QueryPortSwitch, strconv.Itoa(l.QueryPort)},
		{ServerNameSwitch, l.ServerName},
		{WorldNameSwitch, l.WorldName},
		{MaxPlayersSwitch, strconv.Itoa(l.MaxPlayers)},
		{ListInBrowserSwitch, boolWord(l.Listed)},
		{UserDirSwitch, l.UserDir},
		{LogFileSwitch, l.LogFile},
	} {
		if s[1] != "" {
			argv = append(argv, s[0]+"="+s[1])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(l.Rules)) {
		if v := switchValue(k, l.Rules[k]); v != "" {
			argv = append(argv, "-"+k+"="+v)
		}
	}
	if l.Delivery == DeliveryCommandLine {
		for _, s := range [][2]string{
			{ServerPasswordSwitch, l.Secrets.ServerPassword},
			{AdminPasswordSwitch, l.Secrets.AdminPassword},
			{AnnouncementsTokenSwitch, l.Secrets.AnnouncementsToken},
			{ProblemReportTokenSwitch, l.Secrets.ProblemReportToken},
		} {
			if s[1] != "" {
				argv = append(argv, s[0]+"="+s[1])
			}
		}
	} else if l.SecretsFile != "" {
		argv = append(argv, SecretsFileSwitch+"="+l.SecretsFile)
	}
	for _, a := range l.ExtraArgs {
		if !strings.HasSuffix(a, "=") && strings.Trim(a, "-") != "" {
			argv = append(argv, a)
		}
	}
	return argv
}

// LeftOut names what Args leaves out because its value is empty: the rule
// keys, and the extra arguments by position ("extra argument 2"), never by
// what they hold.
func (l Launch) LeftOut() []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(l.Rules)) {
		if switchValue(k, l.Rules[k]) == "" {
			out = append(out, k)
		}
	}
	for i, a := range l.ExtraArgs {
		if strings.HasSuffix(a, "=") {
			out = append(out, "extra argument "+strconv.Itoa(i+1))
		}
	}
	return out
}

// boolWord writes a switch value the way the engine's own switches are
// written: True or False.
func boolWord(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// reserved are the switches ESM sets itself; rules and extra args must not
// repeat them.
var reserved = []string{
	switchName(UnattendedSwitch), switchName(PortSwitch), switchName(QueryPortSwitch),
	switchName(ServerNameSwitch), switchName(WorldNameSwitch), switchName(MaxPlayersSwitch),
	switchName(ListInBrowserSwitch), switchName(UserDirSwitch), switchName(LogFileSwitch),
	switchName(ServerPasswordSwitch), switchName(AdminPasswordSwitch), switchName(SecretsFileSwitch),
	switchName(AnnouncementsTokenSwitch), switchName(ProblemReportTokenSwitch),
}

// switchName is a switch name as it is compared: lower case, no dashes in
// front.
func switchName(name string) string { return strings.ToLower(strings.TrimLeft(name, "-")) }

// IsReservedSwitch reports whether key (with or without the leading "-")
// names a switch ESM sets itself.
func IsReservedSwitch(key string) bool {
	return slices.Contains(reserved, switchName(key))
}

// MaxPlayersLimit is the most players the server takes: it lowers a
// larger -MaxPlayers to this.
const MaxPlayersLimit = 70

// RefusedSwitch reports whether key (with or without the leading "-") names
// a switch that must never reach the server, and says why. Besides
// -MULTIHOME these are the engine's overrides of its configuration files:
// "-ini:<File>:[<Section>]:<Key>=<Value>" (the name is "ini", up to the
// ":"), -iniFile= and -CustomConfig=, which could change what ESM sets
// itself (ports, networking, where the server keeps its files); and
// -CmdLineFile=, which makes the engine put the content of a file where
// the switch stands on its command line: switches ESM would neither show
// nor check.
func RefusedSwitch(key string) (why string, refused bool) {
	switch switchName(key) {
	case switchName(MultihomeSwitch):
		return MultihomeSwitch + " is never passed: with it the server advertises the bind address instead of its Steam identity and players cannot join; the box's firewall decides where the query port is reachable", true
	case "ini", "inifile", "customconfig":
		return "-ini:, -iniFile= and -CustomConfig= change the engine's configuration files and are never passed: they could change what ESM sets itself (ports, networking, the server's folders)", true
	case "cmdlinefile":
		return "-CmdLineFile= is never passed: the engine would read more of its command line from that file, switches that ESM neither shows nor checks", true
	}
	return "", false
}

// How the server reads its command line, which the checks below follow.
// Its launcher joins the arguments into one line, each after a space, and
// puts an argument that holds a space (only the plain space counts) in
// double quotes, after its first "=". The server then looks a switch up as
// "Name=" in that line: without regard to the case of ASCII letters, only
// where the character before the name is not an ASCII letter or digit,
// and never between double quotes, also inside the value of another
// switch ("-MOTD=x,PvP=True" sets PvP). A value in quotes is read whole;
// any other value ends at the first ",", ")" or white space, and an empty
// one takes the next argument. A switch written alone ("-PvP") turns a
// setting that is on or off on; it is found after white space, and what
// white space is, the engine asks its host (see EngineSpace).
//
// That is the engine's usual way, and not its only one. Some of its code
// finds a name and "=" at the start of any word of the line without
// minding the quotes, or searches the line, or each argument before the
// line is joined, for a name as plain text, in the middle of a word too.
// So a value with a space is quoted, but not out of reach.
//
// The rules below make sure that on the line the only "=" are the ones ESM
// writes after the name of a switch, each switch once and each followed by
// the whole value ESM shows, and that no value holds what the engine could
// take for the start of a switch. They look at how a value is built, not
// at which names it holds: no list of names could know every switch of the
// engine. The one exception are the few names the engine searches for as
// plain text and without an "=" (see searched): nothing in the build of a
// value keeps those out, so ESM knows them one by one.

// engineSpaces are the characters that some host of the server's engine
// counts as white space, as ranges of code points (first and last). The
// engine asks the host's C library. On Windows that is every one of them,
// U+180E too, which Unicode no longer lists as white space; on Linux, in
// the "C" locale the engine runs in, only the ASCII ones (U+0009 to U+000D
// and U+0020). ESM does not know what host a build is for when a value is
// set, and a build may move, so a value is checked against all of them.
var engineSpaces = [][2]rune{
	{0x0009, 0x000d}, {0x0020, 0x0020}, {0x0085, 0x0085}, {0x00a0, 0x00a0}, {0x1680, 0x1680}, {0x180e, 0x180e},
	{0x2000, 0x200a}, {0x2028, 0x2029}, {0x202f, 0x202f}, {0x205f, 0x205f}, {0x3000, 0x3000},
}

// EngineSpace reports whether some host of the server's engine counts r as
// white space. It is the one answer to what white space is wherever ESM
// checks a setting that goes onto the server command line.
func EngineSpace(r rune) bool {
	for _, s := range engineSpaces {
		if s[0] <= r && r <= s[1] {
			return true
		}
	}
	return false
}

// printable reports whether r is a character of printable text: a letter,
// a mark, a number, punctuation, a symbol, or the plain space. A control
// character, a format character (zero width space and joiners, direction
// marks, soft hyphen, byte order mark), a line or paragraph separator, any
// other space, a private-use code point and a code point Unicode has not
// assigned are not: they do not show, and what the engine's hosts make of
// them is not the same everywhere, nor from one release to the next.
func printable(r rune) bool {
	return r == ' ' || unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

// enDash is the character the engine takes for a "-" spoilt on its way
// through a mail program: with one anywhere on its command line it stops
// at start.
const enDash = 0x2013

// asciiAlnum reports whether r is an ASCII letter or digit: what the engine
// takes for part of a name when it looks for the start of one.
func asciiAlnum(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9'
}

func asciiLetter(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
}

// checkText reports whether text, something an operator sets, holds a
// character that no setting on the server command line may hold, and says
// which kind: a double quote, a control character, white space other than
// the plain space (by EngineSpace), a character that is not printable
// text, or an en dash. The answer never repeats a part of text.
func checkText(text string) (why string, bad bool) {
	if !utf8.ValidString(text) {
		return "is not valid UTF-8", true
	}
	for _, r := range text {
		switch {
		case r == '"':
			return `has a double quote ("), which the server's command line parsing takes for the end of a quoted value`, true
		case r != ' ' && EngineSpace(r):
			return "has white space other than the plain space (a tab, a line break or a no-break space, say), which the server does not quote and ends a value at", true
		case unicode.IsControl(r):
			return "has a control character", true
		case !printable(r):
			return "has a character that is not printable text (a zero width or direction mark, say, or a code point that is not in use); such a character does not show, and not every host of the server reads it the same", true
		case r == enDash:
			return `has an en dash (U+2013), which the server's engine takes for a spoilt "-": it does not start with one on its command line; write "-"`, true
		}
	}
	return "", false
}

// HoldsSearchedName is what CheckValue says of a value that holds one of
// the names the engine searches its command line for as plain text. It
// does not say which: SearchedName does, for a caller that may show it.
const HoldsSearchedName = "holds a name that the server's engine searches its whole command line for as plain text, in a quoted value too"

// CheckValue reports whether value, text an operator sets that ESM writes
// on the server command line as the value of a switch, is read by the
// server whole and as that value only, and says why not. It is the one
// check for every such text: a rule value, the value of an extra argument
// (after its "="), the server name, the world name, the map, and the
// passwords and tokens when they go on the command line. A value
//
//   - is not empty: the server would read the next argument as its value;
//   - has no double quote, no control character, no white space other
//     than the plain space, no character that is not printable text and no
//     en dash (checkText), and no space at its start or end;
//   - has no "=" at all: the engine finds a name and "=" in a quoted value
//     too, and reads it as a switch of its own;
//   - when it holds no space, has no "," or ")": the server reads it
//     unquoted and would end it there (one with a space is quoted and
//     read whole);
//   - has no space followed by "-" and more, and no "-" followed by an
//     ASCII letter unless an ASCII letter or digit stands right before it
//     ("well-met" is a word, "x;-met" and "-met" could start a switch);
//   - holds none of the names the engine searches for as plain text
//     (SearchedName).
//
// The answer never repeats the value, nor any part of it: it is used for
// secrets too.
func CheckValue(value string) (why string, bad bool) {
	if value == "" {
		return "is empty, and the server would read the next argument as its value", true
	}
	if why, bad := checkText(value); bad {
		return why, true
	}
	if strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
		return "starts or ends with a space", true
	}
	if strings.Contains(value, "=") {
		return `has "=": the server's engine finds a name followed by "=" anywhere on its command line, in a quoted value too, and reads it as a switch of its own`, true
	}
	if !strings.Contains(value, " ") && strings.ContainsAny(value, ",)") {
		return `has no space, so the server reads it unquoted and ends it at "," or ")"`, true
	}
	rs := []rune(value)
	for i, r := range rs {
		if r != '-' || i+1 >= len(rs) {
			continue
		}
		if i > 0 && rs[i-1] == ' ' && rs[i+1] != ' ' {
			return `has a space followed by "-" and more, which the server would read as a switch of its own`, true
		}
		if asciiLetter(rs[i+1]) && (i == 0 || !asciiAlnum(rs[i-1])) {
			return `has "-" and a letter where no letter or digit (A-Z, a-z, 0-9) stands right before the "-", which the server's engine can take for the start of a switch; write a space after the "-", or leave it out`, true
		}
	}
	if _, found := SearchedName(value); found {
		return HoldsSearchedName, true
	}
	return "", false
}

// searched are the names the server's engine searches its command line
// for as plain text and that hold no "=", in lower case. It finds them in
// a quoted value as well as outside, most of them in the middle of a word,
// and in any case of their ASCII letters, so nothing in how a value is
// built keeps them out. (The names it searches for that way with an "="
// need no list: no value holds "=".) They are whole switches: the choice
// of the memory allocator and its checks, a trace setting, how the engine
// finds its folder, where the part of the line that came from the
// environment starts, the cache of a storage server, the overrides of a
// platform's description, an invitation of the Steam client, a relay
// setting of the Steam sockets, a frame rate for the profiler, a trace
// host, two checks of a graphics library, a debug mode of a store's kit,
// two switches handed on to the crash reporter ("/" and the name after a
// space), and a name one of the engine's own tests looks for.
//
// The list is taken from the engine the server builds are made with: the
// names its own runtime code and its Steam plugins search for on a Linux
// or a Windows host, and a few of other hosts. A name that a later engine,
// another plugin or the game itself searches for is not on it until it is
// added here. Two names that code of other plugins and platforms looks up
// are plain words ("game", "iterative") and could not be on it.
var searched = []string{
	"-ansimalloc", "-stompmalloc", "-stomp2malloc", "-binnedmalloc", "-binnedmalloc2", "-binnedmalloc3", "-mimalloc",
	"-tbbmalloc", "-libpasmalloc", "-doublefreefinder", "-tracefullcallstacks", "-basefromworkingdir", "-envafterhere",
	"-zendisablecache", "-zenenablecache", "-zeninvalidatecache", "-zenusesimplejournal", "-zennommappedstorage",
	"ddpi:", "+connect", "rail_debug_mode", "metalshadervalidation", "metalapivalidation",
	"steamsocketsrelays", "csv.targetframerateoverride", "-tracehost", "/nullrhi", "/cleancrashreports",
	"analyticsdisablecaching",
}

// SearchedNames returns the names the server's engine searches its command
// line for as plain text and that hold no "=", in lower case.
func SearchedNames() []string { return slices.Clone(searched) }

// foldASCII lowers the ASCII capital letters of s and nothing else: the
// engine compares names that way.
func foldASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// SearchedName returns the first of the names the engine searches its
// command line for as plain text (searched) that s holds anywhere, in any
// case of its ASCII letters; name is as ESM lists it, not as s has it.
// CheckValue refuses a value that holds one without saying which.
func SearchedName(s string) (name string, found bool) {
	low := foldASCII(s)
	for _, n := range searched {
		if strings.Contains(low, n) {
			return n, true
		}
	}
	return "", false
}

// SearchedSwitch reports whether the engine would take name, a rule key or
// the name of an extra argument (without its "-"), for one of the switches
// it searches for as plain text although it is another: "-ansimallocx"
// holds "-ansimalloc". A name that is such a switch itself is what it
// says, and is not reported.
func SearchedSwitch(name string) (searchedName string, found bool) {
	arg := foldASCII("-" + name)
	for _, n := range searched {
		if arg == n || arg[1:] == n {
			return "", false
		}
	}
	return SearchedName(arg)
}

// EngineLineLimit is the longest command line the server's engine starts
// with, counted in the 16-bit units it keeps text in. With a longer one it
// stops at once.
const EngineLineLimit = 16383

// MaxLineLength is the longest command line ESM lets a change lead to,
// counted the same way. It is well under EngineLineLimit: what starts the
// server may put more on the line than ESM passes (a start script the name
// of the game, say).
const MaxLineLength = 12000

// LineLength returns the length of the line the server's launcher joins
// argv into, as the engine counts it: a space before every argument, the
// two quotes the launcher puts around what holds a space, and two units
// for a character beyond U+FFFF.
func LineLength(argv []string) int {
	n := 0
	for _, a := range argv {
		n++
		if strings.Contains(a, " ") {
			n += 2
		}
		for _, r := range a {
			n++
			if r > 0xffff {
				n++
			}
		}
	}
	return n
}

// PlainSwitchName reports whether name, a rule key or the switch name of an
// extra argument (without its "-"), is ASCII letters and digits only, as
// every switch of the server is. A name with anything else in it ("x_PvP",
// "Foo.LAN", "Foo:Port") holds a second name the server would find on its
// own ("PvP=", "LAN=", "Port=").
func PlainSwitchName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// SameSwitch reports whether a and b name the same switch to the server:
// equal but for the case of ASCII letters.
func SameSwitch(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// UnsafeArgument reports whether value, a setting that goes onto the
// server command line (the server name, the world name, the map, a rule
// key or value, an extra argument), breaks one of the rules that every
// such setting has followed since ESM's first release, and says why: a
// double quote, the name MULTIHOME, or white space (EngineSpace) followed
// by "-" and more (a lone " - " is fine). An instance stored by any
// release keeps to these, so they are checked on every change of an
// instance, for all of its settings. What a value set from now on may hold
// besides is CheckValue's. A secret is never checked here: the answer may
// name what the value holds.
func UnsafeArgument(value string) (why string, unsafe bool) {
	switch {
	case strings.Contains(value, `"`):
		return `has a double quote ("), which the server's command line parsing takes for the end of a quoted value`, true
	case strings.Contains(strings.ToLower(value), switchName(MultihomeSwitch)):
		return `names MULTIHOME: ` + MultihomeSwitch + ` is never passed`, true
	}
	rs := []rune(value)
	for i := 1; i+1 < len(rs); i++ {
		if rs[i] == '-' && EngineSpace(rs[i-1]) && !EngineSpace(rs[i+1]) {
			return `has white space followed by "-" and more, which the server would read as a switch of its own`, true
		}
	}
	return "", false
}

// The engine puts this in front of every message in its log file (the
// -abslog file ESM reads): the time and frame number
// ("[2026.01.02-03.04.05:678][  0]"), then exactly one log category, then
// optionally one verbosity word, each followed by ": "
// ("LogExample: Display: "). All of it is required but the verbosity, so
// nothing a player types can stand where the words of a line ESM reads
// start: a chat line has the player's name, or a tag, between the
// category and the text. UNCONFIRMED: the categories the real lines carry;
// any name of letters, digits and "_" is accepted.
const logPrefix = logStamp + `[A-Za-z][A-Za-z0-9_]*: ` + logVerbosity

// logStamp is the time and frame at the start of every line of the log
// file, and logVerbosity the optional verbosity word after the category.
const (
	logStamp     = `^\[[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]{2}\.[0-9]{2}\.[0-9]{2}:[0-9]{3}\]\[ *[0-9]+\]`
	logVerbosity = `(?:(?:Fatal|Error|Warning|Display|Log|Verbose|VeryVerbose): )?`
)

// ServerIDCategory is the log category of the Steam logon line.
const ServerIDCategory = "LogOnline"

// The shapes of the server log lines ESM reads. Only the fixed words are
// matched; names and numbers in them are the server's own. Each must start
// its message: the same words later in a line (a repeated command line, a
// server name, a chat message) never count.
var (
	// readyLine: the server registered its dedicated session, so it is in
	// the server list and accepts players. CONFIRMED (every real run).
	// Chosen over the Steam logon line, which comes a moment earlier: a
	// server that is logged on but has no session yet cannot be joined.
	readyLine = regexp.MustCompile(logPrefix + `\[SESSION\] Dedicated session '.*' registered \(map `)
	// worldSavedLine: the server wrote its world save (at start, at every
	// autosave and at a stop), or confirmed at a stop, tagged [SHUTDOWN]
	// or [EXIT], that its last write went through. CONFIRMED for the
	// words; UNCONFIRMED whether "Wrote save" starts its message.
	worldSavedLine = regexp.MustCompile(logPrefix + `(?:Wrote save |\[(?:SHUTDOWN|EXIT)\] Synchronous write after the flush succeeded)`)
	// serverIDLine: the server logged on to Steam anonymously and names
	// the Steam id it got. CONFIRMED: the id is new at every start, and
	// the line comes from the online subsystem, under the LogOnline
	// category with the subsystem's tag ("STEAM: ") before the words. Both
	// are pinned here (the tag may also read "OSS: "), so a chat line,
	// which has the player's name after its own category, never matches.
	serverIDLine = regexp.MustCompile(logStamp + ServerIDCategory + `: ` + logVerbosity +
		`(?:STEAM|OSS): Steam game server logged on with UID: ([0-9]{1,20})(?:[^0-9A-Za-z_]|$)`)
	// resumedLine: the server found a save of its world and carries on
	// with it (instead of a new world). CONFIRMED for the words.
	resumedLine = regexp.MustCompile(logPrefix + `\[SERVER\] Resuming world `)
)

// IsReadyLine reports whether line, one whole line of the server log, is
// the server saying it accepts players.
func IsReadyLine(line string) bool { return readyLine.MatchString(line) }

// IsWorldSavedLine reports whether line, one whole line of the server log,
// is the server saying it saved the world.
func IsWorldSavedLine(line string) bool { return worldSavedLine.MatchString(line) }

// IsWorldResumedLine reports whether line, one whole line of the server
// log, is the server saying it carries on with a saved world.
func IsWorldResumedLine(line string) bool { return resumedLine.MatchString(line) }

// ServerID returns the Steam server id that line, one whole line of the
// server log, names, if it is the server's logon line. The id changes every
// time the server starts, so ESM shows it for the running server only and
// never keeps it.
func ServerID(line string) (id string, ok bool) {
	m := serverIDLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Lines of the shapes above, as the fake server writes them. Tests and the
// fake use these; ESM itself only matches lines.

// ReadyLine is a ready line for a session named name on map m with slots
// player slots.
func ReadyLine(name, m string, slots int) string {
	return fmt.Sprintf("[SESSION] Dedicated session '%s' registered (map %s, %d slots)", name, m, slots)
}

// WorldSavedLine is a save-written line for a save file of size bytes.
func WorldSavedLine(file string, size int) string {
	return fmt.Sprintf("Wrote save %s (%d bytes)", file, size)
}

// FlushedLine is the line that confirms the last write at a stop.
const FlushedLine = "[SHUTDOWN] Synchronous write after the flush succeeded."

// ServerIDLine is the message of the logon line naming the Steam id id,
// as it follows the ServerIDCategory category.
func ServerIDLine(id string) string { return "STEAM: Steam game server logged on with UID: " + id }

// ResumedLine is the line of a server that carries on with a saved world.
func ResumedLine(world string) string { return "[SERVER] Resuming world '" + world + "'" }

// NewWorldLine is the line of a server that starts a new world.
func NewWorldLine(world string) string { return "[SERVER] New world '" + world + "'" }

// ServerLogRel is where ESM points the server log (LogFileSwitch), as a
// slash path inside the instance folder. The log is outside the data
// folder, so a backup never copies it.
const ServerLogRel = "logs/server.log"

// ServerLogPath returns the server log path of an instance folder.
func ServerLogPath(instanceDir string) string {
	return filepath.Join(instanceDir, filepath.FromSlash(ServerLogRel))
}

// DataDirRel is the instance's data folder (UserDirSwitch) as a slash path
// inside the instance folder.
const DataDirRel = "data"

// DataDirPath returns the data folder of an instance folder.
func DataDirPath(instanceDir string) string {
	return filepath.Join(instanceDir, DataDirRel)
}

// StatusFileRel is the status file inside the data folder.
// UNCONFIRMED: the real server writes no status file today; ESM then
// relies on the ready log line and the query port.
const StatusFileRel = "esm-status.json"

// StatusFilePath returns the status file path for a data folder.
func StatusFilePath(dataDir string) string {
	return filepath.Join(dataDir, filepath.FromSlash(StatusFileRel))
}

// Server states written to the status file.
const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopping = "stopping"
)

// Ports is the port pair in the status file.
type Ports struct {
	Game  int `json:"game"`
	Query int `json:"query"`
}

// Status is the status file schema. UNCONFIRMED.
type Status struct {
	State    string    `json:"state"`
	PID      int       `json:"pid"`
	Ports    Ports     `json:"ports"`
	Players  int       `json:"players"`
	Version  string    `json:"version,omitempty"`
	LastSave time.Time `json:"last_save,omitzero"`
	// ServerPasswordSet and AdminPasswordSet say which passwords the server
	// took, never what they are; the token fields the same for the tokens.
	ServerPasswordSet     bool `json:"server_password_set"`
	AdminPasswordSet      bool `json:"admin_password_set"`
	AnnouncementsTokenSet bool `json:"announcements_token_set,omitempty"`
	ProblemReportTokenSet bool `json:"problem_report_token_set,omitempty"`
}

// ReadStatus reads the status file of a data folder.
func ReadStatus(dataDir string) (Status, error) {
	var s Status
	b, err := os.ReadFile(StatusFilePath(dataDir))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("gameserver: bad status file: %w", err)
	}
	return s, nil
}

// SaveDirRel is the folder, as a slash path inside the data folder, that
// holds the world saves. Every file below it belongs to the saves (the
// game keeps each world's save, its metadata and older copies there), so
// backups copy the whole folder and restores put back the whole folder.
// CONFIRMED: a real server honours UserDirSwitch and saves below this
// folder; a new build never touches it.
const SaveDirRel = "Saved/SaveGames"

// IsSaveFile reports whether rel, a slash path inside the data folder, is
// a clean path of a file below SaveDirRel.
func IsSaveFile(rel string) bool {
	return strings.HasPrefix(rel, SaveDirRel+"/") && path.Clean(rel) == rel && !strings.ContainsAny(rel, `\:`) &&
		filepath.IsLocal(filepath.FromSlash(rel))
}

// SaveFiles lists the save files of a data folder (every regular file
// below SaveDirRel; links are skipped) as slash paths relative to the data
// folder, sorted.
func SaveFiles(dataDir string) ([]string, error) {
	top := filepath.Join(dataDir, filepath.FromSlash(SaveDirRel))
	out := []string{}
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == top && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// ControlDirRel is the folder inside the data folder that the fake server
// polls for request files; the stop request file is how ESM stops a server
// on Windows. UNCONFIRMED for a real server on Windows.
const ControlDirRel = "control"

// ControlStop is the request file that asks for a polite stop.
const ControlStop = "stop"

// StopMethod is how a polite stop is asked for.
type StopMethod int

// Polite stop methods.
const (
	// StopInterrupt sends exactly one SIGINT.
	StopInterrupt StopMethod = iota
	// StopFile writes <data>/control/stop.
	StopFile
)

func (m StopMethod) String() string {
	if m == StopFile {
		return "stop request file"
	}
	return "SIGINT"
}

// PoliteStop returns the polite stop method for an OS: one SIGINT on Linux,
// the stop request file on Windows.
//
// CONFIRMED on Linux: one SIGINT makes the server save and exit cleanly
// within a few seconds (StopExitCode). A second signal while it shuts down
// may end it before the save is written, and a kill certainly does, so ESM
// sends one signal and waits; it never sends a second one and never kills
// a server on Linux. UNCONFIRMED: what SIGTERM does (never sent by ESM).
func PoliteStop(goos string) StopMethod {
	if goos == "windows" {
		return StopFile
	}
	return StopInterrupt
}

// StopExitCode is the exit code of a server that stopped cleanly after
// SIGINT. CONFIRMED.
const StopExitCode = 130

// SteamAppIDFile is the file that names the game's Steam app id. CONFIRMED:
// a real (non-Shipping) server writes it next to its binary when it starts,
// so it needs to write into the build. ESM places it there when it installs
// a build (from the configured app id), before the build is made read-only.
// UNCONFIRMED: whether the server accepts a file that is already there and
// read-only, instead of writing its own.
const SteamAppIDFile = "steam_appid.txt"

// SteamLibraries are the Steam client libraries the server loads at start.
// CONFIRMED: they are not part of a server build; the 64-bit copies from
// the Steamworks SDK redistributable sit in one folder, which the server
// finds through LD_LIBRARY_PATH.
var SteamLibraries = []string{"steamclient.so", "libsteamwebrtc.so"}

// LibraryPathVar is the variable the server finds SteamLibraries through.
const LibraryPathVar = "LD_LIBRARY_PATH"

// LibraryPath returns the LD_LIBRARY_PATH value for a server: the Steam
// runtime folder first, then what was set before (if anything).
func LibraryPath(runtimeDir, inherited string) string {
	if inherited == "" {
		return runtimeDir
	}
	return strings.Join([]string{runtimeDir, inherited}, ":")
}

// LoopbackLocalAddr returns the local UDP address to send from when talking
// to host: the same loopback address when host is a loopback address, so
// the sending socket is bound to loopback only and never to all
// interfaces; nil (the system's choice) otherwise.
func LoopbackLocalAddr(host string) *net.UDPAddr {
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil
	}
	return &net.UDPAddr{IP: ip}
}

// StopRequestPath returns the stop request file of a data folder.
func StopRequestPath(dataDir string) string {
	return filepath.Join(dataDir, ControlDirRel, ControlStop)
}

// PortProtocols are the protocols the query port is reached with.
// CONFIRMED: UDP.
var PortProtocols = []string{"udp"}

// InboundPorts says which of an instance's ports must be reachable from
// outside the box. CONFIRMED: only the query port. The game port has no
// socket at all: players join through Steam's relay, over outbound UDP.
// ESM still gives every instance its own game port, which the server is
// told and puts in its advert. UNCONFIRMED: two or more servers on one box,
// told apart by their ports only, and a query port other than the default.
const InboundPorts = "query"
