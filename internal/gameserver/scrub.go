package gameserver

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MinHiddenLength is the shortest secret that is hidden wherever its value
// appears; shorter values would garble ordinary text. Short values are
// still masked where they follow a password switch (see MaskSwitches).
const MinHiddenLength = 4

// EscapeForSystemd doubles every "$" so that systemd's variable expansion
// leaves a command-line argument as it is. The systemd supervisor uses it
// on systemd versions that cannot turn the expansion off, so a Scrubber
// hides this form of every secret too.
func EscapeForSystemd(a string) string { return strings.ReplaceAll(a, "$", "$$") }

// Forms returns v and the other spellings v takes in text that ESM writes
// or hands on: with doubled dollars (systemd), JSON-escaped (with and
// without HTML escaping) and Go-quoted, each without the surrounding
// quotes. The list has no duplicates.
func Forms(v string) []string {
	var out []string
	add := func(f string) {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	for _, base := range []string{v, EscapeForSystemd(v)} {
		add(base)
		add(jsonInner(base, true))
		add(jsonInner(base, false))
		q := strconv.Quote(base)
		add(q[1 : len(q)-1])
	}
	return out
}

// jsonInner is v as a JSON string without its quotes.
func jsonInner(v string, escapeHTML bool) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(v); err != nil {
		return v
	}
	s := strings.TrimSuffix(b.String(), "\n")
	return s[1 : len(s)-1]
}

// Scrubber hides a fixed set of secret values in text. It is safe for
// concurrent use; a nil Scrubber hides no values but still applies the
// password switch rule.
type Scrubber struct {
	values []string // the values hidden, without duplicates
	forms  []string // every form of every value, longest first
}

// NewScrubber returns a Scrubber for values. Values shorter than
// MinHiddenLength are left to the password switch rule.
func NewScrubber(values []string) *Scrubber {
	sc := &Scrubber{}
	for _, v := range values {
		if len(v) < MinHiddenLength || slices.Contains(sc.values, v) {
			continue
		}
		sc.values = append(sc.values, v)
		for _, f := range Forms(v) {
			if !slices.Contains(sc.forms, f) {
				sc.forms = append(sc.forms, f)
			}
		}
	}
	slices.SortStableFunc(sc.forms, func(a, b string) int { return len(b) - len(a) })
	return sc
}

// With returns a Scrubber that hides the values of sc and values.
func (sc *Scrubber) With(values []string) *Scrubber {
	if sc == nil {
		return NewScrubber(values)
	}
	return NewScrubber(append(slices.Clone(sc.values), values...))
}

// Scrub returns text with every occurrence of a secret value, in any of its
// forms, replaced by Mask, and then the value of every password switch
// masked (see MaskSwitches). Occurrences that overlap or touch become one
// mask, so no piece of a value is left between two masks.
func (sc *Scrubber) Scrub(text string) string {
	return MaskSwitches(sc.hide(text))
}

func (sc *Scrubber) hide(s string) string {
	if sc == nil || len(sc.forms) == 0 || s == "" {
		return s
	}
	var hit []bool // lazily: which bytes of s belong to a secret
	for _, f := range sc.forms {
		for i := 0; i < len(s); {
			j := strings.Index(s[i:], f)
			if j < 0 {
				break
			}
			if hit == nil {
				hit = make([]bool, len(s))
			}
			start := i + j
			for k := start; k < start+len(f); k++ {
				hit[k] = true
			}
			i = start + 1
		}
	}
	if hit == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if !hit[i] {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(Mask)
		for i < len(s) && hit[i] {
			i++
		}
	}
	return b.String()
}

// HideValues replaces every occurrence of each value (of at least
// MinHiddenLength bytes), in any of its forms, with Mask.
func HideValues(s string, values []string) string {
	return NewScrubber(values).hide(s)
}

// ScrubLine hides values (as a Scrubber does) and then masks the value of
// every password switch. With no values it is the switch rule alone.
func ScrubLine(line string, values []string) string {
	return NewScrubber(values).Scrub(line)
}

var (
	// switchKey finds a password switch: a "-" at the start of the text or
	// after a character that cannot be part of a word, then a key that
	// contains "password" (any case), then "=".
	switchKey = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])(-[A-Za-z0-9_.-]*password[A-Za-z0-9_.-]*=)`)
	// pairKey finds other key=value pairs whose key contains "password" (a
	// URL query, a config line). Their value ends at white space, a quote,
	// "&", "," or ";".
	pairKey = regexp.MustCompile(`(?i)([A-Za-z0-9_-]*password[A-Za-z0-9_]*=)[^\s"'&,;]+`)
)

// MaskSwitches masks the value of every password switch in s, such as
// -ServerPassword=..., up to the next switch (white space, an optional
// quote, "-" and a letter) or the end of the line, so a value with spaces or
// punctuation is masked whole: "-ServerPassword=a b c -Port=1" becomes
// "-ServerPassword=******** -Port=1". The value of any other key=value pair
// whose key contains "password" is masked up to white space or a quote,
// "&", "," or ";".
func MaskSwitches(s string) string {
	if !strings.Contains(strings.ToLower(s), "password") {
		return s
	}
	var b strings.Builder
	pos := 0
	for pos < len(s) {
		loc := switchKey.FindStringSubmatchIndex(s[pos:])
		if loc == nil {
			break
		}
		valStart := pos + loc[3]
		valEnd := valStart + switchValueEnd(s[valStart:])
		b.WriteString(s[pos:valStart])
		if valEnd > valStart {
			b.WriteString(Mask)
		}
		pos = valEnd
	}
	b.WriteString(s[pos:])
	return pairKey.ReplaceAllString(b.String(), "${1}"+Mask)
}

// switchValueEnd returns where the value at the start of v ends: before the
// white space that starts the next switch, or at the end of the line. White
// space right at the start does not end the value.
func switchValueEnd(v string) int {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '\n' || c == '\r':
			return i
		case i > 0 && (c == ' ' || c == '\t') && startsSwitch(v[i:]):
			return i
		}
	}
	return len(v)
}

// startsSwitch reports whether rest is white space, an optional quote, "-"
// and a letter.
func startsSwitch(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
		rest = rest[1:]
	}
	if len(rest) < 2 || rest[0] != '-' {
		return false
	}
	c := rest[1]
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
