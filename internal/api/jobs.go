package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
)

// The long calls (update, backup, restore, installing a build, and ESM's
// own update) run synchronously inside their request, as ever. With
// ?async=true they are started as jobs (package jobs) instead: the call is
// checked as it always was, the job is started, and the answer is 202 with
// the job and its address in Location. The job has a context of its own, so
// the client may go away, and shows its step, so a panel can show how far it
// is; the job is read with GET /jobs/{id} or from the event stream (type
// job), and an install is stopped with DELETE /jobs/{id}.

// cancelWait is how long DELETE /jobs/{id} waits for the job to end after it
// asked it to; a job that has not ended by then is shown as it is, with
// cancelling set.
const cancelWait = 5 * time.Second

// JobList answers GET /jobs.
type JobList struct {
	Jobs []jobs.Job `json:"jobs"`
}

// asyncParam reads ?async: true starts a long call as a job.
func asyncParam(r *http.Request) (bool, error) { return queryBool(r, "async") }

// jobRunsOnInstance says that a job of this kind has an instance for its
// target.
func jobRunsOnInstance(kind string) bool {
	return kind == jobs.KindUpdate || kind == jobs.KindBackup || kind == jobs.KindRestore
}

// jobHidden says that the read role may not see the result and the error of
// the job: it ran on an instance whose server output and failure details are
// hidden from that role (see instances.HiddenNotice), or on one that is gone
// since, of which that is not known.
func (s *Server) jobHidden(j jobs.Job) bool {
	if !jobRunsOnInstance(j.Kind) || s.o.Instances == nil {
		return false
	}
	hidden, found := s.o.Instances.OutputHidden(j.Target)
	return hidden || !found
}

// redactJob is the job as the read role sees it when jobHidden: its steps
// and times, but a status with the notice in place of the reason, and the
// notice in place of the text of the error.
func redactJob(j jobs.Job) jobs.Job {
	if st, ok := j.Result.(instances.Status); ok {
		j.Result = st.ForRead()
	}
	if j.Error != nil {
		e := *j.Error
		e.Message = instances.HiddenNotice
		j.Error = &e
	}
	return j
}

// installTargetKind is the target of an install job as the read role sees
// it: what the audit log records (a folder or an archive in the import
// folder, an address) is for the operator role and above, and the read role
// gets the kind of source: "folder" or "archive" for a local one, the scheme
// and host of an address, and a Steam source as it is (its name is in
// GET /steam).
func installTargetKind(target string) string {
	t := strings.TrimSpace(target)
	if strings.HasPrefix(t, steam.SourcePrefix) {
		return t
	}
	if lower := strings.ToLower(t); strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
		if u, err := url.Parse(t); err == nil && u.Host != "" {
			return strings.ToLower(u.Scheme) + "://" + u.Host
		}
		return "url"
	}
	if builds.IsArchive(t) {
		return "archive"
	}
	return "folder"
}

// pathFreeErrors say, by the code of the error, what a failed install or
// self-update went wrong with, for the read role. The text of such an error
// is the one the call would have answered, and it may name a path of the
// box: the binary of ESM and its update lock, the import folder, SteamCMD,
// the file that an OS error names. The code stays.
var pathFreeErrors = map[string]string{
	"invalid":            "the request was not valid",
	"no_source":          "there is no such Steam source",
	"source_not_allowed": "the source is not one that ESM takes builds from",
	"hash_mismatch":      "a sha256 did not match what was expected",
	"too_large":          "the build is larger than ESM takes",
	"bad_manifest":       "the build has no valid manifest, or its files do not match it",
	"bad_archive":        "the archive could not be read",
	"unsafe_archive":     "the archive holds something that a build may not hold",
	"no_executable":      "the build has no server program for this system",
	"build_exists":       "this build is installed already",
	"download_failed":    "the download failed",
	"steamcmd_failed":    "SteamCMD failed",
	"steamcmd_missing":   "SteamCMD is not where ESM looks for it",
	"steam_manifest":     "SteamCMD finished, but its app manifest could not be read",
	"wrong_password":     "SteamCMD says the password of the branch is wrong",
	"not_enough_space":   "there is not enough free disk space for the build",
	"conflict":           "the request does not fit the state of ESM",
	"update_failed":      "the self-update failed",
	"up_to_date":         "this ESM is the newest release already",
	"unknown_version":    "this ESM is not a release version",
	"busy":               "an update is running, or ESM was updated already and ends in a moment",
	"no_service_manager": "nothing would start ESM again after the update ends it",
	"not_available":      "this ESM cannot update itself",
	"shutting_down":      "ESM was shutting down; the job was stopped",
}

// pathFreeError is the text for the read role of an error with this code: the
// one pathFreeErrors has, or, for a code it has not (an error of the system,
// code "internal", most of all), one that only says that the operator role is
// shown why.
func pathFreeError(code string) string {
	if msg, ok := pathFreeErrors[code]; ok {
		return msg
	}
	return "the job failed; the operator role is shown why"
}

// readView is the job as a caller without the operator role sees it. A job
// carries what the audit log records of the call that started it: who (the
// id of the login or token, and the person a site token acts for), the
// source of an install, and, of ESM's own update, the paths of the binary;
// and an install or a self-update that failed carries the text of its error,
// which may name a path. That is for the operator role and above; the read
// role gets the kind and role of the actor, the kind of source, no paths, and
// for those two kinds the code of the error with a text that names none.
func readView(j jobs.Job) jobs.Job {
	j.Actor.ID, j.Actor.ActingUser = "", ""
	if j.Kind == jobs.KindInstall {
		j.Target = installTargetKind(j.Target)
	}
	if r, ok := j.Result.(SelfUpdateResult); ok {
		r.Binary, r.Previous = "", ""
		j.Result = r
	}
	if j.Error != nil && (j.Kind == jobs.KindInstall || j.Kind == jobs.KindSelfUpdate) {
		e := *j.Error
		e.Message = pathFreeError(e.Code)
		j.Error = &e
	}
	return j
}

// showJob returns a job as the caller may see it.
func (s *Server) showJob(c *call, j jobs.Job) jobs.Job {
	if c.full() {
		return j
	}
	j = readView(j)
	if s.jobHidden(j) {
		j = redactJob(j)
	}
	return j
}

// publishJob puts a change of a job on the event stream. It is always a
// restricted event: the read role gets the job as readView shows it, and,
// when hidden says so, with the result and the error redacted too (see
// redactJob), also when it asks for remembered events later.
func (s *Server) publishJob(j jobs.Job, hidden bool) {
	if s.o.Bus == nil {
		return
	}
	instance := ""
	if jobRunsOnInstance(j.Kind) {
		instance = j.Target
	}
	public := readView(j)
	if hidden {
		public = redactJob(public)
	}
	s.o.Bus.PublishRestricted(events.TypeJob, instance, j, public)
}

// jobEnded writes the audit entry of a job that ended. The call that started
// it was recorded when it was made, as accepted; this is how it ended.
func (s *Server) jobEnded(j jobs.Job) {
	if s.o.Audit == nil {
		return
	}
	e := audit.Entry{Actor: j.Actor, Action: "job.end", Target: j.ID + " " + j.Kind + " " + j.Target, OK: j.State == jobs.StateDone}
	switch {
	case j.Error != nil:
		e.Error = j.Error.Message
	case j.State == jobs.StateCancelled:
		e.Error = "cancelled"
	}
	if err := s.o.Audit.Append(e); err != nil {
		s.log.Error("cannot write the audit log", "err", err)
	}
}

// classifyJob turns the error of a failed job into the code and message its
// call would have answered with.
func classifyJob(err error) (string, string) {
	_, code, msg := classify(err)
	return code, audit.Scrub(msg)
}

// ShutdownJobs stops the server taking jobs and waits for those that run, as
// the end of ESM waits for the requests that run: until ctx is done; what
// still runs then is asked to stop. Call it once the HTTP servers have
// stopped, before what the jobs use (instances, builds, Steam) is closed.
func (s *Server) ShutdownJobs(ctx context.Context) error { return s.jobs.Close(ctx) }

// startJob starts a job of the call and answers 202 with it. The call has
// been checked: work is what it does, as a job.
func (s *Server) startJob(w http.ResponseWriter, c *call, kind, target string, work jobs.Work) error {
	// What a caller wrote (the name a site token acts for) and the source of
	// an install are recorded as the audit log records them: cut and scrubbed.
	j, err := s.jobs.Start(jobs.Spec{Kind: kind, Target: audit.Clean(target), Actor: audit.CleanActor(c.actor)}, work)
	switch {
	case errors.Is(err, jobs.ErrClosed):
		return errStatus(http.StatusServiceUnavailable, "shutting_down", "ESM is shutting down; no job is started")
	case errors.Is(err, jobs.ErrBusy):
		w.Header().Set("Retry-After", "5")
		return errStatus(http.StatusTooManyRequests, "busy", "%d jobs are running; try again when one has ended", s.jobs.Running())
	case err != nil:
		return err
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	return writeJSON(w, http.StatusAccepted, s.showJob(c, j))
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, c *call) error {
	q := r.URL.Query()
	f := jobs.Filter{State: q.Get("state")}
	if f.State != "" && !jobs.ValidState(f.State) {
		return fault.Invalidf("state must be running, done, failed or cancelled")
	}
	// The target is matched against the target as the caller sees it, so
	// that the read role cannot ask whether a folder was installed.
	target := q.Get("target")
	out := JobList{Jobs: []jobs.Job{}}
	for _, j := range s.jobs.List(f) {
		if j = s.showJob(c, j); target == "" || j.Target == target {
			out.Jobs = append(out.Jobs, j)
		}
	}
	return writeJSON(w, http.StatusOK, out)
}

func notFoundJob(id string) error { return fault.NotFoundf("job %q not found", id) }

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, c *call) error {
	id := r.PathValue("id")
	j, ok := s.jobs.Get(id)
	if !ok {
		return notFoundJob(id)
	}
	return writeJSON(w, http.StatusOK, s.showJob(c, j))
}

// startRole is what a caller needs to start a job as it was started: the
// operator role for an update, a backup or a restore; for an install, the
// admin role, or the site role's too when the source is a Steam source; for
// ESM's own update, the same (the site role may, but not with exit_anyway,
// which the job does not show).
func startRole(j jobs.Job) (need auth.Role, siteToo bool) {
	switch j.Kind {
	case jobs.KindInstall:
		return auth.RoleAdmin, strings.HasPrefix(j.Target, steam.SourcePrefix)
	case jobs.KindSelfUpdate:
		return auth.RoleAdmin, true
	}
	return auth.RoleOperator, true
}

// cancelJob cancels a job: DELETE /jobs/{id}. Only an install can be
// cancelled, until it begins to put its build in place, and only by a caller
// who could have started it (an install from a folder, an archive or an
// address needs the admin role; one from a Steam source the site role's
// too). Anything else is 409 not_cancellable. The answer is the job as it is
// once it has stopped, or, when it is still stopping after a few seconds,
// as it is then (cancelling set).
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := decode(r, &struct{}{}); err != nil {
		return err
	}
	id := r.PathValue("id")
	c.target = id
	j, ok := s.jobs.Get(id)
	if !ok {
		return notFoundJob(id)
	}
	need, siteToo := startRole(j)
	if !c.role.Allows(need) && !(siteToo && c.role == auth.RoleSite) {
		return errStatus(http.StatusForbidden, "forbidden", "this job needs the %s role to be started, and to be cancelled; you have %s", need, c.role)
	}
	if _, err := s.jobs.Cancel(id); err != nil {
		var nc *jobs.NotCancellableError
		switch {
		case errors.As(err, &nc):
			return errStatus(http.StatusConflict, "not_cancellable", "%s", nc.Reason)
		case errors.Is(err, jobs.ErrNotFound):
			return notFoundJob(id)
		}
		return err
	}
	j, _ = s.jobs.Wait(id, cancelWait)
	return writeJSON(w, http.StatusOK, s.showJob(c, j))
}
