package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/svcunit"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// writeRolledBack leaves the record the service unit's script leaves when it
// put the old binary back.
func writeRolledBack(t *testing.T, exe, at string) {
	t.Helper()
	rec := `{"from":"v1.0.0","to":"v1.1.0","time":"2026-11-16T10:00:00Z","rolled_back_at":"` + at + `"}` + "\n"
	if err := os.WriteFile(exe+release.RolledBackSuffix, []byte(rec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rollbackEntries(h *harness) []audit.Entry {
	h.t.Helper()
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	var out []audit.Entry
	for _, e := range al.Entries {
		if e.Action == "box.self_update_rollback" {
			out = append(out, e)
		}
	}
	return out
}

// An ESM that finds the record of a rolled back self-update shows it in GET
// /box as last_self_update, and writes one audit entry for it, however often
// it starts. A box with no such record has last_self_update null. A later
// rollback is reported again.
func TestARolledBackSelfUpdateIsReported(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm-under-test")
	h := newESMTuned(t, "process", func(o *service.Options) { o.Executable = exe })

	var raw map[string]json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &raw)
	if string(raw["last_self_update"]) != "null" {
		t.Fatalf("last_self_update with no rollback: %s", raw["last_self_update"])
	}
	if got := rollbackEntries(h); len(got) != 0 {
		t.Fatalf("audit entries with no rollback: %+v", got)
	}
	if code, out, _ := h.runCLI("status"); code != 0 || strings.Contains(out, "rolled back") {
		t.Fatalf("status with no rollback: %d %q", code, out)
	}

	writeRolledBack(t, exe, "2026-11-16T10:20:30Z")
	h.stopService()
	h.startService()
	var box api.Box
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
	u := box.LastSelfUpdate
	if u == nil || u.From != "v1.0.0" || u.To != "v1.1.0" || !u.RolledBack || u.Time.Format(time.RFC3339) != "2026-11-16T10:20:30Z" {
		t.Fatalf("last_self_update: %+v", u)
	}
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &raw)
	if want := `{"from":"v1.0.0","to":"v1.1.0","time":"2026-11-16T10:20:30Z","rolled_back":true}`; string(raw["last_self_update"]) != want {
		t.Fatalf("last_self_update is %s, want %s", raw["last_self_update"], want)
	}
	got := rollbackEntries(h)
	if len(got) != 1 || got[0].Actor.Kind != audit.ActorSystem || got[0].Actor.ID != "esm" || got[0].Target != "esm v1.1.0 to v1.0.0" || !got[0].OK {
		t.Fatalf("audit entries: %+v", got)
	}
	if !strings.Contains(h.stderr.String(), "did not come up and was rolled back") {
		t.Errorf("the service log does not say so:\n%s", h.stderr.String())
	}
	if code, out, errOut := h.runCLI("status"); code != 0 || !strings.Contains(oneLine(t, "status", out),
		"the self-update to v1.1.0 did not come up and was rolled back on 2026-11-16T10:20:30Z") {
		t.Fatalf("status: %d %q %q", code, out, errOut)
	}

	// Another start: still shown, not written again.
	h.stopService()
	h.startService()
	box = api.Box{}
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
	if box.LastSelfUpdate == nil || box.LastSelfUpdate.To != "v1.1.0" {
		t.Fatalf("after a restart: %+v", box.LastSelfUpdate)
	}
	if got := rollbackEntries(h); len(got) != 1 {
		t.Fatalf("a restart wrote the audit entry again: %+v", got)
	}

	// A later rollback is a new one.
	writeRolledBack(t, exe, "2026-11-17T08:00:00Z")
	h.stopService()
	h.startService()
	if got := rollbackEntries(h); len(got) != 2 {
		t.Fatalf("audit entries after a second rollback: %+v", got)
	}

	// The record goes when the next self-update has its new binary in place
	// (release.MarkPending, then release.Placed): the next start shows none.
	if err := release.MarkPending(exe, release.Marker{From: "v1.0.0", To: "v1.2.0"}); err != nil {
		t.Fatal(err)
	}
	if err := release.Placed(exe); err != nil {
		t.Fatal(err)
	}
	h.stopService()
	h.startService()
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &raw)
	if string(raw["last_self_update"]) != "null" {
		t.Fatalf("last_self_update after the next update was put in place: %s", raw["last_self_update"])
	}
}

// A self-update replaces the binary only, so ESM brings the rollback script
// next to it up to date when it starts: a script that differs from the one
// built into this ESM is rewritten, and one that is not there is not made
// (only esm service install makes it).
func TestESMRefreshesTheRollbackScriptAtStart(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm-under-test")
	script := svcunit.ScriptPath(exe)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n# the script of an older ESM\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newESMTuned(t, "process", func(o *service.Options) { o.Executable = exe })
	if got, _ := os.ReadFile(script); string(got) != string(svcunit.Script()) {
		t.Fatalf("the script after the start:\n%s", got)
	}
	if !strings.Contains(h.stderr.String(), "rollback script next to the binary was rewritten") {
		t.Errorf("the service log does not say so:\n%s", h.stderr.String())
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Fatalf("files next to the binary: %v", entries)
	}

	// No script: none is made, at the first start or any later one.
	bare := filepath.Join(t.TempDir(), "esm-under-test")
	h2 := newESMTuned(t, "process", func(o *service.Options) { o.Executable = bare })
	h2.stopService()
	h2.startService()
	if entries, _ := os.ReadDir(filepath.Dir(bare)); len(entries) != 0 {
		t.Fatalf("ESM made files next to a binary that has no script: %v", entries)
	}
}

// A record the script could not fill in (it falls back to the time alone), or
// that cannot be read at all, is still a rollback: ESM shows that one
// happened, with no versions.
func TestARollbackRecordWithoutVersionsIsStillReported(t *testing.T) {
	for name, content := range map[string]string{
		"time only": `{"rolled_back_at":"2026-11-16T10:20:30Z"}` + "\n",
		"not json":  "not json\n",
	} {
		t.Run(name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "esm-under-test")
			os.WriteFile(exe+release.RolledBackSuffix, []byte(content), 0o644)
			stamp := time.Date(2026, 11, 16, 10, 20, 30, 0, time.UTC)
			os.Chtimes(exe+release.RolledBackSuffix, stamp, stamp)
			h := newESMTuned(t, "process", func(o *service.Options) { o.Executable = exe })
			var box api.Box
			h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
			if u := box.LastSelfUpdate; u == nil || u.From != "" || u.To != "" || !u.RolledBack || !u.Time.Equal(stamp) {
				t.Fatalf("last_self_update: %+v", u)
			}
			if got := rollbackEntries(h); len(got) != 1 || got[0].Target != "esm" {
				t.Fatalf("audit entries: %+v", got)
			}
		})
	}
}

// A new ESM that has been up for a while confirms its self-update: the
// pending file and the start lines go, and the service unit no longer puts
// the old binary back. Before that, and in an ESM that is the old side of the
// update, they stay.
func TestASelfUpdateIsConfirmedWhenESMStaysUp(t *testing.T) {
	start := func(t *testing.T, version string, after time.Duration, m release.Marker, prep ...func(exe string)) string {
		t.Helper()
		exe := filepath.Join(t.TempDir(), "esm-under-test")
		os.WriteFile(exe, []byte("binary"), 0o755)
		for _, p := range prep {
			p(exe)
		}
		if err := release.MarkPending(exe, m); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(exe+release.StartsSuffix, []byte("2026-11-16T10:00:00Z\n"), 0o644)
		newESMTuned(t, "process", func(o *service.Options) { o.Executable, o.Version, o.UpdateConfirmAfter = exe, version, after })
		return exe
	}
	marker := release.Marker{From: "v1.0.0", To: "v1.1.0", Time: time.Now().UTC()}
	has := func(path string) bool { _, err := os.Stat(path); return err == nil }

	t.Run("the new version, after the time", func(t *testing.T) {
		exe := start(t, "v1.1.0", 300*time.Millisecond, marker)
		testkit.Eventually(t, 10*time.Second, func() bool { return !has(exe+release.PendingSuffix) && !has(exe+release.StartsSuffix) },
			"the pending file and the start lines are still there")
	})
	t.Run("the new version, before the time", func(t *testing.T) {
		exe := start(t, "v1.1.0", time.Hour, marker)
		time.Sleep(500 * time.Millisecond)
		if !has(exe+release.PendingSuffix) || !has(exe+release.StartsSuffix) {
			t.Fatal("the update was confirmed at once")
		}
	})
	t.Run("the old side of the update", func(t *testing.T) {
		exe := start(t, "v1.0.0", 100*time.Millisecond, marker)
		time.Sleep(700 * time.Millisecond)
		if !has(exe+release.PendingSuffix) || !has(exe+release.StartsSuffix) {
			t.Fatal("an ESM that is not the new version confirmed the update")
		}
	})
	// An update killed after it kept the old binary and marked itself, but
	// before the new binary went in place: the ESM that goes on running is the
	// old side, and its binary is the kept one, so the mark is stale and goes.
	t.Run("an update that never got its binary in place", func(t *testing.T) {
		exe := start(t, "v1.0.0", 300*time.Millisecond, marker, func(exe string) {
			if err := os.Link(exe, exe+release.PreviousSuffix); err != nil {
				t.Fatal(err)
			}
		})
		testkit.Eventually(t, 10*time.Second, func() bool { return !has(exe+release.PendingSuffix) && !has(exe+release.StartsSuffix) },
			"the stale pending file and the start lines are still there")
	})
}

// An ESM that put a new binary in place itself is the old side of that
// update: it never confirms it, however long it lives on before it ends. The
// update here is a forced one to the very version that runs, where the
// versions cannot tell the two sides apart.
func TestAnESMThatUpdatedItselfDoesNotConfirmTheUpdate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the update marker is written on Linux only")
	}
	exe, tune := releaseServer(t, "systemd")
	h := newESMTuned(t, "process", func(o *service.Options) {
		tune(o)
		o.Version, o.UpdateConfirmAfter = "v9.9.9", 500*time.Millisecond
	})
	var res api.SelfUpdateResult
	h.must(http.StatusOK, "POST", "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true}, &res)
	m, err := release.ReadPending(exe)
	if err != nil || m.From != "v9.9.9" || m.To != "v9.9.9" || time.Since(m.Time) > time.Minute {
		t.Fatalf("pending file after the update: %+v, %v", m, err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := release.ReadPending(exe); err != nil {
		t.Fatalf("the ESM that did the update confirmed it: %v", err)
	}
	if _, err := os.Stat(exe + release.StartsSuffix); err == nil {
		t.Error("the update has start lines before any start")
	}
}
