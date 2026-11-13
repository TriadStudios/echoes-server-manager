package instances

import (
	"encoding/json"
	"errors"
	"io/fs"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
	"github.com/TriadStudios/echoes-server-manager/internal/sysinfo"
)

// Live is what ESM reads about an instance's server at the moment it is
// asked: its memory, and the player count and last save from its status
// file. It is not stored and not sent in events. A value that cannot be
// known is null, and Unknown says why.
type Live struct {
	// MemoryBytes is the memory the server uses now. MemorySource says how
	// it was measured: "cgroup" (the server's systemd unit, the server and
	// everything it started) or "process" (the resident memory of the
	// server process alone).
	MemoryBytes  *int64 `json:"memory_bytes"`
	MemorySource string `json:"memory_source"`
	// Players is the player count the running server wrote in its status
	// file. UNCONFIRMED for the real server (see gameserver.Status).
	Players *int `json:"players"`
	// LastSave is the last world save the status file names.
	LastSave *time.Time `json:"last_save"`
	// Unknown lists the values above that are null, and why. Never nil.
	Unknown []sysinfo.Unknown `json:"unknown"`
}

// Scheduled is the next scheduled restart and backup of an instance. Both
// happen only if the server is running then. Absent means none is planned
// (no restart times, backups off, or the scheduler has not planned the
// first backup yet).
type Scheduled struct {
	Restart time.Time `json:"restart,omitzero"`
	Backup  time.Time `json:"backup,omitzero"`
}

// live reads the live values of v.
func (m *Manager) live(v View) Live {
	l := Live{Unknown: []sysinfo.Unknown{}}
	unknown := func(field, reason string) {
		l.Unknown = append(l.Unknown, sysinfo.Unknown{Field: field, Reason: reason})
	}
	running := v.Status.State.active() && v.Status.PID > 0
	if !running {
		unknown("memory_bytes", "the server is not running")
	} else {
		unit := ""
		if m.sup.Kind() == supervisor.KindSystemd {
			unit = supervisor.UnitName(v.Name)
		}
		n, src, err := sysinfo.ProcessMemory(v.Status.PID, unit)
		if err != nil {
			unknown("memory_bytes", err.Error())
		} else {
			l.MemoryBytes, l.MemorySource = &n, src
		}
	}

	var s gameserver.Status
	why := ""
	b, err := m.st.ReadFile(dataDir(v.Name) + "/" + gameserver.StatusFileRel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		why = "the server has not written a status file (" + gameserver.StatusFileRel + ")"
	case err != nil:
		why = "cannot read the status file: " + err.Error()
	case json.Unmarshal(b, &s) != nil:
		why = "the status file is not valid JSON"
	}
	if why != "" {
		unknown("players", why)
		unknown("last_save", why)
		return l
	}
	if s.LastSave.IsZero() {
		unknown("last_save", "the status file names no world save yet")
	} else {
		t := s.LastSave.UTC()
		l.LastSave = &t
	}
	switch {
	case !running:
		unknown("players", "the server is not running")
	case s.PID != 0 && s.PID != v.Status.PID:
		unknown("players", "the status file is from an earlier run of the server")
	case s.State != gameserver.StateReady:
		unknown("players", "the server does not say it is ready yet")
	default:
		p := s.Players
		l.Players = &p
	}
	return l
}

// scheduledLocked returns the next scheduled restart and backup. in.mu must
// be held.
func (m *Manager) scheduledLocked(in *rt) Scheduled {
	s := Scheduled{Restart: in.nextRestart, Backup: in.nextBackup}
	if s.Restart.IsZero() && len(in.cfg.Schedule.RestartAt) > 0 {
		// The scheduler has not planned it yet; it will plan the same time.
		s.Restart = NextRestart(m.opts.Now(), in.cfg.Schedule.RestartAt)
	}
	if !s.Restart.IsZero() {
		s.Restart = s.Restart.UTC()
	}
	if !s.Backup.IsZero() {
		s.Backup = s.Backup.UTC()
	}
	return s
}
