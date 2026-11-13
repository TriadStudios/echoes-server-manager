package instances

import (
	"slices"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
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
