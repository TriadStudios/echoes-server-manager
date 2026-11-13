package instances

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// An automatic update never starts a server that is not running: an
// operator may have stopped it on purpose. For such an instance it backs
// up the saves and switches the build, and starts nothing (switchStopped).
// The new build is then unproven. The next start, whoever asks for it,
// checks it like an update does (startChecked): it starts the new build
// and waits for ready; when that fails, the build the instance ran before
// is put back and started, and automatic updates do not try the failed
// build again. Until then the build to go back to counts as in use, so
// pruning keeps it. The mark is kept in run/autoupdate.json.

// unverified is the unproven automatic update of an instance: build is the
// new build, not yet seen ready, and from the build the instance ran
// before, which the next start puts back when build does not get ready.
// Over several such updates from stays the build that last ran. The zero
// value is no such update.
type unverified struct {
	build string
	from  string
}

// switchStopped ends an automatic update of an instance whose server is
// not running (after its backup): the build is switched, nothing is
// started, and the instance is marked unverified. It runs inside update,
// which holds the old build (holdBuilds) until it returns: pruning keeps
// that build between the switch and the mark that names it. in.op must be
// held.
func (m *Manager) switchStopped(ctx context.Context, in *rt, cfg Instance, to string) (Status, error) {
	old := cfg.Build
	if err := m.setBuild(in, to, append([]string{old}, cfg.PreviousBuilds...)); err != nil {
		if serr := m.stopLocked(ctx, in, "automatic update to "+to+" not done: "+err.Error()); serr != nil {
			return m.mustStatus(in.name), errors.Join(err, serr)
		}
		return m.mustStatus(in.name), err
	}
	in.mu.Lock()
	if in.unverified.from == "" {
		in.unverified.from = old
	}
	in.unverified.build = to
	in.status.Build = to
	in.status.UpdateUnverified, in.status.UpdateRollbackBuild = true, in.unverified.from
	m.saveAutoLocked(in)
	from := in.unverified.from
	in.mu.Unlock()
	reason := "updated from " + old + " to " + to + " while stopped; nothing was started: the next start checks the new build (and goes back to " + from + " if it does not get ready)"
	// Settles the state (stopped) and any watcher of an earlier run.
	if err := m.stopLocked(ctx, in, reason); err != nil {
		return m.mustStatus(in.name), err
	}
	m.log.Info("instance updated while stopped; the next start checks the new build", "instance", in.name, "from", old, "to", to, "rollback", from)
	return m.mustStatus(in.name), nil
}

// unverifiedLocked returns the instance's unproven automatic update, if it
// has one. A mark that does not fit the instance's build any more is
// dropped. in.mu must be held.
func (m *Manager) unverifiedLocked(in *rt) (unverified, bool) {
	u := in.unverified
	if u.build == "" {
		return unverified{}, false
	}
	if u.build != in.cfg.Build {
		m.clearUnverifiedLocked(in)
		return unverified{}, false
	}
	return u, true
}

// clearUnverifiedLocked drops the instance's unproven-update mark and
// reports whether it had one. The caller publishes the status. in.mu must
// be held.
func (m *Manager) clearUnverifiedLocked(in *rt) bool {
	if in.unverified == (unverified{}) {
		return false
	}
	in.unverified = unverified{}
	in.status.UpdateUnverified, in.status.UpdateRollbackBuild = false, ""
	m.saveAutoLocked(in)
	return true
}

// startChecked is the first start of an instance after an automatic
// update switched its build while it was stopped. Like an update, it
// starts the new build and waits until it is ready, whatever the caller
// asked for. When it does not get ready, the new build is stopped, the
// build the instance ran before is put back and started, and the failed
// build is recorded so automatic updates do not try it again. Pruning
// keeps u.from throughout: the mark names it until setBuild has made it
// the current build. in.op must be held.
func (m *Manager) startChecked(ctx context.Context, in *rt, u unverified) (Status, error) {
	ctx = context.WithoutCancel(ctx)
	name := in.name
	r, err := m.launch(ctx, in, "started: first start of "+u.build+" after an automatic update", false)
	if err == nil {
		err = waitRun(ctx, r)
	}
	if err == nil {
		in.mu.Lock()
		if m.clearUnverifiedLocked(in) {
			m.emitLocked(in)
		}
		in.mu.Unlock()
		m.log.Info("automatically updated build ready at its first start", "instance", name, "build", u.build)
		return m.mustStatus(name), nil
	}
	if errors.Is(err, ErrClosed) {
		// ESM is shutting down; the mark stays for the next start.
		return m.mustStatus(name), err
	}
	cause := err
	m.log.Warn("automatically updated build did not get ready at its first start; rolling back", "instance", name, "build", u.build, "to", u.from, "err", cause)
	if err := m.stopLocked(ctx, in, "stopped after a failed update"); err != nil {
		return m.mustStatus(name), fault.Wrap(fault.Failed, "update_failed", err,
			"build %s did not get ready at its first start after an automatic update (%v) and could not be stopped", u.build, cause)
	}
	in.mu.Lock()
	prev := slices.Clone(in.cfg.PreviousBuilds)
	in.mu.Unlock()
	if err := m.setBuild(in, u.from, prev); err != nil {
		return m.mustStatus(name), err
	}
	msg := fmt.Sprintf("build %s did not get ready at its first start after an automatic update (%v); rolled back to %s", u.build, cause, u.from)
	in.mu.Lock()
	m.clearUnverifiedLocked(in)
	in.status.Build = u.from
	in.auto.failed, in.auto.lastError = u.build, msg
	m.saveAutoLocked(in)
	// Published now, not only by the start below: when that start fails
	// early, the state file and the last event must not keep the mark.
	m.emitLocked(in)
	hidden := in.cfg.OutputHidden()
	in.mu.Unlock()
	m.publishUpdate(name, hidden, UpdateEvent{Action: "failed", From: u.from, To: u.build, Message: msg})
	m.auditAuto(audit.Entry{Action: "instance.auto_update", Target: name + " to " + u.build, Error: msg})
	r, err = m.launch(ctx, in, "rolled back to "+u.from, false)
	if err == nil {
		err = waitRun(ctx, r)
	}
	if err != nil {
		return m.mustStatus(name), fault.Wrap(fault.Failed, "update_failed", err, "%s, and build %s did not start again", msg, u.from)
	}
	return m.mustStatus(name), fault.New(fault.Failed, "update_failed", "%s", msg)
}
