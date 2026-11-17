package release

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func linuxOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the update marker is written on Linux only")
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// MarkPending writes the pending file as one JSON line, atomically, and
// touches nothing else: the start lines and the rollback record of an
// earlier update go only when the new binary is in place (Placed).
func TestMarkPending(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm")
	os.WriteFile(exe+StartsSuffix, []byte("2026-11-16T10:00:00Z\n"), 0o644)
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"from":"v0.1.0","to":"v0.2.0"}`), 0o644)
	when := time.Date(2026, 11, 16, 10, 20, 30, 0, time.UTC)
	if err := MarkPending(exe, Marker{From: "v1.0.0", To: "v1.1.0", Time: when}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe + PendingSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"from":"v1.0.0","to":"v1.1.0","time":"2026-11-16T10:20:30Z"}` + "\n"; string(raw) != want {
		t.Fatalf("pending file %q, want %q", raw, want)
	}
	m, err := ReadPending(exe)
	if err != nil || m.From != "v1.0.0" || m.To != "v1.1.0" || !m.Time.Equal(when) {
		t.Fatalf("ReadPending: %+v, %v", m, err)
	}
	for _, kept := range []string{exe + StartsSuffix, exe + RolledBackSuffix} {
		if !exists(kept) {
			t.Errorf("MarkPending removed %s before the new binary was in place", kept)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".esm-update-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// Placed forgets what an earlier update left, and only that.
	if err := Placed(exe); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{exe + StartsSuffix, exe + RolledBackSuffix} {
		if exists(gone) {
			t.Errorf("%s is still there after Placed", gone)
		}
	}
	if !exists(exe + PendingSuffix) {
		t.Error("Placed removed the pending file")
	}
	if err := Placed(exe); err != nil {
		t.Fatalf("Placed with nothing to remove: %v", err)
	}
	// A second update replaces the first one's marker.
	if err := MarkPending(exe, Marker{From: "v1.1.0", To: "v1.2.0", Time: when}); err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadPending(exe); m.To != "v1.2.0" {
		t.Fatalf("second marker %+v", m)
	}
	if _, err := ReadPending(filepath.Join(t.TempDir(), "esm")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no pending file: %v", err)
	}
}

// A self-update on Linux marks itself pending, with the versions and the
// time; the old binary is kept as .previous, and nothing else is left.
func TestUpdateMarksPending(t *testing.T) {
	linuxOnly(t)
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	// What an earlier update left: its start lines and a rollback record. The
	// new update starts from nothing.
	os.WriteFile(exe+StartsSuffix, []byte("2026-11-16T10:00:00Z\n"), 0o644)
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"from":"v0.9.0","to":"v1.0.0"}`), 0o644)
	before := time.Now().Add(-time.Second)
	res, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	if exists(exe + RolledBackSuffix) {
		t.Error("the record of an earlier rollback is still there once the new binary is in place")
	}
	m, err := ReadPending(exe)
	if err != nil || m.From != "v1.0.0" || m.To != "v1.1.0" || m.Time.Before(before) || m.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("pending after the update: %+v, %v", m, err)
	}
	if b, _ := os.ReadFile(res.Previous); string(b) != "old binary" {
		t.Fatalf("previous %q", b)
	}
	if exists(exe + StartsSuffix) {
		t.Error("an update that was not started yet has start lines")
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 3 {
		t.Fatalf("files next to the binary: %v (want the binary, .previous and the pending file)", names)
	}
}

// The new binary goes in place only when the update could be marked: a
// pending file that cannot be written stops the update with the old binary
// where it was.
func TestUpdateWithoutAMarkerChangesNothing(t *testing.T) {
	linuxOnly(t)
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	// A folder in the pending file's place that cannot be replaced.
	if err := os.MkdirAll(filepath.Join(exe+PendingSuffix, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(exe+StartsSuffix, []byte("2026-11-16T10:00:00Z\n"), 0o644)
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"from":"v0.9.0","to":"v1.0.0"}`), 0o644)
	_, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	if err == nil || !strings.Contains(err.Error(), "not confirmed yet") {
		t.Fatalf("update: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatalf("binary changed to %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".esm-update-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// The last record stays: the update that failed before its new binary was
	// in place did not replace anything.
	if !exists(exe+RolledBackSuffix) || !exists(exe+StartsSuffix) {
		t.Error("a failed update removed the last rollback record or the start lines")
	}
}

// An update whose new binary cannot be moved in place (here: the file it was
// downloaded to is gone) keeps the last rollback record and the start lines,
// and leaves no pending file behind.
func TestSwapThatFailsKeepsTheLastRecord(t *testing.T) {
	linuxOnly(t)
	exe := oldBinary(t)
	os.WriteFile(exe+StartsSuffix, []byte("2026-11-16T10:00:00Z\n"), 0o644)
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"from":"v0.9.0","to":"v1.0.0"}`), 0o644)
	mark := func() error { return MarkPending(exe, Marker{From: "v1.0.0", To: "v1.1.0"}) }
	placed := func() { Placed(exe) }
	err := swap(exe, filepath.Join(filepath.Dir(exe), ".esm-update-missing"), exe+PreviousSuffix, mark, placed)
	if err == nil || !strings.Contains(err.Error(), "move the new binary in place") {
		t.Fatalf("swap: %v", err)
	}
	if !exists(exe+RolledBackSuffix) || !exists(exe+StartsSuffix) {
		t.Error("the record or the start lines went although the new binary never got in place")
	}
	if exists(exe + PendingSuffix) {
		t.Error("the pending file of an update that did not happen is still there")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatalf("binary %q", b)
	}
}

// SameAsPrevious tells that the binary is the kept old binary itself: the
// same file, or the same bytes, however long.
func TestSameAsPrevious(t *testing.T) {
	big := func(last byte) []byte {
		b := make([]byte, 200<<10+17)
		for i := range b {
			b[i] = byte(i * 7)
		}
		b[len(b)-1] = last
		return b
	}
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		setup func(exe string)
		want  bool
	}{
		{"the same file under two names", func(exe string) { os.Link(exe, exe+PreviousSuffix) }, true},
		{"a copy with the same bytes", func(exe string) { os.WriteFile(exe+PreviousSuffix, big('a'), 0o755) }, true},
		{"a copy that differs in its last byte", func(exe string) { os.WriteFile(exe+PreviousSuffix, big('b'), 0o755) }, false},
		{"a file of another size", func(exe string) { os.WriteFile(exe+PreviousSuffix, []byte("old binary"), 0o755) }, false},
		{"no previous", func(exe string) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := write(strings.ReplaceAll(tc.name, " ", "-"), big('a'))
			tc.setup(exe)
			if got := SameAsPrevious(exe); got != tc.want {
				t.Fatalf("SameAsPrevious: %v, want %v", got, tc.want)
			}
		})
	}
	if SameAsPrevious(filepath.Join(dir, "no-binary")) {
		t.Error("a binary that is not there is the same as its previous")
	}
}

// ConfirmUpdate removes the pending file and the start lines when this ESM
// is the one the update waits for, and leaves them when it is the old side
// of the update.
func TestConfirmUpdate(t *testing.T) {
	marker := Marker{From: "v1.0.0", To: "v1.1.0", Time: time.Now().UTC()}
	for _, tc := range []struct {
		name    string
		running string
		pending func(exe string)
		cleared bool
	}{
		{"the new version", "v1.1.0", func(exe string) { MarkPending(exe, marker) }, true},
		{"the new version without its v", "1.1.0", func(exe string) { MarkPending(exe, marker) }, true},
		{"a forced update to the same version", "v1.0.0", func(exe string) { MarkPending(exe, Marker{From: "v1.0.0", To: "v1.0.0"}) }, true},
		{"a version the update names neither way", "v1.5.0", func(exe string) { MarkPending(exe, marker) }, true},
		{"the old side of the update", "v1.0.0", func(exe string) { MarkPending(exe, marker) }, false},
		{"a development build, which updated from itself", "dev", func(exe string) { MarkPending(exe, Marker{From: "dev", To: "v1.1.0"}) }, false},
		{"a pending file nobody can read", "v1.1.0", func(exe string) { os.WriteFile(exe+PendingSuffix, []byte("not json"), 0o644) }, true},
		{"no pending file", "v1.1.0", func(exe string) {}, false},
		// An update killed after it kept the old binary and marked itself, but
		// before the new binary went in place: the old ESM runs on, with its
		// own binary as .previous. It is the old side of the update, and
		// still the mark is stale and goes.
		{"a killed update: previous is the same file", "v1.0.0", func(exe string) {
			os.WriteFile(exe, []byte("old binary"), 0o755)
			os.Link(exe, exe+PreviousSuffix)
			MarkPending(exe, marker)
		}, true},
		{"a killed update: previous is a copy", "v1.0.0", func(exe string) {
			os.WriteFile(exe, []byte("old binary"), 0o755)
			os.WriteFile(exe+PreviousSuffix, []byte("old binary"), 0o755)
			MarkPending(exe, marker)
		}, true},
		// The same mark, and a newer binary put there by hand: its ESM is not
		// the old side either.
		{"a killed update, then a binary put there by hand", "v3.0.0", func(exe string) {
			os.WriteFile(exe+PreviousSuffix, []byte("old binary"), 0o755)
			os.WriteFile(exe, []byte("a newer binary"), 0o755)
			MarkPending(exe, marker)
		}, true},
		{"the old side, with the old binary kept next to a new one", "v1.0.0", func(exe string) {
			os.WriteFile(exe+PreviousSuffix, []byte("old binary"), 0o755)
			os.WriteFile(exe, []byte("new binary"), 0o755)
			MarkPending(exe, marker)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "esm")
			tc.pending(exe)
			os.WriteFile(exe+StartsSuffix, []byte("a\nb\n"), 0o644)
			had := exists(exe + PendingSuffix)
			got, err := ConfirmUpdate(exe, tc.running)
			if err != nil || got != tc.cleared {
				t.Fatalf("ConfirmUpdate: %v, %v; want %v", got, err, tc.cleared)
			}
			if tc.cleared && (exists(exe+PendingSuffix) || exists(exe+StartsSuffix)) {
				t.Error("the pending file or the start lines are still there")
			}
			if !tc.cleared && (exists(exe+PendingSuffix) != had || !exists(exe+StartsSuffix)) {
				t.Error("files were removed that should stay")
			}
		})
	}
}

// ReadRolledBack reads what the script writes: the pending record with the
// time added. A file that cannot be read still counts as a rollback.
func TestReadRolledBack(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm")
	if _, ok, err := ReadRolledBack(exe); ok || err != nil {
		t.Fatalf("no file: %v, %v", ok, err)
	}
	os.WriteFile(exe+RolledBackSuffix,
		[]byte(`{"from":"v1.0.0","to":"v1.1.0","time":"2026-11-16T10:00:00Z","rolled_back_at":"2026-11-16T10:00:42Z"}`+"\n"), 0o644)
	r, ok, err := ReadRolledBack(exe)
	if !ok || err != nil || r.From != "v1.0.0" || r.To != "v1.1.0" || r.When().Format(time.RFC3339) != "2026-11-16T10:00:42Z" {
		t.Fatalf("record: %+v, %v, %v", r, ok, err)
	}
	// The script's fallback, when it cannot copy the pending record.
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"rolled_back_at":"2026-11-16T10:00:42Z"}`+"\n"), 0o644)
	if r, ok, err = ReadRolledBack(exe); !ok || err != nil || r.From != "" || r.When().Format(time.RFC3339) != "2026-11-16T10:00:42Z" {
		t.Fatalf("fallback record: %+v, %v, %v", r, ok, err)
	}
	// No time written: the file's own time stands in.
	os.WriteFile(exe+RolledBackSuffix, []byte(`{"from":"v1.0.0","to":"v1.1.0"}`), 0o644)
	stamp := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	os.Chtimes(exe+RolledBackSuffix, stamp, stamp)
	if r, ok, err = ReadRolledBack(exe); !ok || err != nil || !r.When().Equal(stamp) {
		t.Fatalf("record without a time: %+v, %v, %v", r, ok, err)
	}
	// Garbage is a rollback with no details.
	os.WriteFile(exe+RolledBackSuffix, []byte("not json"), 0o644)
	if r, ok, err = ReadRolledBack(exe); !ok || err == nil || r.From != "" {
		t.Fatalf("garbage: %+v, %v, %v", r, ok, err)
	}
	var back RolledBack
	if err := json.Unmarshal([]byte(`{"from":"a","to":"b","time":"2026-11-16T10:00:00Z","rolled_back_at":"2026-11-16T10:00:42Z"}`), &back); err != nil || back.Marker.From != "a" {
		t.Fatalf("the record's fields: %+v, %v", back, err)
	}
}
