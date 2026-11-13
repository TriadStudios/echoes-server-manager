package instances

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The update modes, the wait for an empty server and the one warning
// after update_max_wait, driven tick by tick.
func TestAutoUpdateWaitsAndWarnsOnce(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "steam-main-1"})
	r.install(testkit.BuildOptions{Version: "steam-main-2"})
	r.m.opts.SteamTarget = func(src string) (string, time.Time, bool) {
		if src != "main" {
			return "", time.Time{}, false
		}
		return "steam-main-2", time.Now(), true
	}
	v, err := r.m.Create(CreateRequest{Name: "a", Build: "steam-main-1", Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(nil),
		AutoUpdate: ptr(AutoUpdateInstall), UpdateMaxWait: ptr(config.Duration(time.Minute))}})
	if err != nil {
		t.Fatal(err)
	}
	if !v.UpdateAvailable || v.Update.Source != "main" || v.Update.Target != "steam-main-2" || v.Update.Pending {
		t.Fatalf("install mode: %+v", v.Update)
	}
	if _, err := r.m.Start(context.Background(), "a", true); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SetPlayers(r.m.st.Path()+"/"+dataDir("a"), 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	sub := r.bus.Subscribe(events.Filter{Types: []string{events.TypeUpdate}}, 64)
	defer sub.Close()
	now := time.Now()
	r.m.autoTick(now)
	if v, _ := r.m.Get("a"); v.Update.Pending || v.Build != "steam-main-1" {
		t.Fatalf("install mode updated or is pending: %+v", v.Update)
	}
	if _, _, err := r.m.Patch("a", PatchRequest{Settings: Settings{AutoUpdate: ptr(AutoUpdateRestart)}}); err != nil {
		t.Fatal(err)
	}
	r.m.autoTick(now.Add(10 * time.Second))
	v, _ = r.m.Get("a")
	if !v.Update.Pending || !strings.Contains(v.Update.Waiting, "3 players") || v.Update.Overdue || v.Build != "steam-main-1" {
		t.Fatalf("restart mode with players: %+v", v.Update)
	}
	r.m.autoTick(now.Add(2 * time.Minute))
	r.m.autoTick(now.Add(3 * time.Minute))
	v, _ = r.m.Get("a")
	if !v.Update.Overdue || v.Build != "steam-main-1" {
		t.Fatalf("after update_max_wait: %+v", v.Update)
	}
	var actions []string
	for done := false; !done; {
		select {
		case ev := <-sub.C:
			var u UpdateEvent
			json.Unmarshal(ev.Data, &u)
			actions = append(actions, u.Action)
		case <-time.After(200 * time.Millisecond):
			done = true
		}
	}
	if strings.Join(actions, ",") != "available,overdue" {
		t.Fatalf("update events %v, want available then one overdue", actions)
	}
	if st, _ := r.m.Status("a"); st.State != Running {
		t.Fatalf("state %s", st.State)
	}
}
