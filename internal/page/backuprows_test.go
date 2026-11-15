package page

import (
	"strconv"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

func backupAt(id int, t time.Time, size int64) instances.Backup {
	return instances.Backup{ID: strconv.Itoa(id), Created: t, Reason: instances.ReasonScheduled, SizeBytes: size}
}

// Backups are grouped by the calendar day of the box (Today, Yesterday,
// Older), the newest is marked, and when the list holds as many backups as
// the policy keeps the oldest is marked as the one that goes next.
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
	last := d.Groups[2].Rows[1]
	if !last.GoesNext || d.Groups[2].Rows[0].GoesNext || d.Groups[0].Rows[0].GoesNext {
		t.Errorf("only the oldest goes next: %+v", d.Groups)
	}

	// Below the keep limit nothing goes next; at it (or above, which a
	// protected backup can cause) the oldest does.
	if d := makeBackups(list, 6, now, loc); d.Groups[2].Rows[1].GoesNext || d.Step != 85 {
		t.Errorf("5 of 6 kept: goes next %v, step %d", d.Groups[2].Rows[1].GoesNext, d.Step)
	}
	if d := makeBackups(list, 3, now, loc); !d.Groups[2].Rows[1].GoesNext || d.Step != 100 || d.Keep != 3 {
		t.Errorf("5 of 3 kept: %+v", d)
	}
	// A policy with no keep (an old file) means the default; a single backup
	// is the newest and is not marked to go.
	one := makeBackups(list[:1], 1, now, loc)
	if one.Groups[0].Rows[0].GoesNext || !one.Groups[0].Rows[0].Newest {
		t.Errorf("one backup of one: %+v", one.Groups)
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
