package page

import (
	"fmt"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/sysinfo"
)

// funcs are the helpers the templates use. They only format what the API
// returned.
var funcs = template.FuncMap{
	"size":     sizeOf,
	"sizep":    sizeOfPtr,
	"when":     when,
	"iso":      func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"since":    since,
	"state":    stateWord,
	"warning":  func(code string, stored []string) string { return sentence(instances.WarningWords(code, stored)) },
	"why":      backupWhy,
	"players":  playersOf,
	"memory":   memoryOf,
	"memstep":  memoryStep,
	"unknown":  unknownWhy,
	"has":      func(list []string, s string) bool { return contains(list, s) },
	"join":     strings.Join,
	"ints":     joinInts,
	"every":    everyWords,
	"choices":  backupChoices,
	"yesno":    func(b bool) string { return map[bool]string{true: "yes", false: "no"}[b] },
	"delivery": deliveryWords,
	"active":   func(s instances.State) bool { return s == instances.Running || s == instances.Starting },
	"idle": func(s instances.State) bool {
		return s == instances.Stopped || s == instances.Crashed || s == instances.Failed
	},
	"check":    checkWord,
	"times":    func(list []string) string { return strings.Join(list, ", ") },
	"notempty": func(s []string) bool { return len(s) > 0 },
	"plural":   plural,
	"loopback": func(listen string) bool {
		host, _, err := config.SplitListen(listen)
		return err == nil && config.IsLoopback(host)
	},
}

// sentence starts s with a capital letter and ends it with a period.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// plural is "1 server" or "2 servers".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sizeOf writes a byte count in binary units, like "6.1 GiB".
func sizeOf(n int64) string {
	if n < 0 {
		return "-" + sizeOf(-n)
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	f := "%.1f %ciB"
	if v >= 100 {
		f = "%.0f %ciB"
	}
	return fmt.Sprintf(f, v, "KMGTP"[exp])
}

func sizeOfPtr(n *int64) string {
	if n == nil {
		return "unknown"
	}
	return sizeOf(*n)
}

// step is part of whole as a bar length in steps of 5 percent (0 to 100).
func step(part, whole int64) int {
	if whole <= 0 || part <= 0 {
		return 0
	}
	if part >= whole {
		return 100
	}
	return int((part*100/whole + 2) / 5 * 5)
}

// when writes a time in UTC; the script shows it in the reader's own time.
func when(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// since is how long ago t was, roughly, like "2 d 4 h".
func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d d %d h", int(d.Hours())/24, int(d.Hours())%24)
}

func stateWord(s instances.State) string {
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(string(s[:1])) + string(s[1:])
}

func backupWhy(reason string) string {
	switch reason {
	case instances.ReasonManual:
		return "Made by hand"
	case instances.ReasonScheduled:
		return "Schedule"
	case instances.ReasonPreUpdate:
		return "Before an update"
	case instances.ReasonPreRestore:
		return "Before a restore"
	}
	return reason
}

// playersOf is "players / max", with "?" when the count is not known.
func playersOf(v instances.View) string {
	n := "?"
	if v.Live.Players != nil {
		n = strconv.Itoa(*v.Live.Players)
	}
	return n + " / " + strconv.Itoa(v.MaxPlayers)
}

// memoryOf is the server's memory against its ceiling.
func memoryOf(v instances.View) string {
	ceiling, _ := instances.ParseMemory(v.Limits.MemoryMax)
	used := "unknown"
	if v.Live.MemoryBytes != nil {
		used = sizeOf(*v.Live.MemoryBytes)
	}
	if ceiling > 0 {
		return used + " / " + sizeOf(ceiling)
	}
	if v.Live.MemoryBytes == nil {
		return used
	}
	return used + ", no ceiling"
}

func memoryStep(v instances.View) int {
	ceiling, _ := instances.ParseMemory(v.Limits.MemoryMax)
	if v.Live.MemoryBytes == nil || ceiling <= 0 {
		return 0
	}
	return step(*v.Live.MemoryBytes, ceiling)
}

// unknownWhy is the reason the API gave for an unknown value.
func unknownWhy(list []sysinfo.Unknown, field string) string {
	for _, u := range list {
		if u.Field == field {
			return u.Reason
		}
	}
	return "not reported"
}

func joinInts(list []int) string {
	s := make([]string, len(list))
	for i, n := range list {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

// everyWords says a backup interval in words.
func everyWords(d config.Duration) string {
	t := d.D()
	switch {
	case t <= 0:
		return "never"
	case t%time.Hour == 0:
		if t == time.Hour {
			return "every hour"
		}
		return "every " + strconv.Itoa(int(t/time.Hour)) + " hours"
	case t%time.Minute == 0:
		return "every " + strconv.Itoa(int(t/time.Minute)) + " minutes"
	}
	return "every " + t.String()
}

type choice struct {
	Value, Label string
	Selected     bool
}

// backupChoices are the backup intervals the policy form offers, with the
// current one selected (added when it is none of the usual ones).
func backupChoices(cur config.Duration) []choice {
	out := []choice{}
	found := false
	for _, c := range []struct {
		d     time.Duration
		label string
	}{{0, "Never"}, {time.Hour, "1 hour"}, {6 * time.Hour, "6 hours"}, {12 * time.Hour, "12 hours"}, {24 * time.Hour, "24 hours"}} {
		sel := cur.D() == c.d
		found = found || sel
		out = append(out, choice{Value: config.Duration(c.d).String(), Label: c.label, Selected: sel})
	}
	if !found {
		out = append(out, choice{Value: cur.String(), Label: strings.TrimPrefix(everyWords(cur), "every ") + " (now)", Selected: true})
	}
	return out
}

func deliveryWords(d string) string {
	if d == "command_line" {
		return "on the server command line (unsafe on a shared box)"
	}
	return "in a private file"
}

// checkWord names a health check result in words.
func checkWord(status string) string {
	switch status {
	case "ok":
		return "OK"
	case "warn":
		return "Check"
	case "fail":
		return "Problem"
	}
	return status
}

var (
	errorRe   = regexp.MustCompile(`(?i)\b(error|fatal|critical)\b`)
	warningRe = regexp.MustCompile(`(?i)\b(warn|warning)\b`)
)

// lineLevel tells a log line that reports an error or a warning by its
// words. The script uses the same rule for new lines.
func lineLevel(line string) string {
	switch {
	case errorRe.MatchString(line):
		return "error"
	case warningRe.MatchString(line):
		return "warning"
	}
	return ""
}
