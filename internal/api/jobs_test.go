package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// jobRig is an API with a self-update that waits until the test lets it go,
// and one token of each role.
type jobRig struct {
	*rig
	tok map[auth.Role]string
	ids map[auth.Role]string // the ids of the tokens

	mu    sync.Mutex
	calls int
	// seen is what each call of the self-update saw of its context when it
	// ended.
	seen    []error
	gate    chan struct{}
	started chan struct{}
	check   func(exitAnyway bool) error
}

func newJobRig(t *testing.T) *jobRig {
	t.Helper()
	jr := &jobRig{tok: map[auth.Role]string{}, ids: map[auth.Role]string{}, gate: make(chan struct{}), started: make(chan struct{}, 64)}
	jr.rig = newRig(t, Options{
		SelfUpdate: func(ctx context.Context, force, exitAnyway bool) (release.Result, bool, error) {
			jobs.Step(ctx, "download")
			jr.mu.Lock()
			jr.calls++
			jr.mu.Unlock()
			jr.started <- struct{}{}
			<-jr.gate
			jobs.Step(ctx, "restart")
			jr.mu.Lock()
			jr.seen = append(jr.seen, ctx.Err())
			jr.mu.Unlock()
			return release.Result{From: "v1.0.0", To: "v1.1.0", Binary: "/opt/esm/esm", Previous: "/opt/esm/esm.previous"}, true, nil
		},
		SelfUpdateCheck: func(exitAnyway bool) error {
			if jr.check != nil {
				return jr.check(exitAnyway)
			}
			return nil
		},
	})
	for _, r := range []auth.Role{auth.RoleRead, auth.RoleOperator, auth.RoleAdmin, auth.RoleSite} {
		plain, tok, err := jr.auth.CreateToken(string(r), r)
		if err != nil {
			t.Fatal(err)
		}
		jr.tok[r], jr.ids[r] = plain, tok.ID
	}
	t.Cleanup(func() {
		select {
		case <-jr.gate:
		default:
			close(jr.gate)
		}
	})
	return jr
}

// call sends a request with the token of role, and the further headers named
// in pairs.
func (jr *jobRig) call(role auth.Role, method, target, body string, more ...string) *httptest.ResponseRecorder {
	jr.t.Helper()
	h := bearer(jr.tok[role])
	h["Content-Type"] = "application/json"
	for i := 0; i+1 < len(more); i += 2 {
		h[more[i]] = more[i+1]
	}
	return jr.do(method, target, body, h)
}

func (jr *jobRig) job(id string) jobs.Job {
	jr.t.Helper()
	rec := jr.call(auth.RoleRead, "GET", "/api/v1/jobs/"+id, "")
	var j jobs.Job
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
		jr.t.Fatalf("GET job %s: %d %s", id, rec.Code, rec.Body)
	}
	return j
}

func (jr *jobRig) waitJob(id string) jobs.Job {
	jr.t.Helper()
	var j jobs.Job
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j = jr.job(id); j.State != jobs.StateRunning {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	jr.t.Fatalf("job %s still runs: %+v", id, j)
	return j
}

// A job that was started by a request goes on when the request's context
// ends (the client went away): the work gets a context of its own.
func TestAJobGoesOnWhenItsRequestEnds(t *testing.T) {
	jr := newJobRig(t)
	sub := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}}, 64)
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/box/self-update?async=true", strings.NewReader(`{"force":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+jr.tok[auth.RoleAdmin])
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	jr.s.Handler().ServeHTTP(rec, req)
	var j jobs.Job
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &j) != nil || rec.Header().Get("Location") != "/api/v1/jobs/"+j.ID {
		t.Fatalf("start: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if j.Kind != jobs.KindSelfUpdate || j.Target != "esm" || j.State != jobs.StateRunning || j.Actor.Role != "admin" || j.Actor.Kind != "token" {
		t.Fatalf("started job: %+v", j)
	}
	<-jr.started
	cancel() // the client is gone
	if got := jr.job(j.ID); got.State != jobs.StateRunning || got.Step != "download" {
		t.Fatalf("the job after its request ended: %+v", got)
	}
	close(jr.gate)
	done := jr.waitJob(j.ID)
	if done.State != jobs.StateDone || done.Step != "restart" {
		t.Fatalf("job: %+v %+v", done, done.Error)
	}
	var res SelfUpdateResult
	if b, _ := json.Marshal(done.Result); json.Unmarshal(b, &res) != nil || res.To != "v1.1.0" || !res.Restarting || res.Note == "" {
		t.Fatalf("result %s", b)
	}
	jr.mu.Lock()
	seen := append([]error(nil), jr.seen...)
	jr.mu.Unlock()
	if len(seen) != 1 || seen[0] != nil {
		t.Fatalf("the work saw its context end: %v", seen)
	}
	// The events: it started, went to its steps, ended.
	var states []string
	timeout := time.After(5 * time.Second)
	for len(states) < 4 {
		select {
		case e := <-sub.C:
			var ev jobs.Job
			if json.Unmarshal(e.Data, &ev) != nil || ev.ID != j.ID || e.Type != events.TypeJob || e.Instance != "" {
				t.Fatalf("event %+v", e)
			}
			states = append(states, ev.State+"/"+ev.Step)
		case <-timeout:
			t.Fatalf("events %v", states)
		}
	}
	if strings.Join(states, " ") != "running/ running/download running/restart done/restart" {
		t.Fatalf("events %v", states)
	}
	// The audit log has the start as accepted and the end.
	log := jr.auditLog()
	if !strings.Contains(log, `"action":"box.self_update"`) || !strings.Contains(log, `"action":"job.end"`) ||
		!strings.Contains(log, j.ID+" self_update esm") {
		t.Fatalf("audit log:\n%s", log)
	}
}

// What the call alone tells is answered as the call that waits answers it,
// and starts no job.
func TestAnAsyncCallIsCheckedAsTheCallIs(t *testing.T) {
	jr := newJobRig(t)
	jr.check = func(exitAnyway bool) error { return release.ErrBusy }
	rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async=true", `{}`)
	if rec.Code != http.StatusConflict || errorCode(rec) != "busy" || rec.Header().Get("Location") != "" {
		t.Fatalf("refused by the check: %d %s", rec.Code, rec.Body)
	}
	jr.check = func(bool) error { return errors.New("releases are not looked up") }
	if rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async=true", `{}`); rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "update_failed" {
		t.Fatalf("refused for another reason: %d %s", rec.Code, rec.Body)
	}
	jr.check = nil
	if rec := jr.call(auth.RoleSite, "POST", "/api/v1/box/self-update?async=true", `{"exit_anyway":true}`); rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" {
		t.Fatalf("exit_anyway as a site token: %d %s", rec.Code, rec.Body)
	}
	if rec := jr.call(auth.RoleOperator, "POST", "/api/v1/box/self-update?async=true", `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"async=1x", "async=yes", "async=2"} {
		if rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?"+q, `{}`); rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "invalid" {
			t.Errorf("?%s: %d %s", q, rec.Code, rec.Body)
		}
	}
	if rec := jr.call(auth.RoleRead, "GET", "/api/v1/jobs", ""); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"jobs":[]}` {
		t.Fatalf("no job was made: %d %s", rec.Code, rec.Body)
	}
	jr.mu.Lock()
	defer jr.mu.Unlock()
	if jr.calls != 0 {
		t.Fatalf("the update was started %d times", jr.calls)
	}
}

// Reading jobs is every reader's; cancelling needs the role that starts the
// job, and nothing but an install is cancellable.
func TestJobRoutesAndRoles(t *testing.T) {
	jr := newJobRig(t)
	rec := jr.call(auth.RoleSite, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`)
	var j jobs.Job
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	<-jr.started
	for role, want := range map[auth.Role]int{auth.RoleRead: 200, auth.RoleOperator: 200, auth.RoleAdmin: 200, auth.RoleSite: 200} {
		if rec := jr.call(role, "GET", "/api/v1/jobs/"+j.ID, ""); rec.Code != want {
			t.Errorf("%s reads a job: %d", role, rec.Code)
		}
		if rec := jr.call(role, "GET", "/api/v1/jobs?state=running&target=esm", ""); rec.Code != want || !strings.Contains(rec.Body.String(), j.ID) {
			t.Errorf("%s lists jobs: %d %s", role, rec.Code, rec.Body)
		}
	}
	if rec := jr.do("GET", "/api/v1/jobs", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("jobs with no login: %d", rec.Code)
	}
	if rec := jr.call(auth.RoleRead, "GET", "/api/v1/jobs?state=nope", ""); rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "invalid" {
		t.Fatalf("a state that is none: %d %s", rec.Code, rec.Body)
	}
	if rec := jr.call(auth.RoleRead, "GET", "/api/v1/jobs/none", ""); rec.Code != http.StatusNotFound || errorCode(rec) != "not_found" {
		t.Fatalf("a job that is not there: %d %s", rec.Code, rec.Body)
	}
	// Cancelling takes the role that starts the job: the read role has none, and
	// an operator does not start a self-update. Those who do are told that it
	// is not cancellable.
	for _, role := range []auth.Role{auth.RoleRead, auth.RoleOperator} {
		if rec := jr.call(role, "DELETE", "/api/v1/jobs/"+j.ID, ""); rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" {
			t.Errorf("%s cancels a self-update: %d %s", role, rec.Code, rec.Body)
		}
	}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleSite} {
		rec := jr.call(role, "DELETE", "/api/v1/jobs/"+j.ID, "")
		if rec.Code != http.StatusConflict || errorCode(rec) != "not_cancellable" || !strings.Contains(rec.Body.String(), "self_update") {
			t.Errorf("%s cancels a self-update: %d %s", role, rec.Code, rec.Body)
		}
	}
	if rec := jr.call(auth.RoleAdmin, "DELETE", "/api/v1/jobs/none", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cancel of a job that is not there: %d %s", rec.Code, rec.Body)
	}
	if got := jr.job(j.ID); got.State != jobs.StateRunning {
		t.Fatalf("the job after the refused cancels: %+v", got)
	}
	close(jr.gate)
	if got := jr.waitJob(j.ID); got.State != jobs.StateDone {
		t.Fatalf("job: %+v", got)
	}
	if rec := jr.call(auth.RoleAdmin, "DELETE", "/api/v1/jobs/"+j.ID, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "ended") {
		t.Fatalf("cancel of a job that ended: %d %s", rec.Code, rec.Body)
	}
}

// ESM does not take more jobs than it runs at once, and none once it ends.
func TestJobsAreBoundedAndEndWithShutdown(t *testing.T) {
	jr := newJobRig(t)
	for range jobs.DefaultMaxRunning {
		if rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`); rec.Code != http.StatusAccepted {
			t.Fatalf("start: %d %s", rec.Code, rec.Body)
		}
	}
	rec := jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`)
	if rec.Code != http.StatusTooManyRequests || errorCode(rec) != "busy" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("one job too many: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	close(jr.gate)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := jr.s.ShutdownJobs(ctx); err != nil {
		t.Fatalf("ShutdownJobs: %v", err)
	}
	if rec := jr.call(auth.RoleAdmin, "GET", "/api/v1/jobs?state=done", ""); rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"kind":"self_update"`) != jobs.DefaultMaxRunning {
		t.Fatalf("jobs after the shutdown: %d %s", rec.Code, rec.Body)
	}
	rec = jr.call(auth.RoleAdmin, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`)
	if rec.Code != http.StatusServiceUnavailable || errorCode(rec) != "shutting_down" {
		t.Fatalf("a job after the shutdown: %d %s", rec.Code, rec.Body)
	}
}

// The read role sees the steps and the times of a job on an instance with
// secrets on the command line, and not what its server said.
func TestRedactJob(t *testing.T) {
	in := jobs.Job{ID: "j", Kind: jobs.KindUpdate, Target: "risky", State: jobs.StateFailed, Step: "rollback",
		Steps:  []jobs.StepRecord{{Name: "backup"}},
		Result: instances.Status{Name: "risky", State: instances.Running, Reason: "ready, with -ServerPassword=hunter2"},
		Error:  &jobs.Error{Code: "update_failed", Message: "the server said -AdminPassword=hunter2"}}
	out := redactJob(in)
	st, ok := out.Result.(instances.Status)
	if !ok || st.Reason != instances.HiddenNotice || st.State != instances.Running || out.Error.Message != instances.HiddenNotice || out.Error.Code != "update_failed" ||
		len(out.Steps) != 1 || out.Step != "rollback" || out.ID != "j" {
		t.Fatalf("redacted: %+v %+v", out, out.Error)
	}
	if in.Error.Message == instances.HiddenNotice || in.Result.(instances.Status).Reason == instances.HiddenNotice {
		t.Fatal("redacting changed the job it was given")
	}
	// A result that is no status (a backup) and a job without an error pass.
	b := redactJob(jobs.Job{Result: instances.Backup{ID: "x"}})
	if _, ok := b.Result.(instances.Backup); !ok || b.Error != nil {
		t.Fatalf("redacted backup job: %+v", b)
	}
}

// installTargetKind keeps of the source of an install what the read role may
// see: its kind, the host of an address, a Steam source as it is.
func TestInstallTargetKind(t *testing.T) {
	for in, want := range map[string]string{
		"steam:main":                            "steam:main",
		" steam:main ":                          "steam:main",
		"/home/esm/imports/server-1.0":          "folder",
		"server-1.0":                            "folder",
		"imports/sub/server-1.0.zip":            "archive",
		"/home/esm/imports/server.TAR.GZ":       "archive",
		"build.tgz":                             "archive",
		"C:\\imports\\server":                   "folder",
		"STEAM:main":                            "folder",
		"https://downloads.example.com/a/b.zip": "https://downloads.example.com",
		"HTTPS://Example.com:8443/x/y.tgz":      "https://Example.com:8443",
		"http://10.0.0.5/x.zip":                 "http://10.0.0.5",
		"https:///nohost.zip":                   "url",
		"":                                      "folder",
	} {
		if got := installTargetKind(in); got != want {
			t.Errorf("installTargetKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// What a job carries of the call that started it is the operator role's and
// above: the read role gets the kind and role of the actor but not its id nor
// the person it acts for, the kind of source of an install, and no paths of a
// self-update. The operator, admin and site roles get all of it. The same
// goes for the events, live and from memory, and for the filter by target.
func TestTheReadRoleSeesNoAuditDataInAJob(t *testing.T) {
	jr := newJobRig(t)
	const who = "jane@example.com"
	live := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}, Public: true}, 64)
	defer live.Close()
	liveFull := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}}, 64)
	defer liveFull.Close()

	rec := jr.call(auth.RoleSite, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`, ActingUserHeader, who)
	var j jobs.Job
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &j) != nil || j.Actor.ID != jr.ids[auth.RoleSite] || j.Actor.ActingUser != who {
		t.Fatalf("start by the site role: %d %s", rec.Code, rec.Body)
	}
	<-jr.started
	close(jr.gate)
	jr.waitJob(j.ID)

	// Every role reads it; what each sees of it.
	for _, role := range []auth.Role{auth.RoleRead, auth.RoleOperator, auth.RoleAdmin, auth.RoleSite} {
		full := role != auth.RoleRead
		var one jobs.Job
		rec := jr.call(role, "GET", "/api/v1/jobs/"+j.ID, "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &one) != nil {
			t.Fatalf("%s reads the job: %d %s", role, rec.Code, rec.Body)
		}
		var list JobList
		rec = jr.call(role, "GET", "/api/v1/jobs", "")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Jobs) != 1 {
			t.Fatalf("%s lists the jobs: %d %s", role, rec.Code, rec.Body)
		}
		for where, got := range map[string]jobs.Job{"GET /jobs/{id}": one, "GET /jobs": list.Jobs[0]} {
			var res SelfUpdateResult
			if b, _ := json.Marshal(got.Result); json.Unmarshal(b, &res) != nil || res.To != "v1.1.0" || got.Actor.Kind != "token" || got.Actor.Role != "site" || got.Kind != jobs.KindSelfUpdate {
				t.Errorf("%s, %s: %+v", role, where, got)
				continue
			}
			wantID, wantUser, wantBin, wantPrev := "", "", "", ""
			if full {
				wantID, wantUser, wantBin, wantPrev = jr.ids[auth.RoleSite], who, "/opt/esm/esm", "/opt/esm/esm.previous"
			}
			if got.Actor.ID != wantID || got.Actor.ActingUser != wantUser || res.Binary != wantBin || res.Previous != wantPrev {
				t.Errorf("%s, %s: actor %+v, binary %q, previous %q", role, where, got.Actor, res.Binary, res.Previous)
			}
		}
	}
	// A reader cannot ask for a job by what it hides: the filter by target is
	// matched against the target as the caller sees it.
	if rec := jr.call(auth.RoleRead, "GET", "/api/v1/jobs?target=esm", ""); !strings.Contains(rec.Body.String(), j.ID) {
		t.Fatalf("the reader's list by target: %s", rec.Body)
	}

	// The events: live to a reader, and from memory, never have what a reader
	// may not see; the others' have it all.
	secrets := []string{jr.ids[auth.RoleSite], who, "/opt/esm"}
	timeout := time.After(10 * time.Second)
	var readerEvents, fullEvents int
	for readerEvents < 4 || fullEvents < 4 {
		select {
		case e := <-live.C:
			readerEvents++
			for _, s := range secrets {
				if strings.Contains(string(e.Data), s) {
					t.Errorf("a live job event for a reader has %q: %s", s, e.Data)
				}
			}
		case e := <-liveFull.C:
			fullEvents++
			for _, s := range secrets[:2] {
				if !strings.Contains(string(e.Data), s) {
					t.Errorf("a job event for the others lacks %q: %s", s, e.Data)
				}
			}
		case <-timeout:
			t.Fatalf("%d events for a reader, %d for the others", readerEvents, fullEvents)
		}
	}
	replay, _ := jr.bus.Since(0, events.Filter{Types: []string{events.TypeJob}, Public: true})
	replayFull, _ := jr.bus.Since(0, events.Filter{Types: []string{events.TypeJob}})
	if len(replay) < 4 || len(replay) != len(replayFull) {
		t.Fatalf("%d events for a reader in memory, %d for the others", len(replay), len(replayFull))
	}
	for i, e := range replay {
		for _, s := range secrets {
			if strings.Contains(string(e.Data), s) {
				t.Errorf("a remembered job event for a reader has %q: %s", s, e.Data)
			}
			if !strings.Contains(string(replayFull[i].Data), s) && s != "/opt/esm" {
				t.Errorf("a remembered job event for the others lacks %q: %s", s, replayFull[i].Data)
			}
		}
		if !strings.Contains(string(e.Data), `"kind":"token"`) || !strings.Contains(string(e.Data), `"role":"site"`) {
			t.Errorf("a job event for a reader lost the kind and role of the actor: %s", e.Data)
		}
	}
}

// The person that a site token says it acts for is a note that anyone could
// have written anything in: it is cut and scrubbed as the audit log does it,
// in the job as well.
func TestTheActingUserOfAJobIsScrubbed(t *testing.T) {
	jr := newJobRig(t)
	close(jr.gate)
	const secret = "esm_abcdefghijklmnopqrstuv"
	rec := jr.call(auth.RoleSite, "POST", "/api/v1/box/self-update?async=true", `{"force":true}`, ActingUserHeader, "jane "+secret+" -ServerPassword=hunter2")
	var j jobs.Job
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	done := jr.waitJob(j.ID)
	for what, body := range map[string]string{"the answer": rec.Body.String(), "the job": jr.call(auth.RoleAdmin, "GET", "/api/v1/jobs/"+j.ID, "").Body.String()} {
		if strings.Contains(body, secret) || strings.Contains(body, "hunter2") || !strings.Contains(body, "esm_********") {
			t.Errorf("%s has the acting user as written: %s", what, body)
		}
	}
	if strings.Contains(done.Actor.ActingUser, "hunter2") || strings.Contains(jr.auditLog(), "hunter2") || strings.Contains(jr.auditLog(), secret) {
		t.Errorf("acting user %q, audit log:\n%s", done.Actor.ActingUser, jr.auditLog())
	}
}
