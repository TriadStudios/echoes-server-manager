package gameserver

import (
	"encoding/json"
	"math"
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
		"-ServerPassword=a b c -Port=1":                           "-ServerPassword=******** -Port=1",
		"-ServerPassword=a b c":                                   "-ServerPassword=********",
		`argv: -AdminPassword=x $&"';%,=<> y -log -SavedDir=/d`:   "argv: -AdminPassword=******** -log -SavedDir=/d",
		"-ServerPassword=a, b -AdminPassword=c;d -MaxPlayers=4":   "-ServerPassword=******** -AdminPassword=******** -MaxPlayers=4",
		"-serverpassword=lower case  -ServerName=keep":            "-serverpassword=********  -ServerName=keep",
		"--password=two dashes -x":                                "--password=******** -x",
		`["-ServerPassword=q w" "-log"]`:                          `["-ServerPassword=******** "-log"]`,
		"-ServerPassword=one line\n-Port=2 next line":             "-ServerPassword=********\n-Port=2 next line",
		"-ServerPassword=ends at\r\nthe line":                     "-ServerPassword=********\r\nthe line",
		"-ServerPassword=":                                        "-ServerPassword=",
		"-ServerPassword=-1 -Port=1":                              "-ServerPassword=******** -Port=1",
		"-ServerPassword=a -1 -2 -Port=1":                         "-ServerPassword=******** -Port=1",
		"url?password=abc&x=1":                                    "url?password=********&x=1",
		"admin_password=s3cret next":                              "admin_password=******** next",
		"-ServerName=password=x":                                  "-ServerName=password=********",
		"no switch here: -ServerName=open -Port=7777":             "no switch here: -ServerName=open -Port=7777",
		"rules: value of AdminPassword must be printable":         "rules: value of AdminPassword must be printable",
		"-AnnouncementsToken=abc -ProblemReportToken=d e -Port=1": "-AnnouncementsToken=******** -ProblemReportToken=******** -Port=1",
		"-problemreporttoken=x":                                   "-problemreporttoken=********",
		"-Password=join me -AdminPassword=x -Port=1":              "-Password=******** -AdminPassword=******** -Port=1",
		"the token is not set (0 chars)":                          "the token is not set (0 chars)",
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
//
// The test compares growth, not a fixed time, so a slow machine or the race
// detector does not fail it: four times the text may take at most eight
// times as long. Work in proportion to the text takes about four times as
// long, work that grows with the square of its length sixteen times.
func TestSwitchRuleTakesLinearTime(t *testing.T) {
	const n = 32 << 10
	for _, c := range []struct {
		name string
		text func(n int) string
		want string // the masked text, when it is short
	}{
		{"spaces", func(n int) string { return "-password=" + strings.Repeat(" ", n) + "x" }, "-password=" + Mask},
		{"spaces and tabs", func(n int) string { return "-ServerPassword=a" + strings.Repeat(" \t", n/2) + "b" }, "-ServerPassword=" + Mask},
		{"many switches", func(n int) string { return strings.Repeat("-AdminPassword= ", n/16) }, ""},
	} {
		small, large := c.text(n), c.text(4*n)
		if c.want != "" {
			if got := ScrubLine(large, nil); got != c.want {
				t.Fatalf("%s: value not masked: %q...", c.name, got[:min(len(got), 40)])
			}
		}
		var ratio float64
		for range 2 { // once more if the machine was busy
			ratio = float64(perCall(t, large)) / float64(perCall(t, small))
			t.Logf("%s: %.1f times as long for four times the text", c.name, ratio)
			if ratio <= 8 {
				break
			}
		}
		if ratio > 8 {
			t.Errorf("%s: masking %d bytes took %.1f times as long as %d bytes, want about 4", c.name, len(large), ratio, len(small))
		}
	}
}

// perCall is the shortest time ScrubLine took on text, per call, over three
// rounds of calls lasting at least 20 ms each. A call that takes about 10 s
// fails the test at once.
func perCall(t *testing.T, text string) time.Duration {
	t.Helper()
	best := time.Duration(math.MaxInt64)
	for range 3 {
		calls, start := 0, time.Now()
		var d time.Duration
		for d < 20*time.Millisecond {
			ScrubLine(text, nil)
			calls++
			if d = time.Since(start); d > 10*time.Second {
				t.Fatalf("masking %d bytes took %s", len(text), d)
			}
		}
		best = min(best, d/time.Duration(calls))
	}
	return best
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
