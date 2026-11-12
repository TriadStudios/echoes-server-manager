package gameserver

import (
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The value of a password switch is masked up to the next switch or the end
// of the line, whatever it contains.
func TestSwitchRuleMasksWholeValues(t *testing.T) {
	cases := map[string]string{
		"-ServerPassword=a b c -Port=1":                         "-ServerPassword=******** -Port=1",
		"-ServerPassword=a b c":                                 "-ServerPassword=********",
		`argv: -AdminPassword=x $&"';%,=<> y -log -SavedDir=/d`: "argv: -AdminPassword=******** -log -SavedDir=/d",
		"-ServerPassword=a, b -AdminPassword=c;d -MaxPlayers=4": "-ServerPassword=******** -AdminPassword=******** -MaxPlayers=4",
		"-serverpassword=lower case  -ServerName=keep":          "-serverpassword=********  -ServerName=keep",
		"--password=two dashes -x":                              "--password=******** -x",
		`["-ServerPassword=q w" "-log"]`:                        `["-ServerPassword=******** "-log"]`,
		"-ServerPassword=one line\n-Port=2 next line":           "-ServerPassword=********\n-Port=2 next line",
		"-ServerPassword=ends at\r\nthe line":                   "-ServerPassword=********\r\nthe line",
		"-ServerPassword=":                                      "-ServerPassword=",
		"-ServerPassword=-1 -Port=1":                            "-ServerPassword=******** -Port=1",
		"-ServerPassword=a -1 -2 -Port=1":                       "-ServerPassword=******** -Port=1",
		"url?password=abc&x=1":                                  "url?password=********&x=1",
		"admin_password=s3cret next":                            "admin_password=******** next",
		"-ServerName=password=x":                                "-ServerName=password=********",
		"no switch here: -ServerName=open -Port=7777":           "no switch here: -ServerName=open -Port=7777",
		"rules: value of AdminPassword must be printable":       "rules: value of AdminPassword must be printable",
	}
	for in, want := range cases {
		if got := ScrubLine(in, nil); got != want {
			t.Errorf("ScrubLine(%q) = %q, want %q", in, got, want)
		}
		if got := ScrubLine(ScrubLine(in, nil), nil); got != want {
			t.Errorf("ScrubLine twice on %q = %q, want %q", in, got, want)
		}
	}
}

// Known values are hidden wherever they appear, in every form ESM writes
// them in: as they are, with systemd's doubled dollars, JSON-escaped and
// Go-quoted.
func TestScrubberHidesEveryForm(t *testing.T) {
	const pw = `p$ss w$rd &"';%,=<> \x`
	quoted := strconv.Quote(pw)
	escaped, _ := json.Marshal(pw)
	for what, text := range map[string]string{
		"raw":              "server said " + pw + " twice: " + pw,
		"systemd":          "Failed to start transient service unit: invalid argument " + EscapeForSystemd(pw),
		"json":             `{"line":` + string(escaped) + `}`,
		"go quoted":        "argv " + quoted,
		"quoted systemd":   "argv " + strconv.Quote(EscapeForSystemd(pw)),
		"value of switch":  "-ServerPassword=" + pw + " -Port=1",
		"next to each oth": pw + pw,
	} {
		got := NewScrubber([]string{pw}).Scrub(text)
		for _, piece := range []string{"p$ss", "p$$ss", "w$rd", "w$$rd", `&"';%,=<>`, `\x`, `\\x`, `&`} {
			if strings.Contains(got, piece) {
				t.Errorf("%s: %q is left in %q", what, piece, got)
			}
		}
		if !strings.Contains(got, Mask) {
			t.Errorf("%s: nothing masked in %q", what, got)
		}
	}
}

// A value is also hidden with the doubled dollars systemd needs: older
// systemd versions get every "$" of an argument as "$$", and its messages
// repeat them that way.
func TestScrubLineCoversSystemdEscaping(t *testing.T) {
	const pw = "open $esame $$ 42"
	text := "systemd-run: invalid argument: " + strings.ReplaceAll(pw, "$", "$$")
	got := ScrubLine(text, []string{pw})
	if strings.Contains(got, "esame") || !strings.Contains(got, Mask) {
		t.Fatalf("the systemd form of a password is not hidden: %q", got)
	}
	if got := ScrubLine("nothing of that kind here", []string{pw}); got != "nothing of that kind here" {
		t.Fatalf("got %q", got)
	}
}

// Overlapping or touching values leave no piece between their masks, and
// the longer of two nested values is hidden whole.
func TestScrubberOverlapsAndNesting(t *testing.T) {
	sc := NewScrubber([]string{"abcdef", "defxyz", "secret", "secret12"})
	for in, want := range map[string]string{
		"x abcdefxyz y":        "x " + Mask + " y",
		"x abcdefdefxyz":       "x " + Mask,
		"secret12 and secret1": Mask + " and " + Mask + "1",
		"nothing":              "nothing",
	} {
		if got := sc.Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// Values shorter than MinHiddenLength are not hidden as text (they would
// garble it), but still masked after a password switch.
func TestScrubberShortValues(t *testing.T) {
	sc := NewScrubber([]string{"abc", ""})
	if got := sc.Scrub("abc is a word"); got != "abc is a word" {
		t.Fatalf("a short value was hidden as text: %q", got)
	}
	if got := sc.Scrub("-AdminPassword=abc -log"); got != "-AdminPassword="+Mask+" -log" {
		t.Fatalf("a short value after a switch: %q", got)
	}
	var none *Scrubber
	if got := none.Scrub("-AdminPassword=abc def"); got != "-AdminPassword="+Mask {
		t.Fatalf("nil scrubber: %q", got)
	}
}

// Masking a switch value takes time in proportion to the text, however
// much white space it holds: a long run of spaces inside a value must not
// be scanned again from every space.
func TestSwitchRuleTakesLinearTime(t *testing.T) {
	for _, in := range []string{
		"-password=" + strings.Repeat(" ", 400000) + "x",
		"-ServerPassword=a" + strings.Repeat(" \t", 200000) + "b",
		strings.Repeat("-AdminPassword= ", 50000),
	} {
		start := time.Now()
		out := ScrubLine(in, nil)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("masking %d bytes took %s", len(in), d)
		}
		if strings.Contains(out, "x") && strings.HasPrefix(in, "-password=") {
			t.Fatalf("value not masked: %q...", out[:40])
		}
	}
}

// The end of a switch value is found as the first version of the rule
// found it, on every short mix of the characters that matter.
func TestSwitchValueEndMatchesTheRule(t *testing.T) {
	// slowEnd is the rule written plainly: before the first white space
	// (not at the start) that is followed by more white space, an optional
	// quote, "-" and a letter; or at a line end.
	slowEnd := func(v string) int {
		for i := 0; i < len(v); i++ {
			c := v[i]
			if c == '\n' || c == '\r' {
				return i
			}
			if i > 0 && (c == ' ' || c == '\t') {
				rest := strings.TrimLeft(v[i:], " \t")
				if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
					rest = rest[1:]
				}
				if len(rest) >= 2 && rest[0] == '-' && ('a' <= rest[1] && rest[1] <= 'z' || 'A' <= rest[1] && rest[1] <= 'Z') {
					return i
				}
			}
		}
		return len(v)
	}
	alphabet := []byte{' ', '\t', '-', 'a', 'Z', '1', '"', '\'', '\n', '='}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200000 {
		b := make([]byte, rng.IntN(12))
		for i := range b {
			b[i] = alphabet[rng.IntN(len(alphabet))]
		}
		if got, want := switchValueEnd(string(b)), slowEnd(string(b)); got != want {
			t.Fatalf("switchValueEnd(%q) = %d, want %d", b, got, want)
		}
	}
}
