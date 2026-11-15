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
	// GoesNext marks the oldest backup when the list holds as many as the
	// policy keeps: the next backup made removes it.
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
	yesterday := today.AddDate(0, 0, -1)
	for i, b := range list {
		d.Total += b.SizeBytes
		row := backupRow{Backup: b, Newest: i == 0, GoesNext: i == len(list)-1 && i > 0 && len(list) >= keep}
		name := "Older"
		switch day := dayOf(b.Created, loc); {
		case !day.Before(today):
			name = "Today"
		case day.Equal(yesterday):
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

// dayOf is the start of t's calendar day in loc.
func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}
