package instances

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// autoRig is a manager with builds steam-main-1..n and a source whose
// newest build is *target.
func autoRig(t *testing.T, n int) (*rig, *string) {
	t.Helper()
	r := newRig(t, nil)
	for i := 1; i <= n; i++ {
		r.install(testkit.BuildOptions{Version: "steam-main-" + string(rune('0'+i))})
	}
	target := new(string)
	*target = "steam-main-" + string(rune('0'+n))
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return *target, time.Now(), src == "main"
	}
	return r, target
}

func (r *rig) createOn(name, build string, rules map[string]string) {
	r.t.Helper()
	if _, err := r.m.Create(CreateRequest{Name: name, Build: build, Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(rules),
		AutoUpdate: ptr(AutoUpdateRestart), UpdateMaxWait: ptr(config.Duration(time.Hour))}}); err != nil {
		r.t.Fatal(err)
	}
}

// waitAuto waits until no automatic update runs for the instance.
func (r *rig) waitAuto(name string) {
	r.t.Helper()
	in, _ := r.m.get(name)
	testkit.Eventually(r.t, 30*time.Second, func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return !in.auto.running
	}, "the automatic update of %s did not end", name)
}

// A player who joins after the scheduler saw an empty server, while the
// update makes its backup, stops the update before the server is stopped:
// it is put off, not failed.
func TestAutoUpdateChecksPlayersBeforeTheStop(t *testing.T) {
	r, _ := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	if _, err := r.m.Start(context.Background(), "a", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // past the first save
	data := r.m.st.Path() + "/" + dataDir("a")
	// A player joins once the update has made its backup.
	beforeAutoStop = func(string) {
		if err := testkit.SetPlayers(data, 2); err != nil {
			t.Error(err)
		}
		time.Sleep(400 * time.Millisecond) // the fake reads it within 100ms
	}
	t.Cleanup(func() { beforeAutoStop = nil })
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	v, _ := r.m.Get("a")
	if v.Build != "steam-main-1" || v.Status.State != Running {
		t.Fatalf("updated although a player joined: build %s, %s", v.Build, v.Status.State)
	}
	if v.Update.LastError != "" || !v.Update.Pending || !strings.Contains(v.Update.Waiting, "2 players") {
		t.Fatalf("after the put-off update: %+v", v.Update)
	}
	if testkit.SecondSignal(data) {
		t.Fatal("second signal")
	}
}

// An update refused because the instance is busy (a conflict) is tried
// again later, not marked failed.
func TestAutoUpdateConflictIsNotAFailure(t *testing.T) {
	r, target := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	in, _ := r.m.get("a")
	in.mu.Lock()
	in.status.State = Starting // Update refuses: busy
	cfg := in.cfg.clone()
	in.auto.running = true
	in.mu.Unlock()
	if !r.m.track() {
		t.Fatal("manager closed")
	}
	r.m.autoUpdate(in, cfg, *target)
	in.mu.Lock()
	failed, lastErr := in.auto.failed, in.auto.lastError
	in.status.State = Stopped
	in.mu.Unlock()
	if failed != "" || lastErr != "" {
		t.Fatalf("a busy instance marked the update failed: %q %q", failed, lastErr)
	}
}

// After an admin updates an instance by hand to an older build of its
// source, automatic updates wait for a build newer than the one left.
func TestAutoUpdatePausesAfterAManualDowngrade(t *testing.T) {
	r, target := autoRig(t, 3)
	*target = "steam-main-2"
	r.createOn("a", "steam-main-2", nil)
	if _, err := r.m.Update(context.Background(), "a", "steam-main-1"); err != nil {
		t.Fatal(err)
	}
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	v, _ := r.m.Get("a")
	if v.Build != "steam-main-1" || v.Update.Pending || !strings.Contains(v.Update.Waiting, "newer than steam-main-2") {
		t.Fatalf("after the manual downgrade: build %s, %+v", v.Build, v.Update)
	}
	// A newer build than the one left: updates again.
	*target = "steam-main-3"
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if v, _ = r.m.Get("a"); v.Build != "steam-main-3" {
		t.Fatalf("not updated to the newer build: %s %+v", v.Build, v.Update)
	}
}

// When there is nothing to update to any more, the last error goes too.
func TestAutoUpdateLastErrorClearsWithTheTarget(t *testing.T) {
	r, target := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	in, _ := r.m.get("a")
	in.mu.Lock()
	in.auto.failed, in.auto.lastError = "steam-main-2", "update failed"
	in.mu.Unlock()
	*target = "steam-main-1"
	r.m.autoTick(time.Now())
	if v, _ := r.m.Get("a"); v.Update.LastError != "" {
		t.Fatalf("last error kept: %+v", v.Update)
	}
}
