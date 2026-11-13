package page

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// The note about a newer build says what waits and why, and escapes what
// it shows.
func TestUpdateNote(t *testing.T) {
	p := NewWithLogger(nil, slog.New(slog.DiscardHandler)).(*Page)
	v := instances.View{Instance: instances.Instance{Name: "a", AutoUpdate: instances.AutoUpdateRestart,
		UpdateMaxWait: config.Duration(6 * time.Hour)}, UpdateAvailable: true,
		Update: instances.UpdateInfo{Source: "main", Target: "steam-main-2", Pending: true, Waiting: "3 players are online", Overdue: true,
			LastError: "update failed <b>"}}
	var b bytes.Buffer
	if err := p.tmpl["dashboard"].ExecuteTemplate(&b, "update-note", v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"steam-main-2", "Waiting: 3 players are online.", "Waiting longer than 6h0m0s", "update failed &lt;b&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("note lacks %q:\n%s", want, out)
		}
	}
	v.Update = instances.UpdateInfo{Source: "main", Target: "steam-main-2"}
	v.AutoUpdate = instances.AutoUpdateInstall
	b.Reset()
	if err := p.tmpl["overview"].ExecuteTemplate(&b, "update-note", v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "auto_update is install: update by hand.") {
		t.Errorf("install mode note:\n%s", b.String())
	}
	// A build switched in while the server was stopped is checked at the
	// next start; the note says so and names the build to go back to.
	v = instances.View{Instance: instances.Instance{Name: "a", Build: "steam-main-2", AutoUpdate: instances.AutoUpdateRestart},
		Status: instances.Status{State: instances.Stopped, UpdateUnverified: true, UpdateRollbackBuild: "steam-main-1"}}
	b.Reset()
	if err := p.tmpl["overview"].ExecuteTemplate(&b, "update-note", v); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"New build, checked at next start", "steam-main-2", "goes back to <span class=\"mono\">steam-main-1</span>"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("note of an unproven build lacks %q:\n%s", want, b.String())
		}
	}
}
