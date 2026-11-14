package instances

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// RemoveUnusedBuilds removes only builds it is given, and of those only the
// ones no instance uses: not a current build, and not the build an
// unproven automatic update would go back to.
func TestRemoveUnusedBuildsKeepsBuildsInUse(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.KeepPreviousBuilds = 0 })
	for _, v := range []string{"steam-main-1", "steam-main-2", "steam-main-3", "steam-main-4", "steam-main-5"} {
		r.install(testkit.BuildOptions{Version: v})
	}
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return "steam-main-2", time.Now(), src == "main"
	}
	// a: switched from 1 to 2 while stopped, so 1 is the build to go back to.
	r.createOn("a", "steam-main-1", nil)
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if st, _ := r.m.Status("a"); !st.UpdateUnverified || st.UpdateRollbackBuild != "steam-main-1" {
		t.Fatalf("after the automatic update: %+v", st)
	}
	// b: on 3, and never updated automatically.
	if _, err := r.m.Create(CreateRequest{Name: "b", Build: "steam-main-3", Settings: Settings{BindIP: ptr("127.0.0.1")}}); err != nil {
		t.Fatal(err)
	}

	removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2", "steam-main-3", "steam-main-4"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"steam-main-4"}) {
		t.Fatalf("removed %v, want only steam-main-4", removed)
	}
	for _, v := range []string{"steam-main-1", "steam-main-2", "steam-main-3", "steam-main-5"} {
		if _, err := r.b.Get(v); err != nil {
			t.Errorf("%s is gone: %v", v, err)
		}
	}
	if _, err := r.b.Get("steam-main-4"); err == nil {
		t.Error("steam-main-4 is still installed")
	}
}

// inBackground runs f and returns what it returns on a channel.
func inBackground(f func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	return done
}

// result waits for what inBackground returns.
func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("no answer within a minute")
		return nil
	}
}

// waitWaitedFor waits until instance name is on build and its server is
// starting: the new build is switched in and waited for.
func (r *rig) waitWaitedFor(name, build string) {
	r.t.Helper()
	testkit.Eventually(r.t, 30*time.Second, func() bool {
		v, err := r.m.Get(name)
		return err == nil && v.Build == build && v.Status.State == Starting
	}, "%s never started build %s", name, build)
}

// With keep_previous_builds 0 the build an update started from is no
// previous build once the new one is switched in, yet the update goes back
// to it when the new one does not get ready. Pruning keeps it while the new
// build is waited for. Once the update has ended, the build it left is used
// by no one and can go.
func TestPruneDuringAnUpdateKeepsTheBuildToGoBackTo(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.KeepPreviousBuilds = 0
		c.ReadyTimeout = config.Duration(3 * time.Second)
	})
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2", Fake: map[string]any{"FakeNoReady": true}})
	if _, err := r.m.Create(CreateRequest{Name: "a", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	done := inBackground(func() error {
		_, err := r.m.Update(ctx, "a", "steam-main-2")
		return err
	})
	r.waitWaitedFor("a", "steam-main-2")
	if removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2"}); err != nil || len(removed) > 0 {
		t.Errorf("a prune during the update removed %v (%v)", removed, err)
	}
	if removed, err := r.m.PruneBuilds(); err != nil || len(removed) > 0 {
		t.Errorf("a prune of every unused build during the update removed %v (%v)", removed, err)
	}
	if err := r.m.DeleteBuild("steam-main-1"); fault.CodeOf(err) != "build_in_use" {
		t.Errorf("delete of the build to go back to during the update: %v", err)
	}
	err := result(t, done)
	if fault.CodeOf(err) != "update_failed" || !strings.Contains(err.Error(), "rolled back to steam-main-1") {
		t.Fatalf("update to a build that never gets ready: %v", err)
	}
	if v, _ := r.m.Get("a"); v.Build != "steam-main-1" || v.Status.State != Running {
		t.Fatalf("after the roll back: build %s, %+v", v.Build, v.Status)
	}
	// The update is over: nothing holds the build it went to any more.
	if users := r.m.BuildUsers(); len(users["steam-main-2"]) > 0 || !slices.Equal(users["steam-main-1"], []string{"a"}) {
		t.Fatalf("build users after the update: %v", users)
	}
	removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2"})
	if err != nil || !slices.Equal(removed, []string{"steam-main-2"}) {
		t.Fatalf("prune after the update removed %v (%v), want steam-main-2", removed, err)
	}
}

// The build an update goes to is kept from the moment the update begins,
// before it is switched in: here pruning runs once an automatic update of
// a running server has made its backup. After the update, the build it
// left is used by no one (keep_previous_builds 0) and can go.
func TestPruneKeepsTheBuildAnUpdateGoesTo(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.KeepPreviousBuilds = 0 })
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return "steam-main-2", time.Now(), src == "main"
	}
	r.createOn("a", "steam-main-1", nil)
	if _, err := r.m.Start(context.Background(), "a", true); err != nil {
		t.Fatal(err)
	}
	var (
		removed            []string
		pruneErr, delError error
	)
	beforeAutoStop = func(string) {
		removed, pruneErr = r.m.RemoveUnusedBuilds([]string{"steam-main-2"})
		delError = r.m.DeleteBuild("steam-main-2")
	}
	t.Cleanup(func() { beforeAutoStop = nil })
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if pruneErr != nil || len(removed) > 0 {
		t.Errorf("a prune during the update removed %v (%v)", removed, pruneErr)
	}
	if fault.CodeOf(delError) != "build_in_use" {
		t.Errorf("delete of the build the update goes to: %v", delError)
	}
	v, _ := r.m.Get("a")
	if v.Build != "steam-main-2" || v.Status.State != Running || v.Update.LastError != "" {
		t.Fatalf("after the update: build %s, %+v, %+v", v.Build, v.Status, v.Update)
	}
	got, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2"})
	if err != nil || !slices.Equal(got, []string{"steam-main-1"}) {
		t.Fatalf("prune after the update removed %v (%v), want steam-main-1", got, err)
	}
}

// A build removed after an update looked it up, but before the update
// could keep it, ends the update with no_build: nothing is backed up,
// stopped or switched.
func TestUpdateToABuildRemovedMeanwhile(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	if _, err := r.m.Create(CreateRequest{Name: "a", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil)}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := r.m.Start(ctx, "a", true)
	if err != nil {
		t.Fatal(err)
	}
	in, _ := r.m.get("a")
	in.op.Lock() // the update finds the build, then waits for the instance
	done := inBackground(func() error {
		_, err := r.m.Update(ctx, "a", "steam-main-2")
		return err
	})
	time.Sleep(300 * time.Millisecond)
	removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-2"})
	in.op.Unlock()
	if err != nil || !slices.Equal(removed, []string{"steam-main-2"}) {
		t.Fatalf("removed %v (%v)", removed, err)
	}
	if err := result(t, done); fault.CodeOf(err) != "no_build" {
		t.Fatalf("update to a build that is gone: %v", err)
	}
	if v, _ := r.m.Get("a"); v.Build != "steam-main-1" || v.Status.State != Running || v.Status.PID != st.PID {
		t.Fatalf("after the update: build %s, %+v", v.Build, v.Status)
	}
	if list, _ := r.m.ListBackups("a"); len(list) > 0 {
		t.Fatalf("backups: %+v", list)
	}
}

// The first start after an automatic update switched the build of a
// stopped instance goes back to the build it ran before when the new one
// does not get ready. Pruning keeps that build while the new one is
// waited for; afterwards the failed build can go.
func TestPruneDuringACheckedStartKeepsTheBuildToGoBackTo(t *testing.T) {
	r := newRig(t, func(c *config.Config) {
		c.KeepPreviousBuilds = 0
		c.ReadyTimeout = config.Duration(3 * time.Second)
	})
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2", Fake: map[string]any{"FakeNoReady": true}})
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		return "steam-main-2", time.Now(), src == "main"
	}
	r.createOn("a", "steam-main-1", nil)
	r.m.autoTick(time.Now())
	r.waitAuto("a")
	if st, _ := r.m.Status("a"); !st.UpdateUnverified || st.UpdateRollbackBuild != "steam-main-1" {
		t.Fatalf("after the automatic update: %+v", st)
	}
	done := inBackground(func() error {
		_, err := r.m.Start(context.Background(), "a", false)
		return err
	})
	r.waitWaitedFor("a", "steam-main-2")
	if removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2"}); err != nil || len(removed) > 0 {
		t.Errorf("a prune during the checked start removed %v (%v)", removed, err)
	}
	err := result(t, done)
	if fault.CodeOf(err) != "update_failed" || !strings.Contains(err.Error(), "rolled back to steam-main-1") {
		t.Fatalf("first start of a build that never gets ready: %v", err)
	}
	if v, _ := r.m.Get("a"); v.Build != "steam-main-1" || v.Status.State != Running || v.Status.UpdateUnverified {
		t.Fatalf("after the roll back: build %s, %+v", v.Build, v.Status)
	}
	removed, err := r.m.RemoveUnusedBuilds([]string{"steam-main-1", "steam-main-2"})
	if err != nil || !slices.Equal(removed, []string{"steam-main-2"}) {
		t.Fatalf("prune after the checked start removed %v (%v), want steam-main-2", removed, err)
	}
}
