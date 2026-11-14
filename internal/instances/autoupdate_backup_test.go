package instances

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// An automatic update that a player puts off after its backup leaves no
// backup behind, and never prunes older ones: a busy server whose update
// is put off again and again keeps its restore points.
func TestAutoUpdatePutOffKeepsRestorePoints(t *testing.T) {
	r, _ := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	if _, _, err := r.m.Patch("a", PatchRequest{Settings: Settings{Backup: &BackupPolicy{Keep: 3}}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // past the first save
	manual, err := r.m.Backup(ctx, "a", ReasonManual)
	if err != nil {
		t.Fatal(err)
	}
	data := r.m.st.Path() + "/" + dataDir("a")
	// Every attempt finds the server empty, makes its backup, and then a
	// player is on.
	beforeAutoStop = func(string) {
		if err := testkit.SetPlayers(data, 2); err != nil {
			t.Error(err)
		}
		time.Sleep(400 * time.Millisecond) // the fake reads it within 100ms
	}
	t.Cleanup(func() { beforeAutoStop = nil })
	for i := range 4 {
		if err := testkit.SetPlayers(data, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		r.m.autoTick(time.Now())
		r.waitAuto("a")
		v, _ := r.m.Get("a")
		if v.Build != "steam-main-1" || v.Status.State != Running || !strings.Contains(v.Update.Waiting, "2 players") {
			t.Fatalf("attempt %d: build %s, %s, %+v", i+1, v.Build, v.Status.State, v.Update)
		}
	}
	list, err := r.m.ListBackups("a")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range list {
		if b.ID == manual.ID {
			found = true
		}
		if b.Reason == ReasonPreUpdate {
			t.Errorf("a put-off update left its backup %s", b.ID)
		}
	}
	if !found {
		t.Fatalf("the manual backup was pruned by put-off updates: %+v", list)
	}
}

// When players are on at the moment an automatic update begins, it is put
// off before it makes any backup.
func TestAutoUpdatePutOffBeforeTheBackup(t *testing.T) {
	r, target := autoRig(t, 2)
	r.createOn("a", "steam-main-1", nil)
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	if err := testkit.SetPlayers(r.m.st.Path()+"/"+dataDir("a"), 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	_, err := r.m.update(ctx, "a", *target, true)
	if fault.CodeOf(err) != "players_online" || !strings.Contains(err.Error(), "3 players") {
		t.Fatalf("update with players on: %v", err)
	}
	if list, _ := r.m.ListBackups("a"); len(list) != 0 {
		t.Fatalf("a put-off update made backups: %+v", list)
	}
	if v, _ := r.m.Get("a"); v.Build != "steam-main-1" || v.Status.State != Running {
		t.Fatalf("after the put-off update: build %s, %s", v.Build, v.Status.State)
	}
	// The update is over: it holds the build it was to go to no longer.
	if users := r.m.BuildUsers(); len(users[*target]) > 0 {
		t.Fatalf("build users after the put-off update: %v", users)
	}
}
