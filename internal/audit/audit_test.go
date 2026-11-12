package audit

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

func openLog(t *testing.T) (*Log, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); st.Close() })
	return l, st
}

func TestAppendWritesJSONLines(t *testing.T) {
	l, st := openLog(t)
	var seen []Entry
	l.SetNotify(func(e Entry) { seen = append(seen, e) })
	if err := l.Append(Entry{Actor: Actor{Kind: ActorUser, ID: "admin", Role: "admin"}, Action: "instance.start", Target: "alpha", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Entry{Actor: Actor{Kind: ActorToken, ID: "tok1", Role: "read"}, Action: "instance.stop", Target: "alpha", Error: "forbidden"}); err != nil {
		t.Fatal(err)
	}
	b, err := st.ReadFile(FileName)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), b)
	}
	var e Entry
	if err := json.Unmarshal([]byte(lines[1]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Actor.Kind != "token" || e.Action != "instance.stop" || e.OK || e.Error != "forbidden" || e.Time.IsZero() {
		t.Fatalf("bad entry %+v", e)
	}
	for _, key := range []string{`"time"`, `"actor"`, `"kind"`, `"id"`, `"role"`, `"action"`, `"target"`, `"ok"`, `"error"`} {
		if !strings.Contains(lines[1], key) {
			t.Errorf("line misses %s: %s", key, lines[1])
		}
	}
	if len(seen) != 2 {
		t.Fatalf("notify called %d times", len(seen))
	}
}

func TestAppendIsAppendOnlyAcrossReopen(t *testing.T) {
	l, st := openLog(t)
	l.Append(Entry{Action: "a", OK: true})
	l.Close()
	l2, err := Open(st)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	l2.Append(Entry{Action: "b", OK: true})
	got, err := l2.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "a" || got[1].Action != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestSecretsAreScrubbed(t *testing.T) {
	l, st := openLog(t)
	tok := "esm_" + strings.Repeat("Ab3_-", 9)
	l.Append(Entry{Actor: Actor{Kind: ActorToken, ID: tok}, Action: "x", Target: "t " + tok, Error: "bad token " + tok})
	b, _ := st.ReadFile(FileName)
	if strings.Contains(string(b), tok) {
		t.Fatalf("token leaked into audit log: %s", b)
	}
	if !strings.Contains(string(b), "esm_********") {
		t.Fatalf("mask missing: %s", b)
	}
}

func TestRecentLimitAndConcurrency(t *testing.T) {
	l, _ := openLog(t)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Append(Entry{Action: "a", Target: string(rune('a' + i%26)), OK: true}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := l.Recent(7)
	if err != nil || len(got) != 7 {
		t.Fatalf("len %d err %v", len(got), err)
	}
	all, _ := l.Recent(1000)
	if len(all) != 50 {
		t.Fatalf("want 50 entries, got %d", len(all))
	}
	if got[6] != all[49] || got[0] != all[43] {
		t.Fatal("Recent did not return the newest entries in order")
	}
	if r, _ := l.Recent(0); r != nil {
		t.Fatal("limit 0 should return nothing")
	}
}

func TestScrubPasswordSwitches(t *testing.T) {
	cases := map[string]string{
		"systemd-run failed: -ServerPassword=hunter22 -Port=7777":  "systemd-run failed: -ServerPassword=******** -Port=7777",
		`argv ["-AdminPassword=s3cret!" "-log"]`:                   `argv ["-AdminPassword=******** "-log"]`,
		"url?password=abc&x=1":                                     "url?password=********&x=1",
		"rules: value of AdminPassword must be printable":          "rules: value of AdminPassword must be printable",
		"-serverpassword=lower, -ServerName=keep":                  "-serverpassword=******** -ServerName=keep",
		"token esm_" + strings.Repeat("Ab3_-", 4) + " and nothing": "token esm_******** and nothing",
	}
	for in, want := range cases {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// A password switch is masked up to the next switch, not just to the first
// space or punctuation mark: the same rule as the instance scrubbers.
func TestScrubMasksWholeSwitchValues(t *testing.T) {
	cases := map[string]string{
		"-ServerPassword=a b c -Port=1":                         "-ServerPassword=******** -Port=1",
		`start failed: -AdminPassword=x $&"';%,=<> y -log`:      "start failed: -AdminPassword=******** -log",
		"-ServerPassword=a, b -AdminPassword=c;d -MaxPlayers=4": "-ServerPassword=******** -AdminPassword=******** -MaxPlayers=4",
	}
	for in, want := range cases {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

// Free-text fields are cut to MaxField bytes before they are written (as
// valid UTF-8, with no piece of a token left in clear), so no caller can
// make one entry large.
func TestAppendCapsFields(t *testing.T) {
	l, st := openLog(t)
	var seen []Entry
	l.SetNotify(func(e Entry) { seen = append(seen, e) })
	huge := strings.Repeat("<", 1<<20)
	tok := "esm_" + strings.Repeat("Q", 43)
	e := Entry{
		Actor:  Actor{Kind: ActorAnonymous, ID: huge, Role: huge},
		Action: huge,
		Target: strings.Repeat("é", 1000),
		Error:  strings.Repeat("x", MaxField-10) + tok,
	}
	if err := l.Append(e); err != nil {
		t.Fatal(err)
	}
	b, _ := st.ReadFile(FileName)
	if len(b) > 16<<10 {
		t.Fatalf("one entry took %d bytes", len(b))
	}
	if strings.Contains(string(b), "QQQQQQ") {
		t.Fatalf("a cut token is left in clear: %s", b)
	}
	if len(seen) != 1 {
		t.Fatalf("%d notifications", len(seen))
	}
	got := seen[0]
	for name, v := range map[string]string{"actor id": got.Actor.ID, "role": got.Actor.Role, "action": got.Action, "target": got.Target, "error": got.Error} {
		if len(v) > MaxField || !utf8.ValidString(v) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v", name, len(v), utf8.ValidString(v))
		}
	}
	if !strings.HasPrefix(got.Target, "éé") || !strings.HasPrefix(got.Actor.ID, "<<<") {
		t.Errorf("the start of a field is kept: %q", got.Target[:10])
	}
}

// A line that is too long to read (written by an older version, or by
// hand) is skipped like any other unreadable line; the entries around it
// are still listed.
func TestRecentSkipsOverlongLines(t *testing.T) {
	l, st := openLog(t)
	l.Append(Entry{Action: "before", OK: true})
	f, err := st.OpenAppend(FileName, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte(`{"action":"huge","target":"` + strings.Repeat("a", 3<<20) + "\"}\n"))
	f.Close()
	l.Append(Entry{Action: "after", OK: true})
	got, err := l.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "before" || got[1].Action != "after" {
		t.Fatalf("got %+v", got)
	}
}
