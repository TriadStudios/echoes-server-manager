package page

import (
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones the tests name, on a machine without them

	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

func backupAt(id int, t time.Time, size int64) instances.Backup {
	return instances.Backup{ID: strconv.Itoa(id), Created: t, Reason: instances.ReasonScheduled, SizeBytes: size}
}

// goesNext lists the ids of the backups marked to go next, newest first.
func goesNext(d backupsData) string {
	ids := ""
	for _, g := range d.Groups {
		for _, r := range g.Rows {
			if r.GoesNext {
				ids += r.ID
			}
		}
	}
	return ids
}

// Backups are grouped by the calendar day of the box (Today, Yesterday,
// Older), the newest is marked, and the backups that the next backup made
// removes are marked as going next.
func TestMakeBackups(t *testing.T) {
	loc := time.FixedZone("box", 2*3600)
	now := time.Date(2026, 3, 10, 0, 30, 0, 0, loc) // just after midnight
	list := []instances.Backup{
		backupAt(1, now.Add(-40*time.Minute), 10),                 // 23:50 the day before: yesterday
		backupAt(2, now.Add(20*time.Minute), 20),                  // a clock a little ahead: still today
		backupAt(3, time.Date(2026, 3, 9, 8, 0, 0, 0, loc), 30),   // yesterday
		backupAt(4, time.Date(2026, 3, 8, 23, 59, 0, 0, loc), 40), // older
		backupAt(5, time.Date(2026, 2, 1, 10, 0, 0, 0, loc), 50),  // older
	}
	// The API gives them newest first; this list is made so that the groups
	// come out as they would for one.
	list = []instances.Backup{list[1], list[0], list[2], list[3], list[4]}
	d := makeBackups(list, 5, now, loc)
	type group struct {
		name string
		ids  string
	}
	var got []group
	for _, g := range d.Groups {
		ids := ""
		for _, r := range g.Rows {
			ids += r.ID
		}
		got = append(got, group{g.Name, ids})
	}
	want := []group{{"Today", "2"}, {"Yesterday", "13"}, {"Older", "45"}}
	if len(got) != len(want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d: %v, want %v", i, got[i], want[i])
		}
	}
	if d.Count != 5 || d.Keep != 5 || d.Step != 100 || d.Total != 150 || d.Last == nil || d.Last.ID != "2" {
		t.Errorf("summary: %+v", d)
	}
	if !d.Groups[0].Rows[0].Newest || d.Groups[1].Rows[0].Newest {
		t.Error("only the first backup is the newest")
	}
	if got := goesNext(d); got != "5" {
		t.Errorf("5 of 5 kept: goes next %q, want only the oldest, 5", got)
	}

	// The next backup is the newest of the keep that stay, so it removes
	// the backups from the keep-1th on: none while the list is shorter than
	// keep, the oldest when it is as long, more when it is longer.
	for keep, want := range map[int]string{6: "", 5: "5", 4: "45", 3: "345", 2: "1345", 1: "21345"} {
		if got := goesNext(makeBackups(list, keep, now, loc)); got != want {
			t.Errorf("5 backups, keep %d: goes next %q, want %q", keep, got, want)
		}
	}
	if d := makeBackups(list, 6, now, loc); d.Step != 85 || d.Keep != 6 {
		t.Errorf("5 of 6 kept: %+v", d)
	}
	if d := makeBackups(list, 3, now, loc); d.Step != 100 || d.Keep != 3 || d.Count != 5 {
		t.Errorf("5 of 3 kept: %+v", d)
	}
	// A policy that keeps one: the only backup is the newest and goes next,
	// since the next one replaces it. With none, nothing is marked.
	one := makeBackups(list[:1], 1, now, loc)
	if !one.Groups[0].Rows[0].GoesNext || !one.Groups[0].Rows[0].Newest {
		t.Errorf("one backup of one: %+v", one.Groups)
	}
	if got := goesNext(makeBackups(list[:1], 2, now, loc)); got != "" {
		t.Errorf("one backup of two: goes next %q", got)
	}
	if d := makeBackups(nil, 0, now, loc); d.Keep != instances.DefaultBackupKeep || len(d.Groups) != 0 || d.Last != nil || d.Step != 0 {
		t.Errorf("none: %+v", d)
	}
}

// The day a backup belongs to is the day in the box's zone, not in UTC.
func TestBackupDayFollowsTheBoxZone(t *testing.T) {
	loc := time.FixedZone("box", -8*3600)
	now := time.Date(2026, 3, 10, 20, 0, 0, 0, loc)              // 04:00 on the 11th in UTC
	b := backupAt(1, time.Date(2026, 3, 10, 9, 0, 0, 0, loc), 1) // 17:00 UTC on the 10th: yesterday in UTC, today here
	if g := makeBackups([]instances.Backup{b}, 10, now, loc).Groups; len(g) != 1 || g[0].Name != "Today" {
		t.Errorf("groups: %+v", g)
	}
	if g := makeBackups([]instances.Backup{b}, 10, now, time.UTC).Groups; len(g) != 1 || g[0].Name != "Yesterday" {
		t.Errorf("groups in UTC: %+v", g)
	}
}

// A day that skips midnight when the clocks change (Havana puts them
// forward at 00:00, so 8 March 2026 starts at 01:00) is still a day: the
// day before it is Yesterday, and a backup of that day is not Older.
func TestBackupDaysAcrossASummerTimeChange(t *testing.T) {
	loc, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatal(err)
	}
	// A midnight that does not exist is given as another time, here 23:00 of
	// the day before.
	if m := time.Date(2026, 3, 8, 0, 0, 0, 0, loc); m.Day() == 8 && m.Hour() == 0 {
		t.Skip("the zone data of this machine has no change of summer time at midnight on 8 March 2026")
	}
	for _, c := range []struct {
		name string
		now  time.Time
		at   time.Time
		want string
	}{
		{"the day itself", time.Date(2026, 3, 8, 12, 0, 0, 0, loc), time.Date(2026, 3, 8, 1, 30, 0, 0, loc), "Today"},
		{"the day before the change", time.Date(2026, 3, 8, 12, 0, 0, 0, loc), time.Date(2026, 3, 7, 15, 0, 0, 0, loc), "Yesterday"},
		{"the day before that", time.Date(2026, 3, 8, 12, 0, 0, 0, loc), time.Date(2026, 3, 6, 23, 0, 0, 0, loc), "Older"},
		{"the day after the change, a backup of the changing day", time.Date(2026, 3, 9, 12, 0, 0, 0, loc), time.Date(2026, 3, 8, 4, 0, 0, 0, loc), "Yesterday"},
		{"the day after the change, a backup of the day before", time.Date(2026, 3, 9, 12, 0, 0, 0, loc), time.Date(2026, 3, 7, 22, 0, 0, 0, loc), "Older"},
	} {
		g := makeBackups([]instances.Backup{backupAt(1, c.at, 1)}, 10, c.now, loc).Groups
		if len(g) != 1 || g[0].Name != c.want {
			t.Errorf("%s: %+v, want %s", c.name, g, c.want)
		}
	}
}

// The rows marked to go next are dimmed and say why in words that hold for
// each of them.
func TestBackupsScreenMarksWhatGoesNext(t *testing.T) {
	now := time.Now()
	list := []instances.Backup{backupAt(1, now.Add(-time.Hour), 10), backupAt(2, now.Add(-2*time.Hour), 10), backupAt(3, now.Add(-3*time.Hour), 10)}
	s := instances.View{Instance: instances.Instance{Name: "a", Backup: instances.BackupPolicy{Keep: 2}}}
	out := renderScreen(t, "backups", "admin", &serverData{S: s, Tab: "backups", More: makeBackups(list, 2, now, time.Local)})
	if n := strings.Count(out, `<tr class="goes">`); n != 2 {
		t.Errorf("%d rows go next, want 2 (the second and the third of 3, with 2 kept)", n)
	}
	const why = `title="The policy keeps the newest 2 backups, so the next backup made removes this one."`
	if n := strings.Count(out, why); n != 2 {
		t.Errorf("%d rows say why they go next, want 2", n)
	}
	s.Backup.Keep = 1
	out = renderScreen(t, "backups", "admin", &serverData{S: s, Tab: "backups", More: makeBackups(list[:1], 1, now, time.Local)})
	if !strings.Contains(out, `title="The policy keeps the newest 1 backup, so the next backup made removes this one."`) || strings.Count(out, `<tr class="goes">`) != 1 {
		t.Error("the only backup of a policy that keeps one goes next, and the tag says it keeps the newest 1 backup")
	}
}
