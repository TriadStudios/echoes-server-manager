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
//     not quoted takes the next argument.
//   - A bare switch is "-Name" or "/Name" outside quotes, at the start of
//     the line or after white space, and followed by white space or the end
//     of the line. A setting read as a bool is on when it is there and no
//     "Name=" is found.
package cmdline

import (
	"strconv"
	"strings"
	"unicode"
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
	return unicode.IsSpace(r)
}

// spaceBefore reports whether the character that ends right before
// line[i] is white space.
func spaceBefore(line string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(line[:i])
	return unicode.IsSpace(r)
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
