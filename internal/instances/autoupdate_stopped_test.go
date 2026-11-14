package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// stateEvents returns the states of the state events of instance name
// that sub got so far.
func stateEvents(sub *events.Subscription, name string) []State {
	var out []State
	for {
		select {
		case ev := <-sub.C:
			var st Status
			if json.Unmarshal(ev.Data, &st) == nil && st.Name == name {
				out = append(out, st.State)
			}
		case <-time.After(300 * time.Millisecond):
			return out
		}
	}
}

// hasMarkKeys reports whether the JSON object b has a key of the
// unproven-update mark.
func hasMarkKeys(b []byte) bool {
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) != nil {
		return true
	}
	_, unverified := keys["update_unverified"]
	_, rollback := keys["update_rollback_build"]
	return unverified || rollback
}

// A status without the unproven-update mark has none of its keys, so an
// earlier release, which refuses unknown fields, can read a state file
// written by this one.
func TestStatusWithoutMarkHasNoMarkKeys(t *testing.T) {
	b, err := json.Marshal(Status{Name: "a", State: Stopped, Build: "steam-main-1"})
	if err != nil {
		t.Fatal(err)
	}
	if hasMarkKeys(b) {
		t.Fatalf("a status without the mark: %s", b)
	}
	b, err = json.Marshal(Status{Name: "a", State: Stopped, Build: "steam-main-2", UpdateUnverified: true, UpdateRollbackBuild: "steam-main-1"})
	if err != nil {
		t.Fatal(err)
	}
	var back Status
	if !hasMarkKeys(b) || json.Unmarshal(b, &back) != nil || !back.UpdateUnverified || back.UpdateRollbackBuild != "steam-main-1" {
		t.Fatalf("a status with the mark: %s", b)
	}
}

// An automatic update of a stopped instance switches its build and starts
// nothing. The next start checks the new build; one that does not get
// ready is rolled back, the old build is started, and the failed build is
// not tried again. Until then the old build counts as in use.
func TestAutoUpdateOfAStoppedInstanceStartsNothing(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.KeepPreviousBuilds = 0 })
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2", Fake: map[string]any{"FakeFailStart": true}})
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return "steam-main-2", time.Now(), src == "main"
	}
	r.createOn("a", "steam-main-1", nil)
	sub := r.bus.Subscribe(events.Filter{Types: []string{events.TypeState}}, 256)
	defer sub.Close()
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	for _, s := range stateEvents(sub, "a") {
		if s == Starting || s == Running || s == Stopping {
			t.Fatalf("the automatic update of a stopped instance went through %s", s)
		}
	}
	v, _ := r.m.Get("a")
	if v.Build != "steam-main-2" || v.Status.Build != "steam-main-2" || v.Status.State != Stopped || !v.Status.UpdateUnverified ||
		v.Status.UpdateRollbackBuild != "steam-main-1" || v.Status.PID != 0 {
		t.Fatalf("after the automatic update: build %s, %+v", v.Build, v.Status)
	}
	var saved Status
	if err := r.st.ReadJSON(stateFile("a"), &saved); err != nil || saved.Build != "steam-main-2" || !saved.UpdateUnverified {
		t.Fatalf("state file after the automatic update: %+v %v", saved, err)
	}
	if list, _ := r.m.ListBackups("a"); len(list) != 1 || list[0].Reason != ReasonPreUpdate {
		t.Fatalf("backups: %+v", list)
	}
	// The build a start would roll back to is kept by pruning, though
	// keep_previous_builds is 0.
	if _, err := r.m.PruneBuilds(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.b.Get("steam-main-1"); err != nil {
		t.Fatalf("the build to roll back to was pruned: %v", err)
	}
	if err := r.m.DeleteBuild("steam-main-1"); fault.CodeOf(err) != "build_in_use" {
		t.Fatalf("delete of the build to roll back to: %v", err)
	}
	// The mark is kept across a restart of ESM.
	again, err := New(Options{Store: r.st, Config: r.cfg, Supervisor: r.m.sup, Builds: r.b})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := again.Status("a"); !st.UpdateUnverified || st.UpdateRollbackBuild != "steam-main-1" {
		t.Fatalf("after a restart of ESM: %+v", st)
	}
	again.Close()

	// The first start: the new build fails, the old one is put back and
	// runs.
	_, err = r.m.Start(context.Background(), "a", false)
	if fault.CodeOf(err) != "update_failed" || !strings.Contains(err.Error(), "steam-main-1") {
		t.Fatalf("first start of a broken build: %v", err)
	}
	v, _ = r.m.Get("a")
	if v.Build != "steam-main-1" || v.Status.State != Running || v.Status.UpdateUnverified || v.Status.UpdateRollbackBuild != "" {
		t.Fatalf("after the roll back: build %s, %+v", v.Build, v.Status)
	}
	if v.Update.LastError == "" || v.Update.Pending || !strings.Contains(v.Update.Waiting, "not tried again") {
		t.Fatalf("the failed build is not recorded: %+v", v.Update)
	}
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if v, _ = r.m.Get("a"); v.Build != "steam-main-1" || v.Status.State != Running {
		t.Fatalf("the failed build was tried again: build %s, %+v", v.Build, v.Status)
	}
}

// When the first start of a broken build rolls back and the old build
// cannot be started either, the mark is gone from the status, the state
// file and the last state event all the same.
func TestCheckedStartThatCannotStartTheOldBuildClearsTheMark(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2", Fake: map[string]any{"FakeFailStart": true}})
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return "steam-main-2", time.Now(), src == "main"
	}
	r.createOn("a", "steam-main-1", nil)
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if st, _ := r.m.Status("a"); !st.UpdateUnverified {
		t.Fatalf("after the automatic update: %+v", st)
	}
	// The old build goes missing, so the roll back cannot start it.
	dir, err := r.b.Path("steam-main-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, builds.ManifestName)); err != nil {
		t.Fatal(err)
	}
	sub := r.bus.Subscribe(events.Filter{Types: []string{events.TypeState}}, 256)
	defer sub.Close()
	_, err = r.m.Start(context.Background(), "a", false)
	if fault.CodeOf(err) != "update_failed" || !strings.Contains(err.Error(), "did not start again") {
		t.Fatalf("first start of a broken build: %v", err)
	}
	st, _ := r.m.Status("a")
	if st.UpdateUnverified || st.UpdateRollbackBuild != "" || st.Build != "steam-main-1" {
		t.Fatalf("status after the roll back: %+v", st)
	}
	var last Status
	for done := false; !done; {
		select {
		case ev := <-sub.C:
			last = Status{}
			if json.Unmarshal(ev.Data, &last) != nil {
				t.Fatalf("state event: %s", ev.Data)
			}
		case <-time.After(300 * time.Millisecond):
			done = true
		}
	}
	if last.UpdateUnverified || last.Build != "steam-main-1" {
		t.Fatalf("last state event after the roll back: %+v", last)
	}
	b, err := r.st.ReadFile(stateFile("a"))
	if err != nil || hasMarkKeys(b) {
		t.Fatalf("state file after the roll back: %s %v", b, err)
	}
	if v, _ := r.m.Get("a"); v.Build != "steam-main-1" || v.Update.LastError == "" {
		t.Fatalf("after the roll back: build %s, %+v", v.Build, v.Update)
	}
}

// A good build switched in while the instance was stopped is proven at its
// first start, and the mark goes.
func TestAutoUpdatedBuildIsCheckedAtTheNextStart(t *testing.T) {
	r, _ := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	v, _ := r.m.Get("a")
	if v.Build != "steam-main-2" || v.Status.State != Stopped || !v.Status.UpdateUnverified {
		t.Fatalf("after the automatic update: build %s, %+v", v.Build, v.Status)
	}
	st, err := r.m.Start(context.Background(), "a", false)
	if err != nil || st.State != Running || st.UpdateUnverified || st.UpdateRollbackBuild != "" {
		t.Fatalf("first start of the new build: %+v %v", st, err)
	}
	v, _ = r.m.Get("a")
	if v.Build != "steam-main-2" || !slices.Contains(v.PreviousBuilds, "steam-main-1") {
		t.Fatalf("after the first start: build %s, previous %v", v.Build, v.PreviousBuilds)
	}
	// Without the mark the state file has no mark fields, so an earlier
	// release can still read it.
	if b, err := r.st.ReadFile(stateFile("a")); err != nil || hasMarkKeys(b) {
		t.Fatalf("state file after the first start: %s %v", b, err)
	}
	// Nothing is checked at later starts.
	if _, err := r.m.Restart(context.Background(), "a", "", true); err != nil {
		t.Fatal(err)
	}
	if v, _ = r.m.Get("a"); v.Build != "steam-main-2" || v.Status.State != Running {
		t.Fatalf("after a restart: build %s, %+v", v.Build, v.Status)
	}
}
