package gameserver

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// This file is the output filter: a second line of defence, never the
// guarantee. By design a server gets its passwords in a file and its output
// holds none; with command-line delivery its output does hold them and is
// kept from the read role altogether. The filter masks what it knows in
// what a server or a supervisor's tool wrote, for the case that a password
// turns up there anyway.

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

// Longest is the length of the longest form of a hidden value (0 for
// none): a piece of text cut anywhere ends in at most that many bytes of
// a split value.
func (sc *Scrubber) Longest() int {
	if sc == nil || len(sc.forms) == 0 {
		return 0
	}
	return len(sc.forms[0])
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

// ScrubLine hides values (as a Scrubber does) and then masks the value of
// every password switch. With no values it is the switch rule alone.
func ScrubLine(line string, values []string) string {
	return NewScrubber(values).Scrub(line)
}

var (
	// switchKey finds a password switch: a "-" at the start of the text or
	// after a character that cannot be part of a word, then a key that
	// contains "password" or "token" (any case), then "=". The token
	// switches are the server's announcements and problem report tokens.
	switchKey = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])(-[A-Za-z0-9_.-]*(?:password|token)[A-Za-z0-9_.-]*=)`)
	// pairKey finds other key=value pairs whose key contains "password" (a
	// URL query, a config line). Their value ends at white space, a quote,
	// "&", "," or ";".
	pairKey = regexp.MustCompile(`(?i)([A-Za-z0-9_-]*password[A-Za-z0-9_]*=)[^\s"'&,;]+`)
)

// MaskSwitches masks the value of every password or token switch in s,
// such as -ServerPassword=... or -AnnouncementsToken=..., up to the next
// switch (white space, an optional quote, "-" and a letter) or the end of
// the line, so a value with spaces or punctuation is masked whole:
// "-ServerPassword=a b c -Port=1" becomes "-ServerPassword=******** -Port=1".
// The value of any other key=value pair whose key contains "password" is
// masked up to white space or a quote, "&", "," or ";".
func MaskSwitches(s string) string {
	if low := strings.ToLower(s); !strings.Contains(low, "password") && !strings.Contains(low, "token") {
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
//
// It looks at every byte once: a run of white space is skipped in one go
// and the switch test is made once, at its end.
func switchValueEnd(v string) int {
	for i := 0; i < len(v); {
		c := v[i]
		switch {
		case c == '\n' || c == '\r':
			return i
		case i > 0 && isBlank(c):
			j := i + 1
			for j < len(v) && isBlank(v[j]) {
				j++
			}
			if startsSwitch(v[j:]) {
				return i
			}
			i = j
			continue
		}
		i++
	}
	return len(v)
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' }

// startsSwitch reports whether rest is an optional quote, "-" and a letter.
func startsSwitch(rest string) bool {
	if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
		rest = rest[1:]
	}
	if len(rest) < 2 || rest[0] != '-' {
		return false
	}
	c := rest[1]
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
