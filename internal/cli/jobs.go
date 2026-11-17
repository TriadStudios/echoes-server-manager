package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
)

// jobsCmd lists the jobs: the long calls that were started with
// async=true (by the web page, or by a website's panel); the commands of the
// command line always wait for their own.
func (e *env) jobsCmd(args []string) error {
	fs := e.flags("jobs")
	state := fs.String("state", "", "only jobs in this state: running, done, failed or cancelled")
	target := fs.String("target", "", "only jobs on this instance, build source or esm")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("jobs takes no arguments (esm job <id> shows one)")
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	q := url.Values{}
	if *state != "" {
		q.Set("state", *state)
	}
	if *target != "" {
		q.Set("target", *target)
	}
	path := "/api/v1/jobs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out api.JobList
	if err := c.do(e.ctx, "GET", path, nil, &out); err != nil {
		return err
	}
	if len(out.Jobs) == 0 {
		return e.result(out, "no jobs")
	}
	parts := make([]string, len(out.Jobs))
	for i, j := range out.Jobs {
		parts[i] = describeJob(j, time.Now(), false)
	}
	return e.result(out, plural(len(out.Jobs), "job", "jobs")+": "+strings.Join(parts, "; "))
}

// jobCmd shows one job.
func (e *env) jobCmd(args []string) error {
	fs := e.flags("job")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := want("job", pos, 1, "a job id (esm jobs lists them)"); err != nil {
		return err
	}
	c, err := e.client()
	if err != nil {
		return err
	}
	var j jobs.Job
	if err := c.do(e.ctx, "GET", "/api/v1/jobs/"+url.PathEscape(pos[0]), nil, &j); err != nil {
		return err
	}
	return e.result(j, describeJob(j, time.Now(), true))
}

// describeJob says a job in one line: what it is and how it stands. With
// steps it also lists the steps and how long each took. now is the time
// that a job that runs is measured against.
func describeJob(j jobs.Job, now time.Time, steps bool) string {
	s := fmt.Sprintf("%s %s %s: ", j.ID, j.Kind, j.Target)
	switch j.State {
	case jobs.StateRunning:
		s += "running"
		if j.Step != "" {
			s += ", step " + j.Step + stepProgress(j.Steps)
		}
		if j.Cancelling {
			s += ", being cancelled"
		}
		s += ", " + elapsed(now.Sub(j.Started))
	case jobs.StateDone:
		s += "done in " + elapsed(j.Ended.Sub(j.Started))
	case jobs.StateFailed:
		s += "failed in " + elapsed(j.Ended.Sub(j.Started)) + atStep(j)
		if j.Error != nil {
			s += ": " + j.Error.Message + " (" + j.Error.Code + ")"
		}
	case jobs.StateCancelled:
		s += "cancelled after " + elapsed(j.Ended.Sub(j.Started)) + atStep(j)
	default:
		s += j.State
	}
	if who := jobStarter(j); who != "" {
		s += "; started by " + who
	}
	if steps && len(j.Steps) > 0 {
		parts := make([]string, len(j.Steps))
		for i, st := range j.Steps {
			end := st.Ended
			if end.IsZero() {
				end = now
			}
			parts[i] = st.Name + " " + elapsed(end.Sub(st.Started))
		}
		s += "; steps: " + strings.Join(parts, ", ")
	}
	return s
}

// atStep says which step a job that ended was in.
func atStep(j jobs.Job) string {
	if j.Step == "" {
		return ""
	}
	return " at step " + j.Step
}

// jobStarter says who started a job: the kind and name of the caller, and
// the person a site token said it acted for.
func jobStarter(j jobs.Job) string {
	a := j.Actor
	if a.ID == "" {
		return ""
	}
	who := a.Kind + " " + a.ID
	if a.Kind == "socket" {
		who = "the socket"
	}
	if a.Role != "" {
		who += " (" + a.Role + ")"
	}
	if a.ActingUser != "" {
		who += " for " + a.ActingUser
	}
	return who
}

// stepProgress says how far the step that runs has come, when it says.
func stepProgress(steps []jobs.StepRecord) string {
	if len(steps) == 0 {
		return ""
	}
	st := steps[len(steps)-1]
	switch {
	case st.Total > 0 && st.Percent != nil:
		return fmt.Sprintf(" %.1f%% (%s of %s)", *st.Percent, byteSize(st.Bytes), byteSize(st.Total))
	case st.Percent != nil:
		return fmt.Sprintf(" %.1f%%", *st.Percent)
	case st.Bytes > 0:
		return " (" + byteSize(st.Bytes) + ")"
	}
	return ""
}

// elapsed writes a duration to a tenth of a second under a minute, and to
// the second above.
func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
