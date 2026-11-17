package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// --- helpers ---

// answer is a whole reply.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// raw sends a request over TCP with a token (and header pairs such as
// "X-ESM-Acting-User", "jane") and returns the whole reply.
func (h *harness) raw(method, path, token string, body any, header ...string) answer {
	h.t.Helper()
	req := newRequest(h.t, method, h.base+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header, b}
}

func (a answer) code() string {
	var eb api.ErrorBody
	json.Unmarshal(a.body, &eb)
	return eb.Error.Code
}

// startAsync calls path with async=true and expects 202 with a running job
// and its address in Location.
func (h *harness) startAsync(token, path string, body any, header ...string) jobs.Job {
	h.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	a := h.raw("POST", path+sep+"async=true", token, body, header...)
	var j jobs.Job
	if a.status != http.StatusAccepted || json.Unmarshal(a.body, &j) != nil || j.ID == "" {
		h.t.Fatalf("POST %s async: %d %s", path, a.status, a.body)
	}
	if loc := a.header.Get("Location"); loc != "/api/v1/jobs/"+j.ID {
		h.t.Fatalf("Location %q for job %s", loc, j.ID)
	}
	if j.State != jobs.StateRunning || !j.Ended.IsZero() {
		h.t.Fatalf("a job that was just started: %+v", j)
	}
	return j
}

func (h *harness) job(id, token string) jobs.Job {
	h.t.Helper()
	var j jobs.Job
	if st := h.call("GET", "/api/v1/jobs/"+id, token, nil, &j); st != http.StatusOK {
		h.t.Fatalf("GET job %s: %d", id, st)
	}
	return j
}

// jobEnds waits until the job is not running any more.
func (h *harness) jobEnds(id string) jobs.Job {
	h.t.Helper()
	return h.jobWhere(id, 60*time.Second, func(j jobs.Job) bool { return j.State != jobs.StateRunning })
}

// jobWhere waits until the job is such that ok says so.
func (h *harness) jobWhere(id string, timeout time.Duration, ok func(jobs.Job) bool) jobs.Job {
	h.t.Helper()
	var j jobs.Job
	testkit.Eventually(h.t, timeout, func() bool {
		j = h.job(id, h.admin)
		return ok(j)
	}, "job %s is not as awaited: %+v", id, &j)
	return j
}

func stepNames(j jobs.Job) []string {
	out := []string{}
	for _, s := range j.Steps {
		out = append(out, s.Name)
	}
	return out
}

// resultAs decodes the result of a job as v (what the call answers).
func resultAs(t *testing.T, j jobs.Job, v any) {
	t.Helper()
	b, _ := json.Marshal(j.Result)
	if j.Result == nil || json.Unmarshal(b, v) != nil {
		t.Fatalf("job result %s does not decode: %+v", b, j)
	}
}

func jobList(t *testing.T, h *harness, token, query string) []jobs.Job {
	t.Helper()
	var l api.JobList
	if st := h.call("GET", "/api/v1/jobs"+query, token, nil, &l); st != http.StatusOK {
		t.Fatalf("GET /jobs%s: %d", query, st)
	}
	return l.Jobs
}

// checkStepsAreWhole says that the steps of a finished job follow each
// other: each starts when the one before ended, and the last ends with the
// job.
func checkStepsAreWhole(t *testing.T, j jobs.Job) {
	t.Helper()
	if len(j.Steps) == 0 || j.Ended.IsZero() || j.Step != j.Steps[len(j.Steps)-1].Name {
		t.Fatalf("steps of a finished job: %+v", j)
	}
	for i, s := range j.Steps {
		if s.Started.IsZero() || s.Ended.IsZero() || s.Ended.Before(s.Started) {
			t.Errorf("step %d of job %s: %+v", i, j.ID, s)
		}
		if i > 0 && !s.Started.Equal(j.Steps[i-1].Ended) {
			t.Errorf("step %s starts at %s, the one before ended at %s", s.Name, s.Started, j.Steps[i-1].Ended)
		}
	}
	if last := j.Steps[len(j.Steps)-1]; !last.Ended.Equal(j.Ended) {
		t.Errorf("the last step ended %s, the job %s", last.Ended, j.Ended)
	}
}

// --- an update, a backup and a restore as jobs ---

// An update with async=true is a job with its steps; the call that is not
// async still waits and answers as ever (and makes no job). What the request
// alone tells is answered at once, the same for both.
func TestAsyncUpdateIsAJob(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.install(testkit.BuildOptions{Version: "1.1"})
	h.install(testkit.BuildOptions{Version: "1.2-broken", Fake: map[string]any{"FakeFailStart": true}})
	h.create("alpha", "1.0", fastRules(nil))
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	rd := h.token("read")

	j := h.startAsync(h.admin, "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.1"})
	if j.Kind != jobs.KindUpdate || j.Target != "alpha" || j.Actor.Kind != "token" || j.Actor.Role != "admin" || j.Cancellable {
		t.Fatalf("started job: %+v", j)
	}
	done := h.jobEnds(j.ID)
	if done.State != jobs.StateDone || done.Error != nil {
		t.Fatalf("the update job: %+v %+v", done, done.Error)
	}
	if got, want := stepNames(done), []string{"backup", "stop", "switch_build", "start", "wait_ready"}; !slices.Equal(got, want) {
		t.Fatalf("steps %v, want %v", got, want)
	}
	checkStepsAreWhole(t, done)
	var st instances.Status
	resultAs(t, done, &st)
	if st.State != instances.Running || st.Build != "1.1" {
		t.Fatalf("result of the update job: %+v", st)
	}
	if got := h.status("alpha"); got.Build != "1.1" || got.State != instances.Running {
		t.Fatalf("instance after the job: %+v", got)
	}

	// The reader looks at it, newest first, with filters.
	if l := jobList(t, h, rd, ""); len(l) != 1 || l[0].ID != j.ID || l[0].State != jobs.StateDone {
		t.Fatalf("jobs as a reader: %+v", l)
	}
	for q, n := range map[string]int{"?state=done": 1, "?state=running": 0, "?state=failed": 0, "?target=alpha": 1, "?target=beta": 0, "?state=done&target=alpha": 1, "?state=done&target=beta": 0} {
		if l := jobList(t, h, rd, q); len(l) != n {
			t.Errorf("GET /jobs%s: %d jobs, want %d", q, len(l), n)
		}
	}
	var eb api.ErrorBody
	if st := h.call("GET", "/api/v1/jobs?state=paused", rd, nil, &eb); st != http.StatusUnprocessableEntity || eb.Error.Code != "invalid" {
		t.Fatalf("a state that is none: %d %+v", st, eb)
	}
	if st := h.call("GET", "/api/v1/jobs/nope", rd, nil, &eb); st != http.StatusNotFound || eb.Error.Code != "not_found" {
		t.Fatalf("a job that is not there: %d %+v", st, eb)
	}

	// Without async nothing changes: it waits, answers 200 with the status,
	// and is no job.
	var sync instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.0"}, &sync)
	if sync.State != instances.Running || sync.Build != "1.0" {
		t.Fatalf("synchronous update: %+v", sync)
	}
	if l := jobList(t, h, rd, ""); len(l) != 1 {
		t.Fatalf("the synchronous update made a job: %+v", l)
	}
	if a := h.raw("POST", "/api/v1/instances/alpha/update?async=false", h.admin, api.UpdateRequest{To: "1.1"}); a.status != http.StatusOK || a.header.Get("Location") != "" {
		t.Fatalf("async=false: %d %s", a.status, a.body)
	}

	// A build that does not get ready: rolled back, and the job failed with
	// the error the call answers.
	syncBad := h.raw("POST", "/api/v1/instances/alpha/update", h.admin, api.UpdateRequest{To: "1.2-broken"})
	bad := h.startAsync(h.admin, "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.2-broken"})
	failed := h.jobEnds(bad.ID)
	if failed.State != jobs.StateFailed || failed.Error == nil || failed.Error.Code != "update_failed" || failed.Result != nil ||
		syncBad.status != http.StatusUnprocessableEntity || syncBad.code() != failed.Error.Code || !strings.Contains(failed.Error.Message, "rolled back to 1.1") {
		t.Fatalf("failed update job: %+v %+v (the call: %d %s)", failed, failed.Error, syncBad.status, syncBad.body)
	}
	if got, want := stepNames(failed), []string{"backup", "stop", "switch_build", "start", "wait_ready", "rollback"}; !slices.Equal(got, want) {
		t.Fatalf("steps of the failed update %v, want %v", got, want)
	}
	checkStepsAreWhole(t, failed)
	if got := h.status("alpha"); got.Build != "1.1" || got.State != instances.Running {
		t.Fatalf("instance after the failed job: %+v", got)
	}
	if l := jobList(t, h, rd, "?state=failed"); len(l) != 1 || l[0].ID != bad.ID {
		t.Fatalf("failed jobs: %+v", l)
	}
	if l := jobList(t, h, rd, ""); len(l) != 2 || l[0].ID != bad.ID || l[1].ID != j.ID {
		t.Fatalf("newest first: %+v", l)
	}

	// What the request tells is answered at once, as for the call that waits,
	// and starts no job.
	for _, c := range []struct {
		name, path string
		body       any
		status     int
		code       string
	}{
		{"an instance that is not there", "/api/v1/instances/ghost/update", api.UpdateRequest{To: "1.0"}, 404, "not_found"},
		{"no build named", "/api/v1/instances/alpha/update", api.UpdateRequest{}, 422, "invalid"},
		{"a build that is not installed", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "9.9"}, 422, "no_build"},
		{"the build it runs", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.1"}, 409, "same_build"},
		{"a body that is no JSON", "/api/v1/instances/alpha/update", "{", 400, "bad_request"},
	} {
		for _, async := range []bool{false, true} {
			path := c.path
			if async {
				path += "?async=true"
			}
			var a answer
			if s, ok := c.body.(string); ok {
				a = h.rawBody("POST", path, h.admin, s)
			} else {
				a = h.raw("POST", path, h.admin, c.body)
			}
			if a.status != c.status || a.code() != c.code || a.header.Get("Location") != "" {
				t.Errorf("%s (async %v): %d %s, want %d %s", c.name, async, a.status, a.body, c.status, c.code)
			}
		}
	}
	if a := h.raw("POST", "/api/v1/instances/alpha/update?async=maybe", h.admin, api.UpdateRequest{To: "1.0"}); a.status != http.StatusUnprocessableEntity || a.code() != "invalid" {
		t.Fatalf("async=maybe: %d %s", a.status, a.body)
	}
	if l := jobList(t, h, rd, ""); len(l) != 2 {
		t.Fatalf("the refused calls made jobs: %+v", l)
	}
	if got := h.status("alpha"); got.Build != "1.1" {
		t.Fatalf("a refused call changed the instance: %+v", got)
	}

	// The roles to start it are the call's: a reader may not.
	if a := h.raw("POST", "/api/v1/instances/alpha/update?async=true", rd, api.UpdateRequest{To: "1.0"}); a.status != http.StatusForbidden {
		t.Fatalf("a reader starts an update: %d %s", a.status, a.body)
	}
	// The audit log has the start, as accepted, and the end of the job.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	var started, ended, endedBad bool
	for _, e := range al.Entries {
		switch {
		case e.Action == "instance.update" && e.Target == "alpha to 1.1" && e.OK:
			started = true
		case e.Action == "job.end" && e.Target == j.ID+" update alpha" && e.OK:
			ended = true
		case e.Action == "job.end" && e.Target == bad.ID+" update alpha" && !e.OK && strings.Contains(e.Error, "rolled back"):
			endedBad = true
		}
	}
	if !started || !ended || !endedBad {
		t.Fatalf("audit entries: start %v, end %v, failed end %v: %+v", started, ended, endedBad, al.Entries)
	}
}

// rawBody sends a body that is not JSON-encoded by the helper.
func (h *harness) rawBody(method, path, token, body string) answer {
	h.t.Helper()
	req, err := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{resp.StatusCode, resp.Header, b}
}

// A backup and a restore as jobs, of a running server and of a stopped one.
func TestAsyncBackupAndRestoreAreJobs(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	testkit.Eventually(t, 10*time.Second, func() bool { return h.liveCounter("alpha", "alpha") > 0 }, "the fake never saved")

	// A running server: the backup waits for its next world save.
	j := h.startAsync(h.admin, "/api/v1/instances/alpha/backup", nil)
	if j.Kind != jobs.KindBackup || j.Target != "alpha" {
		t.Fatalf("started: %+v", j)
	}
	done := h.jobEnds(j.ID)
	if done.State != jobs.StateDone || !slices.Equal(stepNames(done), []string{"wait_save", "copy", "prune"}) {
		t.Fatalf("backup job: %+v %+v", done, done.Error)
	}
	checkStepsAreWhole(t, done)
	var b instances.Backup
	resultAs(t, done, &b)
	if b.Reason != instances.ReasonManual || !b.SaveConfirmed || b.Instance != "alpha" || !strings.HasPrefix(b.ID, "2") {
		t.Fatalf("result: %+v", b)
	}
	var bl api.BackupList
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/backups", nil, &bl)
	if len(bl.Backups) != 1 || bl.Backups[0].ID != b.ID {
		t.Fatalf("backups: %+v", bl.Backups)
	}
	// The call that waits answers 201 as ever.
	var sync instances.Backup
	h.must(http.StatusCreated, "POST", "/api/v1/instances/alpha/backup", struct{}{}, &sync)
	if sync.Reason != instances.ReasonManual || len(jobList(t, h, h.admin, "")) != 1 {
		t.Fatalf("synchronous backup: %+v", sync)
	}

	// A restore of a running server: stopped, kept, put in place, started.
	r := h.startAsync(h.admin, "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: b.ID})
	rdone := h.jobEnds(r.ID)
	if rdone.State != jobs.StateDone || !slices.Equal(stepNames(rdone), []string{"stop", "backup_current", "put_in_place", "start", "wait_ready"}) {
		t.Fatalf("restore job: %+v %+v", rdone, rdone.Error)
	}
	checkStepsAreWhole(t, rdone)
	var st instances.Status
	resultAs(t, rdone, &st)
	if st.State != instances.Running {
		t.Fatalf("result: %+v", st)
	}
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/backups", nil, &bl)
	pre := 0
	for _, x := range bl.Backups {
		if x.Reason == instances.ReasonPreRestore {
			pre++
		}
	}
	if pre != 1 {
		t.Fatalf("pre-restore backups: %+v", bl.Backups)
	}

	// A stopped server has no save to wait for and is not started.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)
	j2 := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/alpha/backup", nil).ID)
	if j2.State != jobs.StateDone || !slices.Equal(stepNames(j2), []string{"copy", "prune"}) {
		t.Fatalf("backup job of a stopped server: %+v %+v", j2, j2.Error)
	}
	r2 := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: b.ID}).ID)
	if r2.State != jobs.StateDone || !slices.Equal(stepNames(r2), []string{"backup_current", "put_in_place"}) {
		t.Fatalf("restore job of a stopped server: %+v %+v", r2, r2.Error)
	}
	var st2 instances.Status
	resultAs(t, r2, &st2)
	if st2.State != instances.Stopped {
		t.Fatalf("result: %+v", st2)
	}

	// What the request tells is answered at once.
	n := len(jobList(t, h, h.admin, ""))
	for _, c := range []struct {
		name, path string
		body       any
		status     int
		code       string
	}{
		{"backup of an instance that is not there", "/api/v1/instances/ghost/backup", nil, 404, "not_found"},
		{"restore on an instance that is not there", "/api/v1/instances/ghost/restore", api.RestoreRequest{Backup: b.ID}, 404, "not_found"},
		{"restore of a backup that is not there", "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: "20200101T000000Z-manual"}, 404, "not_found"},
		{"restore of an id that is none", "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: "../x"}, 404, "not_found"},
		{"restore with no id", "/api/v1/instances/alpha/restore", api.RestoreRequest{}, 422, "invalid"},
	} {
		for _, async := range []bool{false, true} {
			path := c.path
			if async {
				path += "?async=true"
			}
			if a := h.raw("POST", path, h.admin, c.body); a.status != c.status || a.code() != c.code {
				t.Errorf("%s (async %v): %d %s, want %d %s", c.name, async, a.status, a.body, c.status, c.code)
			}
		}
	}
	if got := len(jobList(t, h, h.admin, "")); got != n {
		t.Fatalf("refused calls made %d jobs", got-n)
	}
}

// --- installing a build as a job ---

// The install of a folder, an archive and an address as a job, its steps,
// and what is refused at once.
func TestAsyncInstallOfAFolder(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "2.0"})

	j := h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: dir})
	if j.Kind != jobs.KindInstall || j.Target != dir {
		t.Fatalf("started: %+v", j)
	}
	done := h.jobEnds(j.ID)
	if done.State != jobs.StateDone || !slices.Equal(stepNames(done), []string{"unpack", "verify", "place"}) || done.Cancellable {
		t.Fatalf("install job: %+v %+v", done, done.Error)
	}
	checkStepsAreWhole(t, done)
	var b api.BuildView
	resultAs(t, done, &b)
	if b.Version != "2.0" || b.UsedBy == nil {
		t.Fatalf("result: %+v", b)
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 1 || bl.Builds[0].Version != "2.0" {
		t.Fatalf("builds: %+v", bl)
	}

	// The same again: the install fails as the call would, 409.
	again := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: dir}).ID)
	sync := h.raw("POST", "/api/v1/builds", h.admin, api.InstallRequest{Source: dir})
	if again.State != jobs.StateFailed || again.Error == nil || again.Error.Code != "build_exists" || sync.status != http.StatusConflict || sync.code() != "build_exists" {
		t.Fatalf("a build that is installed already: %+v %+v; the call: %d %s", again, again.Error, sync.status, sync.body)
	}

	// An archive with its sha256, from the import folder.
	dir3 := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "3.0"})
	archive := filepath.Join(h.src, "three.tar.gz")
	testkit.MakeTarGz(t, dir3, archive)
	raw, _ := os.ReadFile(archive)
	sum := sha256.Sum256(raw)
	j3 := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: "three.tar.gz", SHA256: hex.EncodeToString(sum[:])}).ID)
	if j3.State != jobs.StateDone || !slices.Equal(stepNames(j3), []string{"unpack", "verify", "place"}) {
		t.Fatalf("archive install job: %+v %+v", j3, j3.Error)
	}
	// A wrong sha256 of a local archive needs the file: the job fails.
	wrong := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: "three.tar.gz", SHA256: strings.Repeat("a", 64)}).ID)
	if wrong.State != jobs.StateFailed || wrong.Error == nil || wrong.Error.Code != "hash_mismatch" {
		t.Fatalf("wrong sha256: %+v %+v", wrong, wrong.Error)
	}

	// What the request tells is refused at once, as for the call that waits.
	n := len(jobList(t, h, h.admin, ""))
	for _, c := range []struct {
		name string
		req  api.InstallRequest
		code string
	}{
		{"no source", api.InstallRequest{}, "invalid"},
		{"a sha256 that is none", api.InstallRequest{Source: "three.tar.gz", SHA256: "xyz"}, "invalid"},
		{"plain http", api.InstallRequest{Source: "http://example.invalid/b.tar.gz", SHA256: strings.Repeat("0", 64)}, "invalid"},
		{"an address with no sha256", api.InstallRequest{Source: "https://example.invalid/b.tar.gz"}, "invalid"},
		{"an address of no archive", api.InstallRequest{Source: "https://example.invalid/b.exe", SHA256: strings.Repeat("0", 64)}, "invalid"},
		{"a place outside the import folder", api.InstallRequest{Source: "/etc"}, "source_not_allowed"},
		{"a folder that is not there", api.InstallRequest{Source: "nothing-here"}, "invalid"},
		{"a sha256 for a folder", api.InstallRequest{Source: "2.0", SHA256: strings.Repeat("0", 64)}, "invalid"},
		{"a file that is no archive", api.InstallRequest{Source: "2.0/manifest.json"}, "invalid"},
		{"a Steam source that is not there", api.InstallRequest{Source: "steam:nope"}, "no_source"},
		{"a sha256 for a Steam source", api.InstallRequest{Source: "steam:nope", SHA256: strings.Repeat("0", 64)}, "invalid"},
	} {
		var codes []string
		for _, async := range []bool{false, true} {
			path := "/api/v1/builds"
			if async {
				path += "?async=true"
			}
			a := h.raw("POST", path, h.admin, c.req)
			codes = append(codes, strconv.Itoa(a.status)+" "+a.code())
			if a.status < 400 || a.code() != c.code || a.header.Get("Location") != "" {
				t.Errorf("%s (async %v): %d %s, want %s", c.name, async, a.status, a.body, c.code)
			}
		}
		if codes[0] != codes[1] {
			t.Errorf("%s: the call that waits answers %q, the job's call %q", c.name, codes[0], codes[1])
		}
	}
	// A build runs with ESM's own rights, so only an admin starts the install of
	// one, as a job too: the others are refused before anything is looked at.
	for name, tok := range map[string]string{"operator": h.token("operator"), "read": h.token("read")} {
		a := h.raw("POST", "/api/v1/builds?async=true", tok, api.InstallRequest{Source: filepath.Join(h.src, "2.0")})
		if a.status != http.StatusForbidden || a.code() != "forbidden" || a.header.Get("Location") != "" {
			t.Errorf("%s token starts an install as a job: %d %s", name, a.status, a.body)
		}
	}
	if got := len(jobList(t, h, h.admin, "")); got != n {
		t.Fatalf("refused calls made %d jobs", got-n)
	}
}

// slowArchive serves a build archive over https on loopback, half of it at
// once and the rest when the test lets it: an install of it is in its
// download for as long as the test wants. It returns what Install is given
// (the address and the sha256), and the function that lets the rest go.
func slowArchive(t *testing.T, version string) (source, sum string, size int, client *http.Client, release func()) {
	t.Helper()
	dir := testkit.MakeBuild(t, t.TempDir(), testkit.BuildOptions{Version: version})
	archive := filepath.Join(t.TempDir(), "build.tar.gz")
	testkit.MakeTarGz(t, dir, archive)
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		half := len(data) / 2
		w.Write(data[:half])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		w.Write(data[half:])
	}))
	t.Cleanup(func() { release(); srv.Close() })
	s := sha256.Sum256(data)
	return srv.URL + "/build.tar.gz", hex.EncodeToString(s[:]), len(data), srv.Client(), release
}

// An install that downloads shows the bytes so far, can be cancelled while
// it downloads by the role that started it and by no other, and puts
// nothing in place when it is; the same install that is let go ends done.
func TestAnInstallThatDownloadsIsCancelledByItsOwnRole(t *testing.T) {
	t.Parallel()
	source, sum, size, client, release := slowArchive(t, "4.0")
	h := newESMTuned(t, "process", func(o *service.Options) { o.HTTPClient = client })
	op, rd, site := h.token("operator"), h.token("read"), h.token("site")

	j := h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: source, SHA256: sum}, api.ActingUserHeader, "")
	if j.Target != source {
		t.Fatalf("target %q", j.Target)
	}
	// While it downloads: the bytes come in, and it can be cancelled.
	cur := h.jobWhere(j.ID, 20*time.Second, func(j jobs.Job) bool {
		return j.Step == "download" && len(j.Steps) == 1 && j.Steps[0].Bytes > 0
	})
	s := cur.Steps[0]
	if s.Total != int64(size) || s.Bytes <= 0 || s.Bytes >= s.Total || s.Percent == nil || *s.Percent <= 0 || *s.Percent >= 100 || !cur.Cancellable {
		t.Fatalf("a download under way: %+v (size %d) cancellable %v", s, size, cur.Cancellable)
	}
	// The client that started it goes away: the job does not.
	h.http.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)
	if cur = h.job(j.ID, h.admin); cur.State != jobs.StateRunning {
		t.Fatalf("the job after the connection closed: %+v", cur)
	}

	// Nobody but the role that starts it cancels it: an install from an
	// address is the admin's, not the operator's, the site's or a reader's.
	var eb api.ErrorBody
	for name, tok := range map[string]string{"read": rd, "operator": op, "site": site} {
		eb = api.ErrorBody{}
		if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, tok, nil, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
			t.Errorf("%s token cancels an install from an address: %d %+v", name, st, eb)
		}
	}
	if cur = h.job(j.ID, rd); cur.State != jobs.StateRunning || cur.Cancelling {
		t.Fatalf("the job after the refused cancels: %+v", cur)
	}
	// An admin does.
	var cancelled jobs.Job
	if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, h.admin, nil, &cancelled); st != http.StatusOK ||
		cancelled.State != jobs.StateCancelled || cancelled.Error != nil || cancelled.Result != nil || cancelled.Cancellable || cancelled.Ended.IsZero() {
		t.Fatalf("cancel: %d %+v", st, cancelled)
	}
	checkStepsAreWhole(t, cancelled)
	if !slices.Equal(stepNames(cancelled), []string{"download"}) {
		t.Fatalf("steps of the cancelled install: %v", stepNames(cancelled))
	}
	// Nothing was placed, and nothing is left of the work.
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 0 {
		t.Fatalf("builds after the cancel: %+v", bl)
	}
	testkit.Eventually(t, 5*time.Second, func() bool {
		left, _ := filepath.Glob(filepath.Join(h.root, "tmp", "build-*"))
		return len(left) == 0
	}, "the work folder of the cancelled install is still there")
	// The job stays in the list as cancelled, and cannot be cancelled again.
	if l := jobList(t, h, rd, "?state=cancelled"); len(l) != 1 || l[0].ID != j.ID {
		t.Fatalf("cancelled jobs: %+v", l)
	}
	if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, h.admin, nil, &eb); st != http.StatusConflict || eb.Error.Code != "not_cancellable" {
		t.Fatalf("a second cancel: %d %+v", st, eb)
	}
	if st := h.call("DELETE", "/api/v1/jobs/nope", h.admin, nil, &eb); st != http.StatusNotFound || eb.Error.Code != "not_found" {
		t.Fatalf("a job that is not there: %d %+v", st, eb)
	}
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	var cancelEntry, endEntry bool
	for _, e := range al.Entries {
		cancelEntry = cancelEntry || e.Action == "job.cancel" && e.Target == j.ID && e.OK
		endEntry = endEntry || e.Action == "job.end" && strings.HasPrefix(e.Target, j.ID+" install ") && !e.OK && e.Error == "cancelled"
	}
	if !cancelEntry || !endEntry {
		t.Fatalf("audit: cancel %v, end %v: %+v", cancelEntry, endEntry, al.Entries)
	}

	// The same install, let go, runs through: download, unpack, verify, place.
	release()
	j2 := h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: source, SHA256: sum})
	done := h.jobEnds(j2.ID)
	if done.State != jobs.StateDone || !slices.Equal(stepNames(done), []string{"download", "unpack", "verify", "place"}) {
		t.Fatalf("install job: %+v %+v", done, done.Error)
	}
	checkStepsAreWhole(t, done)
	if d := done.Steps[0]; d.Bytes != int64(size) || d.Total != int64(size) || d.Percent == nil || *d.Percent != 100 {
		t.Fatalf("the download step: %+v", d)
	}
	var b api.BuildView
	resultAs(t, done, &b)
	if b.Version != "4.0" {
		t.Fatalf("result: %+v", b)
	}
	// A job that is done is not cancellable, by the role that started it
	// either.
	if st := h.call("DELETE", "/api/v1/jobs/"+j2.ID, h.admin, nil, &eb); st != http.StatusConflict || eb.Error.Code != "not_cancellable" {
		t.Fatalf("cancel of an install that is done: %d %+v", st, eb)
	}
}

// steamJobRig is a service with a Steam source and a fake SteamCMD that
// can be made to take its time.
type steamJobRig struct {
	*harness
	dir string
	ctl testkit.SteamControl
}

func newSteamJobRig(t *testing.T, slowMS int, tune func(*service.Options)) *steamJobRig {
	t.Helper()
	steamDir := t.TempDir()
	content := filepath.Join(t.TempDir(), "content")
	exes := testkit.MakeContent(t, content, testkit.BuildOptions{Version: "x"})
	ctl := testkit.SteamControl{AppID: "1000001", BuildID: "100", Content: content, Branch: "testing", SlowMS: slowMS}
	exe := testkit.FakeSteamCMD(t, steamDir, ctl)
	h := newESMTuned(t, "process", tune, func(cfg map[string]any) {
		cfg["steamcmd"] = exe
		cfg["steam_sources"] = []map[string]any{{"name": "main", "app_id": "1000001", "branch": "testing", "executable": exes,
			"default_map": testkit.DefaultMap, "channel": "live", "check_every": "0"}}
	})
	return &steamJobRig{harness: h, dir: steamDir, ctl: ctl}
}

// A Steam install as a job shows the progress SteamCMD tells; the site role,
// which may start it, cancels it, and the person it acted for is in the
// job and in the audit log.
func TestASteamInstallJobShowsItsProgressAndTheSiteRoleCancelsIt(t *testing.T) {
	t.Parallel()
	r := newSteamJobRig(t, 2500, nil)
	h := r.harness
	site, op, rd := h.token("site"), h.token("operator"), h.token("read")

	j := h.startAsync(site, "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, api.ActingUserHeader, "jane")
	if j.Kind != jobs.KindInstall || j.Target != "steam:main" || j.Actor.Role != "site" || j.Actor.ActingUser != "jane" || j.Actor.Kind != "token" {
		t.Fatalf("started: %+v", j)
	}
	cur := h.jobWhere(j.ID, 20*time.Second, func(j jobs.Job) bool {
		return j.Step == "download" && len(j.Steps) == 1 && j.Steps[0].Bytes > 0
	})
	if s := cur.Steps[0]; s.Bytes != 1 || s.Total != 2 || s.Percent == nil || *s.Percent != 50 || !cur.Cancellable {
		t.Fatalf("what SteamCMD said: %+v, cancellable %v", s, cur.Cancellable)
	}
	// The operator and a reader may not cancel it; the site role may.
	var eb api.ErrorBody
	for name, tok := range map[string]string{"read": rd, "operator": op} {
		if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, tok, nil, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
			t.Errorf("%s token cancels an install from Steam: %d %+v", name, st, eb)
		}
	}
	var cancelled jobs.Job
	if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, site, nil, &cancelled); st != http.StatusOK || cancelled.State != jobs.StateCancelled {
		t.Fatalf("cancel by the site role: %d %+v", st, cancelled)
	}
	var sl api.SteamList
	h.must(http.StatusOK, "GET", "/api/v1/steam", nil, &sl)
	if len(sl.Sources) != 1 || sl.Sources[0].Running {
		t.Fatalf("SteamCMD still runs: %+v", sl)
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 0 {
		t.Fatalf("builds after the cancel: %+v", bl)
	}
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=200", nil, &al)
	var cancelEntry, endEntry bool
	for _, e := range al.Entries {
		cancelEntry = cancelEntry || e.Action == "job.cancel" && e.Target == j.ID && e.Actor.Role == "site" && e.OK
		endEntry = endEntry || e.Action == "job.end" && strings.HasPrefix(e.Target, j.ID+" install steam:main") && e.Actor.Role == "site" && e.Actor.ActingUser == "jane" && !e.OK
	}
	if !endEntry || !cancelEntry {
		t.Fatalf("audit (the end is of the person the job was started for): end %v, cancel %v: %+v", endEntry, cancelEntry, al.Entries)
	}

	// Without the pause it runs through, and an install of the same build
	// id again is a job that only downloads and finds the build there.
	r.ctl.SlowMS = 0
	testkit.SetSteamControl(t, r.dir, r.ctl)
	done := h.jobEnds(h.startAsync(site, "/api/v1/builds", api.InstallRequest{Source: "steam:main"}).ID)
	if done.State != jobs.StateDone || !slices.Equal(stepNames(done), []string{"download", "unpack", "verify", "place"}) {
		t.Fatalf("steam install job: %+v %+v", done, done.Error)
	}
	checkStepsAreWhole(t, done)
	var b api.BuildView
	resultAs(t, done, &b)
	if b.Version != "steam-main-100" {
		t.Fatalf("result: %+v", b)
	}
	again := h.jobEnds(h.startAsync(site, "/api/v1/builds", api.InstallRequest{Source: "steam:main"}).ID)
	if again.State != jobs.StateDone || !slices.Equal(stepNames(again), []string{"download"}) {
		t.Fatalf("steam install job of a build that is there: %+v %+v", again, again.Error)
	}
	// A site token may start no install from a folder, as a job either; and
	// cancels no job that is not an install.
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "7.7"})
	if a := h.raw("POST", "/api/v1/builds?async=true", site, api.InstallRequest{Source: dir}); a.status != http.StatusForbidden || a.code() != "forbidden" || a.header.Get("Location") != "" {
		t.Fatalf("site token starts an install from a folder as a job: %d %s", a.status, a.body)
	}
	if st := h.call("DELETE", "/api/v1/jobs/"+done.ID, site, nil, &eb); st != http.StatusConflict || eb.Error.Code != "not_cancellable" {
		t.Fatalf("cancel of an install that is done: %d %+v", st, eb)
	}
}

// Only an install can be cancelled: an update, a backup and a restore are
// refused with 409, by the role that started them and by the others, and run
// to their end.
func TestOnlyAnInstallIsCancellable(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.install(testkit.BuildOptions{Version: "1.1"})
	h.create("alpha", "1.0", fastRules(nil))
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	op, rd, site := h.token("operator"), h.token("read"), h.token("site")
	var eb api.ErrorBody

	j := h.startAsync(op, "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.1"})
	// While it runs: the roles that may start it are told it is not
	// cancellable; a reader is refused for lack of the role.
	for name, tok := range map[string]string{"operator": op, "admin": h.admin, "site": site} {
		if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, tok, nil, &eb); st != http.StatusConflict || eb.Error.Code != "not_cancellable" {
			t.Errorf("%s token cancels an update: %d %+v", name, st, eb)
		}
	}
	if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, rd, nil, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
		t.Errorf("read token cancels an update: %d %+v", st, eb)
	}
	done := h.jobEnds(j.ID)
	if done.State != jobs.StateDone {
		t.Fatalf("the update that was not cancelled: %+v %+v", done, done.Error)
	}
	if st := h.call("DELETE", "/api/v1/jobs/"+j.ID, h.admin, nil, &eb); st != http.StatusConflict || eb.Error.Code != "not_cancellable" {
		t.Fatalf("cancel of a job that is done: %d %+v", st, eb)
	}
	// A body of the wrong shape is refused as in every call.
	if a := h.rawBody("DELETE", "/api/v1/jobs/"+j.ID, h.admin, `{"x":1}`); a.status != http.StatusBadRequest {
		t.Fatalf("cancel with a body: %d %s", a.status, a.body)
	}
}

// --- events ---

// Every change of a job is an event of type job, for readers, with the
// instance of an update, backup or restore; the audit events stay the
// admin's.
func TestJobEventsAreSentToReaders(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	h.create("beta", "1.0", fastRules(nil))
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	rd := h.token("read")
	readers := h.recordEventsAs(rd, "")
	only := h.openEvents("?types=job&instance=beta", rd, "")
	admins := h.recordEvents()

	j := h.startAsync(h.admin, "/api/v1/instances/alpha/backup", nil)
	done := h.jobEnds(j.ID)
	testkit.Eventually(t, 10*time.Second, func() bool {
		for _, e := range readers.all() {
			var ev jobs.Job
			if e.Type == events.TypeJob && json.Unmarshal(e.Data, &ev) == nil && ev.ID == j.ID && ev.State == jobs.StateDone {
				return true
			}
		}
		return false
	}, "no event of the job ending")

	var seen []jobs.Job
	var last uint64
	for _, e := range readers.all() {
		if e.Type == events.TypeAudit {
			t.Errorf("an audit event for a reader: %+v", e)
		}
		if e.Type != events.TypeJob {
			continue
		}
		var ev jobs.Job
		if err := json.Unmarshal(e.Data, &ev); err != nil || ev.ID != j.ID {
			continue
		}
		if e.Instance != "alpha" || e.ID <= last {
			t.Errorf("job event: instance %q, id %d after %d", e.Instance, e.ID, last)
		}
		last = e.ID
		seen = append(seen, ev)
	}
	// It started with no step, went through its three, and ended done.
	if len(seen) < 5 || seen[0].State != jobs.StateRunning || len(seen[0].Steps) != 0 {
		t.Fatalf("events of the job: %d, first %+v", len(seen), seen[0])
	}
	var order []string
	for _, ev := range seen {
		if ev.Step != "" && (len(order) == 0 || order[len(order)-1] != ev.Step) {
			order = append(order, ev.Step)
		}
	}
	end := seen[len(seen)-1]
	if !slices.Equal(order, []string{"wait_save", "copy", "prune"}) || end.State != jobs.StateDone || len(end.Steps) != 3 || end.Result == nil {
		t.Fatalf("steps seen %v, last event %+v", order, end)
	}
	if got, want := end.Steps, done.Steps; len(got) != len(want) || !got[2].Ended.Equal(want[2].Ended) {
		t.Fatalf("the last event and the job differ: %+v and %+v", got, want)
	}
	// The admin's stream has it as well, and the filter by instance holds.
	var n int
	testkit.Eventually(t, 10*time.Second, func() bool {
		n = 0
		for _, e := range admins.all() {
			if e.Type == events.TypeJob {
				n++
			}
		}
		return n >= len(seen)
	}, "the admin got %d job events, the reader %d", n, len(seen))
	if n != len(seen) {
		t.Errorf("the admin got %d job events, the reader %d", n, len(seen))
	}
	select {
	case e := <-only.events:
		t.Fatalf("an event of a job of alpha on a stream for beta: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
	if a := h.raw("GET", "/api/v1/events?types=nonsense", rd, nil); a.status != http.StatusUnprocessableEntity {
		t.Fatalf("a type that is none: %d %s", a.status, a.body)
	}
}

// What the read role sees of an instance that passes its passwords on the
// command line is hidden in its jobs as everywhere: the reason in the
// result and the text of the error, in the job and in the event.
func TestTheReadRoleSeesNoDetailOfAJobOnAnInstanceWithSecretsOnTheCommandLine(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.install(testkit.BuildOptions{Version: "1.1"})
	h.install(testkit.BuildOptions{Version: "1.2-broken", Fake: map[string]any{"FakeFailStart": true}})
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{"name": "risky", "build": "1.0", "bind_ip": "127.0.0.1",
		"rules": fastRules(nil), "secret_delivery": "command_line"}, nil)
	h.create("plain", "1.0", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/risky/start?wait=true", struct{}{}, nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/plain/start?wait=true", struct{}{}, nil)
	rd := h.token("read")
	stream := h.recordEventsAs(rd, "")

	ok := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/risky/update", api.UpdateRequest{To: "1.1"}).ID)
	bad := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/risky/update", api.UpdateRequest{To: "1.2-broken"}).ID)
	plainBad := h.jobEnds(h.startAsync(h.admin, "/api/v1/instances/plain/update", api.UpdateRequest{To: "1.2-broken"}).ID)
	if ok.State != jobs.StateDone || bad.State != jobs.StateFailed || plainBad.State != jobs.StateFailed {
		t.Fatalf("jobs: %+v %+v %+v", ok, bad, plainBad)
	}
	var full, hidden instances.Status
	resultAs(t, ok, &full)
	resultAs(t, h.job(ok.ID, rd), &hidden)
	if full.Reason == instances.HiddenNotice || full.Reason == "" || hidden.Reason != instances.HiddenNotice || hidden.State != instances.Running || hidden.Build != "1.1" {
		t.Fatalf("result of the update: for the admin %+v, for a reader %+v", full, hidden)
	}
	if got := h.job(bad.ID, rd); got.Error == nil || got.Error.Message != instances.HiddenNotice || got.Error.Code != "update_failed" {
		t.Fatalf("error of the failed update for a reader: %+v", got.Error)
	}
	if bad.Error == nil || bad.Error.Message == instances.HiddenNotice || !strings.Contains(bad.Error.Message, "rolled back") {
		t.Fatalf("error of the failed update for the admin: %+v", bad.Error)
	}
	// An instance whose output is not hidden is shown in full.
	if got := h.job(plainBad.ID, rd); got.Error == nil || got.Error.Message == instances.HiddenNotice || !strings.Contains(got.Error.Message, "rolled back") {
		t.Fatalf("error of a job on a plain instance for a reader: %+v", got.Error)
	}
	for _, j := range jobList(t, h, rd, "?target=risky") {
		if j.Error != nil && j.Error.Message != instances.HiddenNotice {
			t.Errorf("the list shows a reader %+v", j.Error)
		}
		if st, ok := j.Result.(map[string]any); ok && st["reason"] != instances.HiddenNotice {
			t.Errorf("the list shows a reader %+v", st)
		}
	}
	// The events say the same.
	testkit.Eventually(t, 10*time.Second, func() bool {
		for _, e := range stream.all() {
			var ev jobs.Job
			if e.Type == events.TypeJob && json.Unmarshal(e.Data, &ev) == nil && ev.ID == plainBad.ID && ev.State == jobs.StateFailed {
				return true
			}
		}
		return false
	}, "no event of the last job ending")
	for _, e := range stream.all() {
		var ev jobs.Job
		if e.Type != events.TypeJob || json.Unmarshal(e.Data, &ev) != nil || e.Instance != "risky" {
			continue
		}
		if ev.Error != nil && ev.Error.Message != instances.HiddenNotice {
			t.Errorf("an event for a reader has the text of an error: %+v", ev.Error)
		}
		if st, ok := ev.Result.(map[string]any); ok && st["reason"] != instances.HiddenNotice {
			t.Errorf("an event for a reader has a reason: %+v", st)
		}
	}
	// Later, with Last-Event-ID, nothing else is given to a reader.
	late := h.recordEventsAs(rd, "0")
	testkit.Eventually(t, 10*time.Second, func() bool {
		for _, e := range late.all() {
			var ev jobs.Job
			if e.Type == events.TypeJob && json.Unmarshal(e.Data, &ev) == nil && ev.ID == plainBad.ID && ev.State == jobs.StateFailed {
				return true
			}
		}
		return false
	}, "the late stream has not the events up to the last job")
	for _, e := range late.all() {
		if strings.Contains(string(e.Data), "rolled back to 1.1") && e.Instance == "risky" {
			t.Errorf("a late event has the text of the error: %s", e.Data)
		}
	}
}

// --- ESM's own update as a job ---

// A self-update as a job: it ends done when the new binary is in place and
// ESM is about to end; what the release server tells ends it as failed.
func TestAsyncSelfUpdateIsAJob(t *testing.T) {
	t.Parallel()
	exe, tune := releaseServer(t, "systemd")
	h := newESMTuned(t, "process", tune)
	site := h.token("site")

	// A development build cannot tell that a release is newer: only the
	// release server tells, so the job fails with the call's code.
	nf := h.jobEnds(h.startAsync(h.admin, "/api/v1/box/self-update", struct{}{}).ID)
	if nf.State != jobs.StateFailed || nf.Error == nil || nf.Error.Code != "unknown_version" || nf.Target != "esm" || nf.Kind != jobs.KindSelfUpdate {
		t.Fatalf("self-update of a development build: %+v %+v", nf, nf.Error)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the old esm" {
		t.Fatalf("binary changed to %q", b)
	}
	// A site token may not end ESM for good, as a job either.
	if a := h.raw("POST", "/api/v1/box/self-update?async=true", site, api.SelfUpdateRequest{Force: true, ExitAnyway: true}); a.status != http.StatusForbidden || a.code() != "forbidden" {
		t.Fatalf("site token with exit_anyway: %d %s", a.status, a.body)
	}

	j := h.startAsync(site, "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true}, api.ActingUserHeader, "jane")
	done := h.jobEnds(j.ID)
	if done.State != jobs.StateDone || done.Error != nil || done.Actor.ActingUser != "jane" {
		t.Fatalf("self-update job: %+v %+v", done, done.Error)
	}
	if got, want := stepNames(done), []string{"verify_signature", "download", "place_binary", "restart"}; !slices.Equal(got, want) {
		t.Fatalf("steps %v, want %v", got, want)
	}
	checkStepsAreWhole(t, done)
	var res api.SelfUpdateResult
	resultAs(t, done, &res)
	if res.To != "v9.9.9" || !res.Restarting || res.Previous != exe+".previous" || res.Note == "" {
		t.Fatalf("result: %+v", res)
	}
	if d := done.Steps[1]; d.Bytes != int64(len("the new esm")) || d.Total != d.Bytes {
		t.Fatalf("the download step: %+v", d)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the new esm" {
		t.Fatalf("binary %q", b)
	}
	select {
	case <-h.svc.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the service was not asked to end after the update")
	}
	// Until it ends, a second one is refused at once, as the call is.
	if a := h.raw("POST", "/api/v1/box/self-update?async=true", h.admin, api.SelfUpdateRequest{Force: true}); a.status != http.StatusConflict || a.code() != "busy" {
		t.Fatalf("a second self-update as a job: %d %s", a.status, a.body)
	}
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=100", nil, &al)
	var end bool
	for _, e := range al.Entries {
		end = end || e.Action == "job.end" && e.Target == j.ID+" self_update esm" && e.OK && e.Actor.ActingUser == "jane"
	}
	if !end {
		t.Fatalf("audit: %+v", al.Entries)
	}
}

// With nothing to start ESM again a self-update is refused at once, as the
// call that waits is, and no job is made; with exit_anyway an admin starts it.
func TestAsyncSelfUpdateWithoutAServiceManager(t *testing.T) {
	t.Parallel()
	exe, tune := releaseServer(t, "")
	h := newESMTuned(t, "process", tune)
	a := h.raw("POST", "/api/v1/box/self-update?async=true", h.admin, api.SelfUpdateRequest{Force: true})
	if a.status != http.StatusConflict || a.code() != "no_service_manager" || a.header.Get("Location") != "" {
		t.Fatalf("self-update as a job with nothing to restart ESM: %d %s", a.status, a.body)
	}
	if got := jobList(t, h, h.admin, ""); len(got) != 0 {
		t.Fatalf("jobs: %+v", got)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the old esm" {
		t.Fatalf("binary changed to %q", b)
	}
	j := h.startAsync(h.admin, "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true, ExitAnyway: true})
	done := h.jobEnds(j.ID)
	var res api.SelfUpdateResult
	resultAs(t, done, &res)
	if done.State != jobs.StateDone || res.Restarting || !strings.Contains(res.Note, "by hand") {
		t.Fatalf("self-update job with exit_anyway: %+v %+v", done, res)
	}
	select {
	case <-h.svc.Exit():
	case <-time.After(5 * time.Second):
		t.Fatal("the service was not asked to end")
	}
}

// --- shutdown ---

// ESM's end waits for a job that runs as it waits for a request that runs,
// and the job ends done and is audited.
func TestShutdownWaitsForAJob(t *testing.T) {
	r := newSteamJobRig(t, 1500, nil)
	h := r.harness
	j := h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: "steam:main"})
	h.jobWhere(j.ID, 20*time.Second, func(j jobs.Job) bool { return j.Step == "download" })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	began := time.Now()
	if err := h.svc.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	h.svc = nil
	if time.Since(began) < 500*time.Millisecond {
		t.Errorf("the shutdown did not wait for the job: %s", time.Since(began))
	}
	if _, err := os.Stat(filepath.Join(h.root, "builds", "steam-main-100", "manifest.json")); err != nil {
		t.Fatalf("the build of the job that was waited for is not there: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
	if !strings.Contains(string(log), `"action":"job.end"`) || !strings.Contains(string(log), j.ID+" install steam:main") {
		t.Fatalf("the audit log has no end of the job:\n%s", log)
	}
}

// A job that does not end in the time the shutdown has is asked to stop, and
// the shutdown goes on without it, as it does for a request that is cut off.
func TestShutdownStopsAJobThatTakesTooLong(t *testing.T) {
	r := newSteamJobRig(t, 60000, nil)
	h := r.harness
	j := h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: "steam:main"})
	h.jobWhere(j.ID, 20*time.Second, func(j jobs.Job) bool { return j.Step == "download" })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	began := time.Now()
	err := h.svc.Shutdown(ctx)
	h.svc = nil
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("the shutdown took %s", took)
	}
	if _, err := os.Stat(filepath.Join(h.root, "builds", "steam-main-100")); err == nil {
		t.Fatal("a build was placed by a job that was stopped")
	}
}

// --- a post of raw JSON shapes ---

func TestJobRoutesTakeNoBodyButAnEmptyObject(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	for _, body := range []string{``, `{}`} {
		a := h.rawBody("DELETE", "/api/v1/jobs/nope", h.admin, body)
		if a.status != http.StatusNotFound {
			t.Errorf("DELETE with %q: %d %s", body, a.status, a.body)
		}
	}
	if a := h.rawBody("DELETE", "/api/v1/jobs/nope", h.admin, `[]`); a.status != http.StatusBadRequest {
		t.Errorf("DELETE with an array: %d %s", a.status, a.body)
	}
	if got := jobList(t, h, h.admin, ""); got == nil || len(got) != 0 {
		t.Errorf("an empty list is [], not null: %v", got)
	}
	var raw map[string]json.RawMessage
	if st := h.call("GET", "/api/v1/jobs", h.admin, nil, &raw); st != 200 || !bytes.Equal(raw["jobs"], []byte("[]")) {
		t.Errorf("GET /jobs of none: %d %s", st, raw["jobs"])
	}
}
