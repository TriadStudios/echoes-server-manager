package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The text of the error of a job that failed is the one the call would have
// answered, and may name a path. Here two real failures: a self-update of a
// binary whose folder is not there (the update lock cannot be made, and the
// error names the lock), and the install of a folder while the folder that ESM
// unpacks in is a file. The operator, admin and site roles get the text; the
// read role gets the code and a text that names no path, in GET /jobs/{id}, in
// GET /jobs, in the live events and in the events asked for later.
func TestTheReadRoleSeesNoPathInTheErrorOfAFailedInstallOrSelfUpdate(t *testing.T) {
	t.Parallel()
	_, releaseTune := releaseServer(t, "systemd")
	gone := filepath.Join(t.TempDir(), "gone-dir", "esm")
	h := newESMTuned(t, "process", func(o *service.Options) {
		releaseTune(o)
		o.Executable = gone
	})
	rd, op, site := h.token("read"), h.token("operator"), h.token("site")
	readers, admins := h.recordEventsAs(rd, ""), h.recordEvents()

	self := h.jobEnds(h.startAsync(site, "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true}).ID)
	tmp := filepath.Join(h.root, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "2.0"})
	install := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: dir}).ID)

	cases := []struct {
		job  jobs.Job
		code string
		path string // what the full text names and the read role's does not
		read string // what the read role's text is
	}{
		{self, "update_failed", filepath.Dir(gone), "the self-update failed"},
		{install, "internal", "tmp", "the job failed; the operator role is shown why"},
	}
	for _, c := range cases {
		if c.job.State != jobs.StateFailed || c.job.Error == nil || c.job.Error.Code != c.code || !strings.Contains(c.job.Error.Message, c.path) {
			t.Fatalf("job %s did not fail as it was made to: %+v %+v", c.job.Kind, c.job, c.job.Error)
		}
	}
	check := func(who, where string, got jobs.Job, full bool) {
		t.Helper()
		for _, c := range cases {
			if got.ID != c.job.ID {
				continue
			}
			want := c.read
			if full {
				want = c.job.Error.Message
			}
			if got.Error == nil || got.Error.Code != c.code || got.Error.Message != want {
				t.Errorf("%s, %s, %s: error %+v, want code %s and text %q", who, where, c.job.Kind, got.Error, c.code, want)
			}
			return
		}
		t.Errorf("%s, %s: job %s is none of the jobs", who, where, got.ID)
	}
	for who, tok := range map[string]string{"read": rd, "operator": op, "admin": h.admin, "site": site} {
		full := who != "read"
		for _, c := range cases {
			check(who, "GET /jobs/{id}", h.job(c.job.ID, tok), full)
		}
		list := jobList(t, h, tok, "?state=failed")
		if len(list) != len(cases) {
			t.Fatalf("%s lists %d failed jobs, want %d", who, len(list), len(cases))
		}
		for _, j := range list {
			check(who, "GET /jobs", j, full)
		}
	}

	// The events: live to a reader, and asked for later with Last-Event-ID.
	late := h.recordEventsAs(rd, "0")
	failed := func(l *eventLog) map[string]jobs.Job {
		out := map[string]jobs.Job{}
		for _, e := range l.all() {
			var j jobs.Job
			if e.Type == events.TypeJob && decodeJob(e, &j) && j.State == jobs.StateFailed {
				out[j.ID] = j
			}
		}
		return out
	}
	for name, l := range map[string]*eventLog{"live reader": readers, "late reader": late, "admin": admins} {
		testkit.Eventually(t, 20*time.Second, func() bool { return len(failed(l)) == len(cases) }, "the %s stream has the end of %d of the jobs", name, len(cases))
		for _, j := range failed(l) {
			check(name, "an event", j, name == "admin")
		}
	}
	for name, l := range map[string]*eventLog{"live reader": readers, "late reader": late} {
		for _, e := range l.all() {
			if e.Type != events.TypeJob {
				continue
			}
			// As the strings the JSON holds, not as its text, where a path of
			// Windows has each backslash doubled.
			strs := jobStrings(e.Data)
			for _, c := range cases {
				if anyHas(strs, c.path) {
					t.Errorf("an event on the %s stream names %q: %s", name, c.path, e.Data)
				}
			}
		}
	}
	if st := h.call("GET", "/api/v1/jobs/"+install.ID, rd, nil, nil); st != http.StatusOK {
		t.Fatalf("the reader reads the job: %d", st)
	}
}
