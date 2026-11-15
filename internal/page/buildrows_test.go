package page

import (
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

func sized(version, channel string, installed time.Time, size int64, usedBy ...string) api.BuildView {
	return api.BuildView{Build: builds.Build{Version: version, Channel: channel, Installed: installed, SizeBytes: size}, UsedBy: usedBy}
}

func server(name, build string) instances.View {
	return instances.View{Instance: instances.Instance{Name: name, Build: build}}
}

// Builds come newest first with a Newest mark for each channel; each says
// plainly whether a server runs it, only keeps it for a rollback, or no
// server uses it; and the space that removing the unused ones frees is the
// sum of their sizes.
func TestBuildRows(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	list := []api.BuildView{
		sized("old", "live", at, 100, "alpha"),                       // alpha's rollback
		sized("mid", "live", at.Add(time.Hour), 200),                 // nobody
		sized("test-1", "test", at.Add(2*time.Hour), 400),            // nobody
		sized("new", "live", at.Add(3*time.Hour), 800, "alpha", "b"), // alpha runs it; b keeps it
		sized("test-0", "test", at.Add(-time.Hour), 1600, "b"),       // b runs it
	}
	servers := []instances.View{server("alpha", "new"), server("b", "test-0")}
	d := buildRows(list, servers)
	type want struct {
		version, status string
		newest          bool
		current, kept   string
	}
	for i, w := range []want{
		{"new", buildRunning, true, "alpha", "b"},
		{"test-1", buildUnused, true, "", ""},
		{"mid", buildUnused, false, "", ""},
		{"old", buildKept, false, "", "alpha"},
		{"test-0", buildRunning, false, "b", ""},
	} {
		r := d.Builds[i]
		cur, kept := "", ""
		if len(r.Current) > 0 {
			cur = r.Current[0]
		}
		if len(r.Kept) > 0 {
			kept = r.Kept[0]
		}
		if r.Version != w.version || r.Status != w.status || r.Newest != w.newest || cur != w.current || kept != w.kept {
			t.Errorf("row %d: %s %s newest=%v current=%v kept=%v; want %+v", i, r.Version, r.Status, r.Newest, r.Current, r.Kept, w)
		}
	}
	if d.Unused != 2 || d.Freed != 600 || d.Total != 3100 {
		t.Errorf("unused %d freeing %d of %d", d.Unused, d.Freed, d.Total)
	}
	if d := buildRows(nil, nil); len(d.Builds) != 0 || d.Unused != 0 || d.Freed != 0 {
		t.Errorf("no builds: %+v", d)
	}
}
