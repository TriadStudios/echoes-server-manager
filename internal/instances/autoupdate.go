package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
)

// An instance follows the Steam source its current build came from (a
// build steam-<source>-<buildid>; see steam.SourceOf). When that source's
// newest build (Options.SteamTarget) is another installed build of the
// instance's channel, the instance has an update available. What happens
// then is its auto_update:
//
//   - off and install: nothing more. The source's checks have installed the
//     build; the instance shows it as available.
//   - restart: the scheduler updates the instance (the usual update: a
//     backup, one stop signal, a start of the new build, and a roll back
//     when it does not become ready) as soon as its server answers the
//     query port with 0 players, or right away when it is stopped. While
//     players are on, or the count is not known, it waits and asks again
//     at the next scheduler tick. Waiting longer than update_max_wait
//     raises a warning event once (it keeps waiting). An automatic update
//     that failed is not tried again for the same build.
//
// Every step is an "update" event, and each automatic update and each
// overdue warning is in the audit log (actor kind "system").

// UpdateInfo is what an instance's view says about a newer build.
type UpdateInfo struct {
	// Source is the Steam source the instance's build came from ("" when
	// it follows none).
	Source string `json:"source"`
	// Target is the newer build ("" when there is none).
	Target string `json:"target"`
	// Since is when this instance first saw Target.
	Since time.Time `json:"since,omitzero"`
	// Pending says that the instance will update itself (auto_update
	// restart) and waits for the moment.
	Pending bool `json:"pending"`
	// Waiting says why a pending update has not run yet.
	Waiting string `json:"waiting"`
	// Overdue says that it has waited longer than update_max_wait.
	Overdue bool `json:"overdue"`
	// LastError is why the last automatic update failed ("" when none did).
	LastError string `json:"last_error"`
}

// UpdateEvent is the data of an "update" event.
type UpdateEvent struct {
	// Action: available, started, updated, failed, overdue.
	Action  string `json:"action"`
	From    string `json:"from"`
	To      string `json:"to"`
	Message string `json:"message"`
}

// autoState is what the scheduler keeps per instance.
type autoState struct {
	target  string
	since   time.Time
	waiting string
	warned  bool
	running bool
	// failed is the target whose automatic update failed, and lastError why.
	failed    string
	lastError string
}

// PlayerQueryTimeout bounds the player count query before an automatic
// update.
const PlayerQueryTimeout = time.Second

// updateTarget returns the build an instance on build (channel) could
// update to, and the source it follows.
func (m *Manager) updateTarget(build, channel string) (source, target string) {
	src, _, ok := steam.SourceOf(build)
	if !ok || m.opts.SteamTarget == nil {
		return "", ""
	}
	t, _, ok := m.opts.SteamTarget(src)
	if !ok || t == build {
		return src, ""
	}
	b, err := m.builds.Get(t)
	if err != nil || b.Channel != channel {
		return src, ""
	}
	return src, t
}

// updateInfo is the UpdateInfo of an instance with settings cfg and
// scheduler state a.
func (m *Manager) updateInfo(cfg Instance, a autoState) UpdateInfo {
	src, target := m.updateTarget(cfg.Build, cfg.Channel)
	u := UpdateInfo{Source: src, Target: target, LastError: a.lastError}
	if target == "" {
		return u
	}
	if a.target == target {
		u.Since = a.since
	}
	if cfg.AutoUpdate == AutoUpdateRestart && a.failed != target {
		u.Pending = true
		u.Waiting = a.waiting
		u.Overdue = a.warned
	}
	if a.failed == target {
		u.Waiting = "the automatic update to " + target + " failed; it is not tried again for this build (update by hand to try again)"
	}
	return u
}

// publishUpdate publishes an update event. For an instance whose output is
// hidden from the read role, that role gets it without the message (which
// may repeat a failure).
func (m *Manager) publishUpdate(name string, hidden bool, ev UpdateEvent) {
	if hidden {
		pub := ev
		pub.Message = ""
		m.bus.PublishRestricted(events.TypeUpdate, name, ev, pub)
		return
	}
	m.bus.Publish(events.TypeUpdate, name, ev)
}

func (m *Manager) auditAuto(e audit.Entry) {
	if m.opts.Audit == nil {
		return
	}
	e.Actor = audit.Actor{Kind: audit.ActorSystem, ID: "auto-update"}
	if err := m.opts.Audit.Append(e); err != nil {
		m.log.Error("cannot write the audit log", "err", err)
	}
}

// autoTick looks at every instance's update and starts the automatic
// updates that may run now.
func (m *Manager) autoTick(now time.Time) {
	if m.opts.SteamTarget == nil {
		return
	}
	for _, in := range m.all() {
		in.mu.Lock()
		cfg := in.cfg.clone()
		state := in.status.State
		in.mu.Unlock()
		_, target := m.updateTarget(cfg.Build, cfg.Channel)
		in.mu.Lock()
		if target == "" {
			// Nothing to update to (or an update has the new build in
			// place right now): forget the wait, keep what an update
			// that runs or failed needs.
			in.auto = autoState{failed: in.auto.failed, lastError: in.auto.lastError, running: in.auto.running}
			in.mu.Unlock()
			continue
		}
		if in.auto.target != target {
			in.auto.target, in.auto.since, in.auto.waiting, in.auto.warned = target, now, "", false
			in.mu.Unlock()
			m.log.Info("update available", "instance", cfg.Name, "from", cfg.Build, "to", target, "auto_update", cfg.AutoUpdate)
			m.publishUpdate(cfg.Name, cfg.OutputHidden(), UpdateEvent{Action: "available", From: cfg.Build, To: target,
				Message: "build " + target + " is available (auto_update " + cfg.AutoUpdate + ")"})
			in.mu.Lock()
		}
		skip := cfg.AutoUpdate != AutoUpdateRestart || in.auto.failed == target || in.auto.running
		in.mu.Unlock()
		if skip {
			continue
		}
		waiting := ""
		switch state {
		case Stopped:
		case Running:
			waiting = m.playersWaiting(cfg)
		default:
			waiting = "the instance is " + string(state)
		}
		in.mu.Lock()
		in.auto.waiting = waiting
		since, warned := in.auto.since, in.auto.warned
		overdue := waiting != "" && !warned && now.Sub(since) >= cfg.UpdateMaxWait.D()
		if overdue {
			in.auto.warned = true
		}
		start := waiting == "" && !in.auto.running
		if start {
			in.auto.running = true
		}
		in.mu.Unlock()
		if overdue {
			msg := fmt.Sprintf("the update to %s has waited longer than update_max_wait (%s): %s; it keeps waiting", target, cfg.UpdateMaxWait.D(), waiting)
			m.log.Warn("automatic update waits too long", "instance", cfg.Name, "to", target, "waiting", waiting)
			m.publishUpdate(cfg.Name, cfg.OutputHidden(), UpdateEvent{Action: "overdue", From: cfg.Build, To: target, Message: msg})
			m.auditAuto(audit.Entry{Action: "instance.auto_update.overdue", Target: cfg.Name + " to " + target, Error: msg})
		}
		if start {
			if !m.track() {
				in.mu.Lock()
				in.auto.running = false
				in.mu.Unlock()
				return
			}
			go m.autoUpdate(in, cfg, target)
		}
	}
}

// playersWaiting asks the server for its player count and returns why an
// update must wait, or "" when the server is empty.
func (m *Manager) playersWaiting(cfg Instance) string {
	ctx, cancel := context.WithTimeout(context.Background(), PlayerQueryTimeout)
	defer cancel()
	info, err := gameserver.QueryInfo(ctx, cfg.BindIP, cfg.Ports.Query)
	switch {
	case err != nil:
		return "the player count is not known (no answer on the query port); waiting for an answer of 0 players"
	case info.Players == 1:
		return "1 player is online"
	case info.Players > 0:
		return fmt.Sprintf("%d players are online", info.Players)
	}
	return ""
}

// autoUpdate runs one automatic update.
func (m *Manager) autoUpdate(in *rt, cfg Instance, target string) {
	defer m.wg.Done()
	hidden := cfg.OutputHidden()
	m.log.Info("automatic update", "instance", cfg.Name, "from", cfg.Build, "to", target)
	m.publishUpdate(cfg.Name, hidden, UpdateEvent{Action: "started", From: cfg.Build, To: target,
		Message: "updating from " + cfg.Build + " to " + target + " (auto_update restart)"})
	_, err := m.Update(context.Background(), cfg.Name, target)
	in.mu.Lock()
	in.auto.running = false
	if err != nil {
		in.auto.failed, in.auto.lastError = target, err.Error()
	} else {
		in.auto = autoState{}
	}
	in.mu.Unlock()
	e := audit.Entry{Action: "instance.auto_update", Target: cfg.Name + " to " + target, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
		m.log.Warn("automatic update failed", "instance", cfg.Name, "to", target, "err", err)
		m.publishUpdate(cfg.Name, hidden, UpdateEvent{Action: "failed", From: cfg.Build, To: target, Message: err.Error()})
	} else {
		m.publishUpdate(cfg.Name, hidden, UpdateEvent{Action: "updated", From: cfg.Build, To: target, Message: "updated to " + target})
	}
	m.auditAuto(e)
}
