package instances

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
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
//     raises a warning event once (it keeps waiting). Right before the
//     server is stopped (after the backup) the player count is asked again:
//     a player who joined meanwhile puts the update off. An update that is
//     refused because the instance is busy is tried again later. An
//     automatic update that failed is not tried again for the same build.
//
// An admin who updates an instance by hand to an older build of its source
// pauses its automatic updates until the source has a build newer than the
// one the instance left. The failed build and the pause are kept in
// run/autoupdate.json, so they hold across restarts of ESM.
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
	// Action: available, started, updated, failed, postponed, overdue,
	// paused.
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
	// holdSource and holdBuildID pause automatic updates after a manual
	// update to an older build: until the source has a build id above
	// holdBuildID.
	holdSource  string
	holdBuildID string
}

// autoFile is what of autoState is kept in run/autoupdate.json.
type autoFile struct {
	Failed      string `json:"failed,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	HoldSource  string `json:"hold_source,omitempty"`
	HoldBuildID string `json:"hold_buildid,omitempty"`
}

func autoFileRel(name string) string { return instDir(name) + "/run/autoupdate.json" }

// saveAutoLocked writes the lasting part of the instance's auto state.
// in.mu must be held.
func (m *Manager) saveAutoLocked(in *rt) {
	if in.deleted {
		return
	}
	f := autoFile{Failed: in.auto.failed, LastError: in.auto.lastError, HoldSource: in.auto.holdSource, HoldBuildID: in.auto.holdBuildID}
	if f == (autoFile{}) {
		m.st.Remove(autoFileRel(in.name))
		return
	}
	if err := m.st.WriteJSON(autoFileRel(in.name), f, 0o600); err != nil {
		m.log.Warn("cannot write the automatic update state", "instance", in.name, "err", err)
	}
}

// loadAuto reads the lasting part of an instance's auto state.
func (m *Manager) loadAuto(name string) autoState {
	var f autoFile
	if err := m.st.ReadJSON(autoFileRel(name), &f); err != nil {
		return autoState{}
	}
	return autoState{failed: f.Failed, lastError: f.LastError, holdSource: f.HoldSource, holdBuildID: f.HoldBuildID}
}

// idLess compares two build ids (digits) as numbers.
func idLess(a, b string) bool {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// held reports whether a manual downgrade pauses the update to target.
func (a autoState) held(target string) bool {
	src, id, ok := steam.SourceOf(target)
	return ok && a.holdSource == src && !idLess(a.holdBuildID, id)
}

func (a autoState) holdText() string {
	return "automatic updates wait for a build newer than " + steam.Version(a.holdSource, a.holdBuildID) +
		" (the instance was updated by hand to an older build)"
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
	if a.held(target) {
		u.Waiting = a.holdText()
		return u
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
			// place right now): forget the wait and the last failure; keep
			// a running update and a pause.
			old := in.auto
			in.auto = autoState{running: old.running, holdSource: old.holdSource, holdBuildID: old.holdBuildID}
			if old.failed != "" || old.lastError != "" {
				m.saveAutoLocked(in)
			}
			in.mu.Unlock()
			continue
		}
		if in.auto.holdSource != "" && !in.auto.held(target) {
			// A build newer than the one left by hand: the pause is over.
			in.auto.holdSource, in.auto.holdBuildID = "", ""
			m.saveAutoLocked(in)
		}
		if in.auto.target != target {
			in.auto.target, in.auto.since, in.auto.waiting, in.auto.warned = target, now, "", false
			in.mu.Unlock()
			m.log.Info("update available", "instance", cfg.Name, "from", cfg.Build, "to", target, "auto_update", cfg.AutoUpdate)
			m.publishUpdate(cfg.Name, cfg.OutputHidden(), UpdateEvent{Action: "available", From: cfg.Build, To: target,
				Message: "build " + target + " is available (auto_update " + cfg.AutoUpdate + ")"})
			in.mu.Lock()
		}
		skip := cfg.AutoUpdate != AutoUpdateRestart || in.auto.failed == target || in.auto.running || in.auto.held(target)
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

// noteManualUpdate pauses automatic updates after a manual update to an
// older build of the same source, and ends a pause after any other manual
// update.
func (m *Manager) noteManualUpdate(in *rt, from, to string) {
	fs, fid, ok1 := steam.SourceOf(from)
	ts, tid, ok2 := steam.SourceOf(to)
	in.mu.Lock()
	down := ok1 && ok2 && fs == ts && idLess(tid, fid)
	if down {
		in.auto.holdSource, in.auto.holdBuildID = fs, fid
	} else {
		in.auto.holdSource, in.auto.holdBuildID = "", ""
	}
	m.saveAutoLocked(in)
	text, hidden := in.auto.holdText(), in.cfg.OutputHidden()
	in.mu.Unlock()
	if down {
		m.publishUpdate(in.name, hidden, UpdateEvent{Action: "paused", From: from, To: to, Message: text})
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
	_, err := m.update(context.Background(), cfg.Name, target, true)
	putOff := err != nil && fault.Is(err, fault.Conflict)
	in.mu.Lock()
	in.auto.running = false
	switch {
	case putOff:
		// Busy, or a player joined: tried again at a later tick.
		in.auto.waiting = err.Error()
	case err != nil:
		in.auto.failed, in.auto.lastError = target, err.Error()
	default:
		in.auto = autoState{}
	}
	m.saveAutoLocked(in)
	in.mu.Unlock()
	if putOff {
		m.log.Info("automatic update put off", "instance", cfg.Name, "to", target, "why", err)
		m.publishUpdate(cfg.Name, hidden, UpdateEvent{Action: "postponed", From: cfg.Build, To: target, Message: err.Error()})
		m.auditAuto(audit.Entry{Action: "instance.auto_update.postponed", Target: cfg.Name + " to " + target, Error: err.Error()})
		return
	}
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

// beforeAutoStop, when set (tests only), runs in an automatic update right
// before the player count is asked again.
var beforeAutoStop func(name string)
