package instances

import (
	"context"
	"time"
)

// schedLoop runs scheduled restarts and backups until the manager closes.
func (m *Manager) schedLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.opts.SchedulerInterval)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			return
		case <-t.C:
			m.schedTick(m.opts.Now())
		}
	}
}

// schedTick starts the restarts and backups that are due at now. Restart
// times are local wall-clock times; a slot that passes while the instance
// is not running is skipped.
func (m *Manager) schedTick(now time.Time) {
	for _, in := range m.all() {
		in.mu.Lock()
		times := in.cfg.Schedule.RestartAt
		every := in.cfg.Backup.Every.D()
		running := in.status.State == Running
		var restart, backup bool
		switch {
		case len(times) == 0:
			in.nextRestart = time.Time{}
		case in.nextRestart.IsZero():
			in.nextRestart = NextRestart(now, times)
		case !now.Before(in.nextRestart):
			restart = running
			in.nextRestart = NextRestart(now, times)
		}
		switch {
		case every <= 0:
			in.nextBackup = time.Time{}
		case in.nextBackup.IsZero():
			in.nextBackup = now.Add(every)
		case !now.Before(in.nextBackup):
			backup = running
			in.nextBackup = now.Add(every)
		}
		name := in.name
		in.mu.Unlock()
		if restart {
			go func() {
				if _, err := m.Restart(context.Background(), name, "scheduled restart", false); err != nil {
					m.log.Warn("scheduled restart failed", "instance", name, "err", err)
				}
			}()
		}
		if backup {
			go func() {
				if _, err := m.Backup(context.Background(), name, ReasonScheduled); err != nil {
					m.log.Warn("scheduled backup failed", "instance", name, "err", err)
				}
			}()
		}
	}
}
