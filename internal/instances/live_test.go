package instances

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/sysinfo"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// unknownReason returns why field is unknown in l, "" when it is known.
func unknownReason(l Live, field string) string {
	for _, u := range l.Unknown {
		if u.Field == field {
			return u.Reason
		}
	}
	return ""
}

// checkLiveHonest fails when a value of l is both known and listed as
// unknown, or neither.
func checkLiveHonest(t *testing.T, l Live) {
	t.Helper()
	known := map[string]bool{"memory_bytes": l.MemoryBytes != nil, "players": l.Players != nil, "last_save": l.LastSave != nil}
	for f, k := range known {
		if why := unknownReason(l, f); k == (why != "") {
			t.Fatalf("%s: known %v, unknown reason %q (%+v)", f, k, why, l)
		}
	}
	if (l.MemoryBytes != nil) != (l.MemorySource != "") {
		t.Fatalf("memory %v with source %q", l.MemoryBytes, l.MemorySource)
	}
}

// The view says what ESM can read about a server right now, and why it
// cannot read the rest.
func TestLiveFacts(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	v, err := r.m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	checkLiveHonest(t, v.Live)
	if v.Live.MemoryBytes != nil || v.Live.Players != nil || unknownReason(v.Live, "memory_bytes") != "the server is not running" ||
		!strings.Contains(unknownReason(v.Live, "players"), "status file") {
		t.Fatalf("stopped: %+v", v.Live)
	}
	if _, err := r.m.Start(context.Background(), "alpha", true); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		v, _ = r.m.Get("alpha")
		return v.Live.Players != nil && v.Live.LastSave != nil
	}, "no player count or last save from the status file")
	checkLiveHonest(t, v.Live)
	if *v.Live.Players != 0 || v.Live.LastSave.After(time.Now().Add(time.Minute)) {
		t.Fatalf("running: %+v", v.Live)
	}
	switch runtime.GOOS {
	case "linux":
		if v.Live.MemoryBytes == nil || *v.Live.MemoryBytes <= 0 || v.Live.MemorySource != sysinfo.SourceProcess {
			t.Fatalf("memory of a running server: %+v", v.Live)
		}
	case "windows":
		if v.Live.MemoryBytes != nil || !strings.Contains(unknownReason(v.Live, "memory_bytes"), "Windows") {
			t.Fatalf("memory on Windows: %+v", v.Live)
		}
	}
	// The list carries the same.
	list := r.m.List()
	if len(list) != 1 || list[0].Live.Players == nil {
		t.Fatalf("list: %+v", list)
	}
	if _, err := r.m.Stop(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	v, _ = r.m.Get("alpha")
	checkLiveHonest(t, v.Live)
	if v.Live.Players != nil || v.Live.MemoryBytes != nil || v.Live.LastSave == nil {
		t.Fatalf("stopped again (the last save stays known): %+v", v.Live)
	}
}

// A status file of an earlier run never gives a player count.
func TestLivePlayersOnlyFromThisRun(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if err := r.st.MkdirAll(dataDir("alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteJSON(dataDir("alpha")+"/"+gameserver.StatusFileRel,
		gameserver.Status{State: gameserver.StateReady, PID: 1, Players: 7}, 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := r.m.Get("alpha")
	if v.Live.Players != nil || unknownReason(v.Live, "players") != "the server is not running" {
		t.Fatalf("players of a stopped server: %+v", v.Live)
	}
	in := r.m.lookup("alpha")
	in.mu.Lock()
	in.status.State, in.status.PID = Running, 999999
	in.mu.Unlock()
	v, _ = r.m.Get("alpha")
	in.mu.Lock()
	in.status.State, in.status.PID = Stopped, 0
	in.mu.Unlock()
	if v.Live.Players != nil || !strings.Contains(unknownReason(v.Live, "players"), "earlier run") {
		t.Fatalf("players from another pid: %+v", v.Live)
	}
}

// The view names the next scheduled restart and backup.
func TestScheduledInView(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	if v, _ := r.m.Get("alpha"); !v.Scheduled.Restart.IsZero() || !v.Scheduled.Backup.IsZero() {
		t.Fatalf("nothing scheduled, view says %+v", v.Scheduled)
	}
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{
		Schedule: &Schedule{RestartAt: []string{"04:00", "16:30"}},
		Backup:   &BackupPolicy{Every: config.Duration(time.Hour), Keep: 3}}}); err != nil {
		t.Fatal(err)
	}
	times := []string{"04:00", "16:30"}
	lo := NextRestart(time.Now(), times)
	v, _ := r.m.Get("alpha")
	hi := NextRestart(time.Now(), times)
	if !v.Scheduled.Restart.Equal(lo) && !v.Scheduled.Restart.Equal(hi) {
		t.Fatalf("next restart %v, want %v", v.Scheduled.Restart, lo)
	}
	if v.Scheduled.Restart.Location() != time.UTC || !v.Scheduled.Backup.IsZero() {
		t.Fatalf("scheduled %+v", v.Scheduled)
	}
	now := time.Date(2026, 5, 1, 3, 0, 0, 0, time.Local)
	r.m.schedTick(now)
	v, _ = r.m.Get("alpha")
	if !v.Scheduled.Restart.Equal(time.Date(2026, 5, 1, 4, 0, 0, 0, time.Local)) || !v.Scheduled.Backup.Equal(now.Add(time.Hour)) {
		t.Fatalf("after a scheduler check: %+v", v.Scheduled)
	}
}
