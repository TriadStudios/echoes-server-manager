package instances

import (
	"slices"
	"strconv"
	"time"
)

// State is the life-cycle state of an instance.
type State string

// States.
const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Running  State = "running"
	Stopping State = "stopping"
	Crashed  State = "crashed"
	Failed   State = "failed" // gave up after crashes, never became ready, or a stop failed
	Updating State = "updating"
	// Deleted appears only in the last state event of a deleted instance.
	Deleted State = "deleted"
)

// active reports states in which a server process is (or should be) alive.
func (s State) active() bool {
	switch s {
	case Starting, Running, Stopping, Updating:
		return true
	}
	return false
}

// Ready sources, in the order they are checked.
const (
	ReadyStatusFile = "status_file"
	ReadyLogLine    = "log_line"
	ReadyQueryPort  = "query_port"
	// ReadyAdopted marks a server that was already running when ESM
	// started. It counts as running at once; the other sources are checked
	// again only to show which one confirms.
	ReadyAdopted = "adopted"
)

// Status is the live state of an instance. It is mirrored to
// instances/<name>/run/state.json and published as "state" events.
type Status struct {
	Name            string    `json:"name"`
	State           State     `json:"state"`
	Reason          string    `json:"reason"`
	Since           time.Time `json:"since"`
	Build           string    `json:"build"`
	PID             int       `json:"pid"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	ReadyAt         time.Time `json:"ready_at,omitzero"`
	ReadySource     string    `json:"ready_source"`
	ExitCode        *int      `json:"exit_code"` // last exit code when known
	Crashes         int       `json:"crashes"`   // crashes inside the back-off window
	GaveUp          bool      `json:"gave_up"`
	NextRestart     time.Time `json:"next_restart,omitzero"`
	RestartRequired bool      `json:"restart_required"`
	// UpdateUnverified says that an automatic update switched the build of
	// this instance while it was stopped, and the new build has not been
	// seen ready yet. The next start checks it: it starts the new build
	// and waits for ready, and when it does not get ready, puts back
	// UpdateRollbackBuild and starts that. Both are left out of the JSON
	// when there is no such update, so a state file without one stays
	// readable by an earlier release.
	UpdateUnverified    bool   `json:"update_unverified,omitempty"`
	UpdateRollbackBuild string `json:"update_rollback_build,omitempty"`
}

// BackoffDelay is the wait before the restart that follows crash number n
// (1-based) inside the window: base*2^(n-1), capped at max.
func BackoffDelay(base, max time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	return min(d, max)
}

// RecentCrashes keeps the crash times that are inside the window ending at
// now.
func RecentCrashes(times []time.Time, now time.Time, window time.Duration) []time.Time {
	return slices.DeleteFunc(slices.Clone(times), func(t time.Time) bool { return now.Sub(t) >= window })
}

// GiveUp reports whether the crashes inside the window reach maxCrashes.
func GiveUp(recent []time.Time, maxCrashes int) bool { return len(recent) >= maxCrashes }

// NextRestart returns the first daily restart time strictly after now, in
// now's location. times are "HH:MM" strings; it returns the zero time when
// there are none.
func NextRestart(now time.Time, times []string) time.Time {
	var best time.Time
	for _, t := range times {
		if len(t) != 5 {
			continue
		}
		h, err1 := strconv.Atoi(t[:2])
		m, err2 := strconv.Atoi(t[3:])
		if err1 != nil || err2 != nil {
			continue
		}
		c := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
		if !c.After(now) {
			c = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
		}
		if best.IsZero() || c.Before(best) {
			best = c
		}
	}
	return best
}
