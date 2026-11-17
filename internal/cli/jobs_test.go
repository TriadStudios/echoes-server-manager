package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
)

func TestDescribeJob(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	pct := 42.5
	actor := audit.Actor{Kind: "token", ID: "tok_1", Role: "site", ActingUser: "jane"}
	steps := []jobs.StepRecord{
		{Name: "backup", Started: t0, Ended: t0.Add(1500 * time.Millisecond)},
		{Name: "download", Started: t0.Add(1500 * time.Millisecond), Bytes: 1 << 20, Total: 2 << 20, Percent: &pct},
	}
	for _, c := range []struct {
		name  string
		job   jobs.Job
		steps bool
		want  []string
	}{
		{"running", jobs.Job{ID: "abc", Kind: "install", Target: "steam:main", State: jobs.StateRunning, Step: "download", Steps: steps, Started: t0, Actor: actor}, false,
			[]string{"abc install steam:main: running, step download 42.5% (1.0 MiB of 2.0 MiB), 1m30s", "started by token tok_1 (site) for jane"}},
		{"running, no step yet", jobs.Job{ID: "abc", Kind: "update", Target: "alpha", State: jobs.StateRunning, Started: t0}, false,
			[]string{"abc update alpha: running, 1m30s"}},
		{"being cancelled", jobs.Job{ID: "abc", Kind: "install", Target: "x", State: jobs.StateRunning, Step: "unpack", Cancelling: true, Started: t0}, false,
			[]string{"running, step unpack, being cancelled"}},
		{"done", jobs.Job{ID: "abc", Kind: "backup", Target: "alpha", State: jobs.StateDone, Started: t0, Ended: t0.Add(3200 * time.Millisecond),
			Step: "prune", Steps: []jobs.StepRecord{{Name: "copy", Started: t0, Ended: t0.Add(3 * time.Second)}, {Name: "prune", Started: t0.Add(3 * time.Second), Ended: t0.Add(3200 * time.Millisecond)}}}, true,
			[]string{"abc backup alpha: done in 3.2s", "steps: copy 3s, prune 200ms"}},
		{"failed", jobs.Job{ID: "abc", Kind: "update", Target: "alpha", State: jobs.StateFailed, Step: "rollback", Started: t0, Ended: t0.Add(2 * time.Minute),
			Error: &jobs.Error{Code: "update_failed", Message: "update failed, rolled back to 1.0"}}, false,
			[]string{"failed in 2m0s at step rollback: update failed, rolled back to 1.0 (update_failed)"}},
		{"cancelled", jobs.Job{ID: "abc", Kind: "install", Target: "x", State: jobs.StateCancelled, Step: "download", Started: t0, Ended: t0.Add(time.Second)}, false,
			[]string{"cancelled after 1s at step download"}},
		{"socket", jobs.Job{ID: "abc", Kind: "backup", Target: "a", State: jobs.StateDone, Started: t0, Ended: t0, Actor: audit.Actor{Kind: "socket", ID: "socket", Role: "admin"}}, false,
			[]string{"started by the socket (admin)"}},
	} {
		got := describeJob(c.job, t0.Add(90*time.Second), c.steps)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not have %q", c.name, got, w)
			}
		}
		if strings.Contains(got, "\n") {
			t.Errorf("%s: more than one line: %q", c.name, got)
		}
	}
}
