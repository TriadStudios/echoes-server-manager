package page

import (
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// backupRow is one backup in the list on the Backups screen.
type backupRow struct {
	instances.Backup
	// Newest marks the last backup made.
	Newest bool
	// GoesNext marks a backup that the next backup made removes: pruning
	// keeps the newest keep backups, the new one among them, so it removes
	// the backups from the (keep-1)th of this list on. That is the oldest
	// one when the list holds as many as the policy keeps, and more when
	// it holds more.
	GoesNext bool
}

// backupGroup is the backups of one day of the list: Today, Yesterday or
// Older.
type backupGroup struct {
	Name string
	Rows []backupRow
}

type backupsData struct {
	// Groups are the backups, newest first, by day.
	Groups []backupGroup
	// Last is the newest backup, nil when there is none.
	Last *instances.Backup
	// Count backups are kept of Keep, which Step shows as a bar of steps of
	// 5 percent; Total is their size together.
	Count, Keep, Step int
	Total             int64
	Why               string
}

// makeBackups makes the data of the Backups screen from the list the API
// gave (newest first) and the policy's keep. A day is a calendar day in
// loc, as now is: the box's own time zone, which the schedules use.
func makeBackups(list []instances.Backup, keep int, now time.Time, loc *time.Location) backupsData {
	if keep < 1 {
		keep = instances.DefaultBackupKeep
	}
	d := backupsData{Count: len(list), Keep: keep, Step: step(int64(len(list)), int64(keep))}
	if len(list) > 0 {
		d.Last = &list[0]
	}
	today := dayOf(now, loc)
	yesterday := dayBefore(now, loc)
	for i, b := range list {
		d.Total += b.SizeBytes
		row := backupRow{Backup: b, Newest: i == 0, GoesNext: i >= keep-1}
		name := "Older"
		switch day := dayOf(b.Created, loc); {
		case day >= today:
			name = "Today"
		case day == yesterday:
			name = "Yesterday"
		}
		if n := len(d.Groups); n > 0 && d.Groups[n-1].Name == name {
			d.Groups[n-1].Rows = append(d.Groups[n-1].Rows, row)
		} else {
			d.Groups = append(d.Groups, backupGroup{Name: name, Rows: []backupRow{row}})
		}
	}
	return d
}

// dayOf names the calendar day of t in loc by its date, as a number that
// is greater for a later day (20260310). Days are compared by date, never
// by their first moment: a day that skips midnight in a change of summer
// time has no midnight to compare.
func dayOf(t time.Time, loc *time.Location) int {
	y, m, d := t.In(loc).Date()
	return y*10000 + int(m)*100 + d
}

// dayBefore is the calendar day before the one of t in loc.
func dayBefore(t time.Time, loc *time.Location) int {
	y, m, d := t.In(loc).Date()
	// At noon, which no change of summer time falls on.
	return dayOf(time.Date(y, m, d-1, 12, 0, 0, 0, loc), loc)
}
