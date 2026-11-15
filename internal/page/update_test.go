package page

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// A server has one update status, the first of five that holds: failed,
// waiting, available, a new build not checked yet, up to date.
func TestUpdateStatus(t *testing.T) {
	avail := instances.UpdateInfo{Source: "main", Target: "steam-main-2"}
	for _, c := range []struct {
		name string
		v    instances.View
		want updateState
	}{
		{"nothing", instances.View{}, updateState{"ok", "Up to date"}},
		{"available", instances.View{UpdateAvailable: true, Update: avail}, updateState{"avail", "Update available"}},
		{"pending, no reason yet", instances.View{UpdateAvailable: true, Update: instances.UpdateInfo{Target: "x", Pending: true}}, updateState{"avail", "Update available"}},
		{"waiting for players", instances.View{UpdateAvailable: true, Update: instances.UpdateInfo{Target: "x", Pending: true, Waiting: "3 players are online"}}, updateState{"wait", "Waiting"}},
		{"paused after an update by hand", instances.View{UpdateAvailable: true, Update: instances.UpdateInfo{Target: "x", Waiting: "automatic updates wait for a build newer than steam-main-1"}}, updateState{"wait", "Waiting"}},
		// A failed update also sets Waiting (it is not tried again): the
		// failure is what the pill says.
		{"failed", instances.View{UpdateAvailable: true, Update: instances.UpdateInfo{Target: "x", Waiting: "the automatic update to x failed", LastError: "never got ready"}}, updateState{"fail", "Update failed"}},
		{"new build not checked", instances.View{Status: instances.Status{UpdateUnverified: true, UpdateRollbackBuild: "steam-main-1"}}, updateState{"info", "New build, checked at next start"}},
		{"new build and a newer one", instances.View{UpdateAvailable: true, Update: avail, Status: instances.Status{UpdateUnverified: true}}, updateState{"avail", "Update available"}},
	} {
		if got := updateStatus(c.v); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The pill and the card's words come from the status, and what the API
// returned is escaped.
func TestUpdatePillTemplate(t *testing.T) {
	p := NewWithLogger(nil, slog.New(slog.DiscardHandler)).(*Page)
	v := instances.View{Instance: instances.Instance{Name: "a", AutoUpdate: instances.AutoUpdateRestart,
		UpdateMaxWait: config.Duration(6 * time.Hour)}, UpdateAvailable: true,
		Update: instances.UpdateInfo{Source: "main", Target: "steam-main-<2>", Pending: true, Waiting: "3 players are online", Overdue: true}}
	var b bytes.Buffer
	if err := p.tmpl["dashboard"].ExecuteTemplate(&b, "update-pill", v); err != nil {
		t.Fatal(err)
	}
	if want := `<span class="pill wait"><span class="dot" aria-hidden="true"></span>Waiting</span>`; b.String() != want {
		t.Errorf("pill:\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	v = instances.View{Status: instances.Status{UpdateUnverified: true}}
	if err := p.tmpl["overview"].ExecuteTemplate(&b, "update-pill", v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `class="pill info"`) || !strings.Contains(b.String(), "New build, checked at next start") {
		t.Errorf("pill of an unproven build:\n%s", b.String())
	}
}

func buildOf(version, channel string, installed time.Time) api.BuildView {
	return api.BuildView{Build: builds.Build{Version: version, Channel: channel, Installed: installed}}
}

// The update form lists the builds of the server's channel, newest first,
// marks the one the server runs (which cannot be chosen) and the newest,
// and starts on the newest unless the server runs it.
func TestUpdateChoices(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	all := []api.BuildView{
		buildOf("1.0", "live", at),
		buildOf("1.2", "live", at.Add(48*time.Hour)),
		buildOf("1.1", "live", at.Add(24*time.Hour)),
		buildOf("2.0-beta", "test", at.Add(72*time.Hour)),
	}
	var versions []string
	c := updateChoices(instances.View{Instance: instances.Instance{Channel: "live", Build: "1.0"}}, all)
	for _, o := range c.Options {
		versions = append(versions, o.Version)
	}
	if strings.Join(versions, " ") != "1.2 1.1 1.0" || c.Others != 2 || !c.Selected || c.Behind != 0 {
		t.Fatalf("on 1.0: %+v", c)
	}
	if o := c.Options; !o[0].Newest || !o[0].Selected || o[1].Newest || o[1].Selected || !o[2].Running || o[2].Selected {
		t.Errorf("marks on 1.0: %+v", o)
	}
	// The newest build is the one running: nothing is chosen for the operator.
	c = updateChoices(instances.View{Instance: instances.Instance{Channel: "live", Build: "1.2"}}, all)
	if c.Selected || c.Others != 2 || !c.Options[0].Running || !c.Options[0].Newest {
		t.Errorf("on the newest: %+v", c)
	}
	for _, o := range c.Options {
		if o.Selected {
			t.Errorf("on the newest: %s is selected", o.Version)
		}
	}
	// The build the Steam source gives is chosen, even when another was
	// installed later.
	c = updateChoices(instances.View{Instance: instances.Instance{Channel: "live", Build: "1.0"}, Update: instances.UpdateInfo{Target: "1.1"}}, all)
	if !c.Options[1].Selected || c.Options[0].Selected {
		t.Errorf("with a target: %+v", c.Options)
	}
	// Only the running build installed: nothing to update to.
	c = updateChoices(instances.View{Instance: instances.Instance{Channel: "test", Build: "2.0-beta"}}, all)
	if c.Others != 0 || len(c.Options) != 1 || c.Selected {
		t.Errorf("alone: %+v", c)
	}
	if c = updateChoices(instances.View{Instance: instances.Instance{Channel: "live", Build: "1.0"}}, nil); len(c.Options) != 0 || c.Others != 0 {
		t.Errorf("no builds: %+v", c)
	}
}

// How far behind a server is can be told only for a build of a Steam
// source, by its build id (compared as a number); for any other build the
// page does not say.
func TestBuildsBehind(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	list := []api.BuildView{
		buildOf("steam-main-1000", "live", at.Add(3*time.Hour)),
		buildOf("steam-main-999", "live", at.Add(2*time.Hour)),
		buildOf("steam-main-100", "live", at.Add(time.Hour)),
		buildOf("steam-other-5000", "live", at.Add(4*time.Hour)),
		buildOf("1.1", "live", at.Add(5*time.Hour)),
	}
	for build, want := range map[string]int{
		"steam-main-100":  2, // 999 and 1000: 1000 is more than 999, not less as text
		"steam-main-999":  1,
		"steam-main-1000": 0,
		"steam-main-0099": 3, // leading zeros do not count
		"1.0":             0, // not from a source: not told, however many were installed later
		"steam-main-":     0,
	} {
		if got := buildsBehind(build, list); got != want {
			t.Errorf("buildsBehind(%q) = %d, want %d", build, got, want)
		}
	}
	c := updateChoices(instances.View{Instance: instances.Instance{Channel: "live", Build: "steam-main-100"}}, list)
	if c.Behind != 2 {
		t.Errorf("behind in the form: %d", c.Behind)
	}
}
