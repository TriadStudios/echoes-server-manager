// Package cmdline reads a game server command line the way the game's
// engine reads it. Tests and the fake server use it to see what a real
// server makes of the arguments ESM passes: checking the arguments one by
// one is not enough, because the server never sees them one by one. ESM
// itself never parses a command line.
//
// How the engine reads its command line:
//
//   - Its launcher joins the arguments (all but the program) into one line,
//     each after a space. An argument that holds a space (only the plain
//     space counts) is put in double quotes: after its first "=" when it
//     starts with "-" and has an "=", whole otherwise. Nothing else is
//     quoted or escaped.
//   - A value is looked up as "Name=": without regard to the case of ASCII
//     letters (any other character must match exactly), only where the
//     character before the name is not an ASCII letter or digit, and never
//     inside quotes (every double quote starts or ends a quoted part). The
//     first match counts.
//   - A value that starts with a double quote runs to the next one. Any
//     other value starts after the white space that follows the "=", and
//     ends at the first ",", ")" or white space. So an empty value that is
//     not quoted takes the next argument. Here the engine names the white
//     space itself: space, tab, carriage return and line feed, on every
//     host.
//   - A bare switch is "-Name" or "/Name" outside quotes, at the start of
//     the line or after white space, and followed by white space or the end
//     of the line. A setting read as a bool is on when it is there and no
//     "Name=" is found. Here the engine asks its host what white space is,
//     and the hosts do not agree: Linux counts the ASCII ones, Windows a
//     good deal more. Space is every character that any host counts, so
//     this reader finds every bare switch that some host finds.
//   - Some names the engine does not look up this way at all: it searches
//     the line, or the arguments one by one before they are joined, for
//     them as plain text, inside quotes too and mostly without regard to
//     what stands before or after them. Searched lists them and Found finds
//     them.
//   - The engine does not start with a line of more than Limit characters,
//     nor with an en dash anywhere on it (see Refuses).
package cmdline

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Join joins argv, the arguments without the program, into the line the
// server reads.
func Join(argv []string) string {
	var b strings.Builder
	for _, a := range argv {
		b.WriteByte(' ')
		if strings.Contains(a, " ") {
			at := 0
			if strings.HasPrefix(a, "-") {
				if i := strings.IndexByte(a, '='); i >= 0 {
					at = i + 1
				}
			}
			a = a[:at] + `"` + a[at:] + `"`
		}
		b.WriteString(a)
	}
	return b.String()
}

// Limit is the longest line the engine starts with, counted in the 16-bit
// units it keeps its text in: a character beyond U+FFFF counts twice.
const Limit = 16383

// Units returns the length of line as the engine counts it.
func Units(line string) int {
	n := 0
	for _, r := range line {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// Refuses says why the engine does not start with line, or "" when it
// does: the line is longer than Limit, or it holds an en dash (U+2013),
// which the engine takes for a "-" spoilt on its way through a mail
// program and stops at.
func Refuses(line string) string {
	switch {
	case Units(line) > Limit:
		return "the command line is longer than " + strconv.Itoa(Limit) + " characters"
	case strings.ContainsRune(line, '\u2013'):
		return "the command line holds an en dash (U+2013)"
	}
	return ""
}

// spaces are the characters that some host of the engine counts as white
// space, as ranges of code points (first and last). Windows counts every
// one of them; Linux, where the engine runs in the "C" locale, only the
// ASCII ones (U+0009 to U+000D and U+0020).
var spaces = [][2]rune{
	{0x0009, 0x000d}, {0x0020, 0x0020}, {0x0085, 0x0085}, {0x00a0, 0x00a0}, {0x1680, 0x1680}, {0x180e, 0x180e},
	{0x2000, 0x200a}, {0x2028, 0x2029}, {0x202f, 0x202f}, {0x205f, 0x205f}, {0x3000, 0x3000},
}

// Space reports whether some host of the engine counts r as white space.
func Space(r rune) bool {
	for _, s := range spaces {
		if s[0] <= r && r <= s[1] {
			return true
		}
	}
	return false
}

// alnum reports whether c is an ASCII letter or digit. Bytes of a
// character beyond ASCII are neither, as the character itself is not.
func alnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// fold lowers an ASCII capital letter; every other byte stays.
func fold(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// matchAt reports whether pat stands at line[i:], ignoring the case of
// ASCII letters.
func matchAt(line string, i int, pat string) bool {
	if len(line)-i < len(pat) {
		return false
	}
	for k := 0; k < len(pat); k++ {
		if fold(line[i+k]) != fold(pat[k]) {
			return false
		}
	}
	return true
}

// find returns where pat first stands in line at or after from, outside
// quotes and after a character that is not an ASCII letter or digit, or -1.
// The search knows nothing of what comes before from: it starts outside
// quotes and as if after such a character, as the engine's does.
func find(line, pat string, from int) int {
	quoted, afterAlnum := false, false
	for i := from; i < len(line); i++ {
		c := line[i]
		if !quoted && !afterAlnum && matchAt(line, i, pat) {
			return i
		}
		afterAlnum = alnum(c)
		if c == '"' {
			quoted = !quoted
		}
	}
	return -1
}

// valueAt reads the value that starts at line[i:], right after an "=".
func valueAt(line string, i int) string {
	if i < len(line) && line[i] == '"' {
		rest := line[i+1:]
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	for i < len(line) && strings.IndexByte(" \t\r\n", line[i]) >= 0 {
		i++
	}
	rest := line[i:]
	if end := strings.IndexAny(rest, ",) \t\r\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// Value returns the value the server reads for name ("Port" for "Port="),
// and whether it finds name at all.
func Value(line, name string) (string, bool) {
	pat := name + "="
	i := find(line, pat, 0)
	if i < 0 {
		return "", false
	}
	return valueAt(line, i+len(pat)), true
}

// spaceAt reports whether the character at line[i:] is white space.
func spaceAt(line string, i int) bool {
	r, _ := utf8.DecodeRuneInString(line[i:])
	return Space(r)
}

// spaceBefore reports whether the character that ends right before
// line[i] is white space.
func spaceBefore(line string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(line[:i])
	return Space(r)
}

// Param reports whether the bare switch -name (or /name) is in line.
func Param(line, name string) bool {
	if name == "" {
		return false
	}
	for from := 0; ; {
		i := find(line, name, from)
		if i < 0 {
			return false
		}
		if i > 0 && (line[i-1] == '-' || line[i-1] == '/') && (i == 1 || spaceBefore(line, i-1)) {
			if end := i + len(name); end == len(line) || spaceAt(line, end) {
				return true
			}
		}
		from = i + 1
	}
}

// Bool reads name as a setting that is on or off: the value of "name="
// when there is one, else on when the bare switch is there. set says
// whether either is in line.
func Bool(line, name string) (on, set bool) {
	if v, ok := Value(line, name); ok {
		return toBool(v), true
	}
	if Param(line, name) {
		return true, true
	}
	return false, false
}

// toBool reads a bool value as the engine does: true, yes and on in any
// case are true, false, no and off are false, and anything else is true
// when it starts with a number other than 0.
func toBool(v string) bool {
	for _, w := range []string{"true", "yes", "on"} {
		if SameName(v, w) {
			return true
		}
	}
	for _, w := range []string{"false", "no", "off"} {
		if SameName(v, w) {
			return false
		}
	}
	end := 0
	for end < len(v) && (v[end] >= '0' && v[end] <= '9' || end == 0 && (v[end] == '-' || v[end] == '+')) {
		end++
	}
	n, _ := strconv.Atoi(v[:end])
	return n != 0
}

// Switch is a place in a command line where the server finds a switch with
// a value: Name and "=" outside quotes, after a character that is not an
// ASCII letter or digit (or at the start of the line).
type Switch struct {
	// Name is the run of ASCII letters and digits before the "=".
	Name string
	// Value is what the server reads there.
	Value string
	// At is where Name starts in the line.
	At int
}

// Switches lists, in order, every place in line where the server finds a
// switch with a value whose name is ASCII letters and digits: every name
// the server could look up there.
func Switches(line string) []Switch {
	var out []Switch
	quoted, afterAlnum := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if !quoted && !afterAlnum && alnum(c) {
			j := i
			for j < len(line) && alnum(line[j]) {
				j++
			}
			if j < len(line) && line[j] == '=' {
				out = append(out, Switch{Name: line[i:j], Value: valueAt(line, j+1), At: i})
			}
		}
		afterAlnum = alnum(c)
		if c == '"' {
			quoted = !quoted
		}
	}
	return out
}

// SameName reports whether a and b name the same switch to the server:
// equal but for the case of ASCII letters.
func SameName(a, b string) bool {
	return len(a) == len(b) && matchAt(a, 0, b)
}

// Searched are the names the engine searches its command line for as
// plain text, in lower case; the engine ignores the case of their ASCII
// letters. It finds them inside a quoted value as well as outside, and most
// of them in the middle of a word. They are few and fixed in the engine:
//
//   - The ones that end in "=", or hold one, read what follows as their
//     value: settings of the memory system that Linux hosts read from each
//     argument before the line is joined, the trace switch that is found
//     after any "-" in any argument, the folder to load plugins from (found
//     at the start of a word, quotes or not), the switches that are cut out
//     of the line as plain text once one of them is there, and a few that
//     only tell the engine what kind of run it is.
//   - The others are whole switches: the choice of the memory allocator and
//     its checks, how the engine finds its folder, where the part of the
//     line that came from the environment starts, the cache of a storage
//     server, the overrides of a platform's description, an invitation of
//     the Steam client, a relay setting of the Steam sockets, a frame rate
//     for the profiler, a trace host, what is handed on to the crash
//     reporter from a single argument ("/" and the name after a space), and
//     a name one of the engine's own tests looks for.
//
// These are the names that the engine's own runtime code and its Steam
// plugins search for on a Linux or a Windows host, and a few of other
// hosts. Names that only code of other plugins or platforms looks up are
// not here; two of those are plain words ("game", "iterative").
var Searched = []string{
	"-sigdfl=", "-crashhandlerstacksize=", "-filemapcachesize=", "-vmapoolscale=", "-trace=", "plugin=",
	"-logcmds=", "-statcmds=", "-cmdlinefile=", "-inifile=", "abscrashreportclientlog=",
	"-run=iostore", "-compressionformats=oodle", "-compressionformat=oodle", "-dlcname=", "run=deriveddatacache",

	"-ansimalloc", "-stompmalloc", "-stomp2malloc", "-binnedmalloc", "-binnedmalloc2", "-binnedmalloc3", "-mimalloc",
	"-tbbmalloc", "-libpasmalloc", "-doublefreefinder", "-tracefullcallstacks", "-basefromworkingdir", "-envafterhere",
	"-zendisablecache", "-zenenablecache", "-zeninvalidatecache", "-zenusesimplejournal", "-zennommappedstorage",
	"ddpi:", "+connect", "rail_debug_mode", "metalshadervalidation", "metalapivalidation",
	"steamsocketsrelays", "csv.targetframerateoverride", "-tracehost", "/nullrhi", "/cleancrashreports",
	"analyticsdisablecaching",
}

// Found returns the names of Searched that stand in an argument of argv,
// each once, in the order of Searched. It is plain text search, as the
// engine's: an argument that is the switch itself ("-ansimalloc") holds its
// name too.
func Found(argv []string) []string {
	var out []string
	for _, name := range Searched {
		for _, a := range argv {
			if holds(a, name) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// holds reports whether name stands anywhere in s, ignoring the case of
// ASCII letters.
func holds(s, name string) bool {
	for i := 0; i+len(name) <= len(s); i++ {
		if matchAt(s, i, name) {
			return true
		}
	}
	return false
}
