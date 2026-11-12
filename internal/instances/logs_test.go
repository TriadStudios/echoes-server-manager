package instances

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// logRig is a manager that only has a store: enough to follow files.
func logRig(t *testing.T) (*Manager, func(text string)) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	add := func(text string) {
		t.Helper()
		f, err := st.OpenAppend("log", 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{st: st}, add
}

// A tail hands out whole lines only, as the file has them.
func TestFollowWholeLines(t *testing.T) {
	m, add := logRig(t)
	tl := tail{rel: "log"}
	if got := m.follow(&tl); got != nil {
		t.Fatalf("missing file: %q", got)
	}
	add("first\nsecond half")
	if got := m.follow(&tl); !slices.Equal(got, []string{"first"}) {
		t.Fatalf("got %q", got)
	}
	if got := m.follow(&tl); got != nil {
		t.Fatalf("nothing new: %q", got)
	}
	add(" and the rest\r\nthird\n")
	if got := m.follow(&tl); !slices.Equal(got, []string{"second half and the rest\r", "third"}) {
		t.Fatalf("got %q", got)
	}
	// A file that shrank is read from its start.
	if err := m.st.WriteFile("log", []byte("new file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.follow(&tl); !slices.Equal(got, []string{"new file"}) {
		t.Fatalf("after the file was replaced: %q", got)
	}
}

// A tail that starts in the middle of a line drops the rest of that line:
// it is not a line of its own, whatever it says.
func TestTailAtDropsAnUnfinishedLine(t *testing.T) {
	m, add := logRig(t)
	if tl := m.tailAt("log"); tl.off != 0 || tl.skip {
		t.Fatalf("tail of a missing file: %+v", tl)
	}
	add("old line\nCommand line: -Motd=")
	tl := m.tailAt("log")
	if !tl.skip {
		t.Fatal("the tail does not know it starts inside a line")
	}
	add(gameserver.ReadyMarker + " -log\n" + gameserver.WorldSavedMarker + " (1)\n")
	got := m.follow(&tl)
	if !slices.Equal(got, []string{gameserver.WorldSavedMarker + " (1)"}) {
		t.Fatalf("got %q", got)
	}
	// At a line end nothing is dropped.
	tl = m.tailAt("log")
	if tl.skip {
		t.Fatal("a tail at a line end drops the next line")
	}
	add("next\n")
	if got := m.follow(&tl); !slices.Equal(got, []string{"next"}) {
		t.Fatalf("got %q", got)
	}
}

// A line longer than the limit is handed out once, from its start; what
// follows of it is dropped and never taken for a new line.
func TestFollowDropsTheRestOfALongLine(t *testing.T) {
	m, add := logRig(t)
	tl := tail{rel: "log"}
	head := "Command line: " + strings.Repeat("a", maxLineBytes)
	add(head)
	got := m.follow(&tl)
	if len(got) != 1 || got[0] != head {
		t.Fatalf("the start of a long line: %d lines", len(got))
	}
	add(gameserver.ReadyMarker + " still the same line")
	if got := m.follow(&tl); got != nil {
		t.Fatalf("the rest of a long line was handed out: %q", got)
	}
	add(" its end\n" + gameserver.ReadyMarker + " (really)\n")
	if got := m.follow(&tl); !slices.Equal(got, []string{gameserver.ReadyMarker + " (really)"}) {
		t.Fatalf("got %q", got)
	}
}

// Lines are shown without a carriage return, cut to the limit and as valid
// UTF-8.
func TestShowLine(t *testing.T) {
	if got := showLine("text\r"); got != "text" {
		t.Fatalf("got %q", got)
	}
	// The cut falls inside a two-byte character.
	long := "a" + strings.Repeat("é", maxLineBytes)
	got := showLine(long)
	if len(got) > maxLineBytes+len("�") || !strings.HasSuffix(got, "�") || !strings.HasPrefix(long, strings.TrimSuffix(got, "�")) {
		t.Fatalf("a long line is %d bytes and ends with %q", len(got), got[len(got)-4:])
	}
	if got := showLine("bad \xff byte"); got != "bad � byte" {
		t.Fatalf("got %q", got)
	}
}

// A log request returns at most MaxTailLines lines, and its answer stays
// within MaxLogReplyBytes however long the lines are and however much
// JSON escaping blows them up; the newest lines are the ones kept.
func TestLogRepliesAreBounded(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if err := r.st.MkdirAll(dataDir("alpha")+"/Logs", 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(n int, line func(i int) string) {
		t.Helper()
		var b strings.Builder
		for i := range n {
			b.WriteString(line(i))
			b.WriteByte('\n')
		}
		if err := r.st.WriteFile(serverLog("alpha"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Many short lines: the line cap.
	write(MaxTailLines+1000, func(i int) string { return fmt.Sprintf("line %d", i) })
	lines, err := r.m.Logs("alpha", SourceServer, MaxTailLines+500, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != MaxTailLines || lines[len(lines)-1] != fmt.Sprintf("line %d", MaxTailLines+999) {
		t.Fatalf("%d lines ending with %q, want the newest %d", len(lines), lines[len(lines)-1], MaxTailLines)
	}
	// Long lines of characters that JSON escapes six times over: the size
	// cap. Unbounded, the tail would be several times MaxLogReplyBytes.
	const total = 3000
	write(total, func(i int) string { return fmt.Sprintf("%05d", i) + strings.Repeat("<", 4000) })
	lines, err = r.m.Logs("alpha", SourceServer, MaxTailLines, true)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(lines)
	if len(lines) == 0 || len(b) > MaxLogReplyBytes+2 {
		t.Fatalf("%d lines in %d bytes of JSON, want at most %d bytes", len(lines), len(b), MaxLogReplyBytes)
	}
	if !strings.HasPrefix(lines[len(lines)-1], fmt.Sprintf("%05d", total-1)) {
		t.Fatalf("the newest line is not kept: %.20q", lines[len(lines)-1])
	}
	if one, _ := json.Marshal(lines[0]); len(b)+len(one)+1 <= MaxLogReplyBytes {
		t.Fatalf("%d lines in %d bytes: one more would have fitted", len(lines), len(b))
	}
}

// The log tail returns whole lines too.
func TestTailLines(t *testing.T) {
	m, add := logRig(t)
	if got, err := m.tailLines("log", 5); err != nil || len(got) != 0 {
		t.Fatalf("missing file: %q %v", got, err)
	}
	add("one\ntwo\nthree\n")
	if got, _ := m.tailLines("log", 2); !slices.Equal(got, []string{"two", "three"}) {
		t.Fatalf("got %q", got)
	}
	if got, _ := m.tailLines("log", 10); !slices.Equal(got, []string{"one", "two", "three"}) {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(filepath.Join(m.st.Path(), "log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.tailLines("log", 10); len(got) != 0 {
		t.Fatalf("empty file: %q", got)
	}
}

// A huge line costs no more to show than a long one: it is cut to a
// bounded piece before it is masked.
func TestShowOutputMasksABoundedPiece(t *testing.T) {
	filter := gameserver.NewScrubber([]string{"hunter22"})
	line := "say hunter22 -ServerPassword=x -log " + strings.Repeat("a b ", 1<<20) // 4 MiB
	showOutput(filter, line)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := showOutput(filter, line)
	runtime.ReadMemStats(&after)
	if d := after.TotalAlloc - before.TotalAlloc; d > 1<<20 {
		t.Fatalf("showing one line of %d bytes allocated %d bytes", len(line), d)
	}
	if want := "say " + gameserver.Mask + " -ServerPassword=" + gameserver.Mask + " -log a b"; !strings.HasPrefix(got, want) || len(got) != maxLineBytes {
		t.Fatalf("got %d bytes: %.80q", len(got), got)
	}
	// A value masked whole makes the piece short: the line is masked
	// whole, and that is right too.
	if got := showOutput(filter, "-ServerPassword="+strings.Repeat("a b ", 1<<20)); got != "-ServerPassword="+gameserver.Mask {
		t.Fatalf("got %.80q", got)
	}
}

// When masking shrinks the cut piece so much that its end would be shown,
// the end may hold the start of a password that the cut split: then the
// whole line is masked instead.
func TestShowOutputNeverShowsAPieceOfACutPassword(t *testing.T) {
	secret := "Sw0rdfish-" + strings.Repeat("q", 90) // 100 bytes
	filter := gameserver.NewScrubber([]string{secret})
	line := strings.Repeat(secret, maxScrubBytes/len(secret)+5) // the cut falls inside a copy
	if maxScrubBytes%len(secret) == 0 {
		t.Fatal("pick a length that the cut splits")
	}
	got := showOutput(filter, line)
	if strings.Contains(got, "Sw0rd") || strings.Contains(got, "qqqq") {
		t.Fatalf("a piece of the password is shown: %.200q", got)
	}
}
