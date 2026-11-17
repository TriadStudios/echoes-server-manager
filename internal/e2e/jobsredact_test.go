package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// decodeJob reads the job that a job event carries.
func decodeJob(e events.Event, j *jobs.Job) bool { return json.Unmarshal(e.Data, j) == nil }

// tokenID is the id of a token, as the token itself says it.
func (h *harness) tokenID(token string) string {
	h.t.Helper()
	var si api.SessionInfo
	if st := h.call("GET", "/api/v1/session", token, nil, &si); st != http.StatusOK || si.ID == "" {
		h.t.Fatalf("GET /session: %d %+v", st, si)
	}
	return si.ID
}

// A job carries what the audit log records of the call that started it: who
// (the id of the token, and the person a site token acts for), the source of
// an install (a folder or an archive in the import folder, an address), and,
// of ESM's own update, the paths of the binary. That is for the operator role
// and above. The read role gets the kind and role of the actor, the kind of
// source (the host of an address, a Steam source as it is) and no paths, in
// GET /jobs, GET /jobs/{id}, the live events and the events asked for later.
func TestTheReadRoleSeesNoAuditDataInJobs(t *testing.T) {
	t.Parallel()
	exe, releaseTune := releaseServer(t, "systemd")
	source, sum, _, client, letGo := slowArchive(t, "5.0")
	letGo()
	r := newSteamJobRig(t, false, func(o *service.Options) {
		releaseTune(o)
		o.HTTPClient = client
	})
	h := r.harness
	rd, op, site := h.token("read"), h.token("operator"), h.token("site")
	adminID, siteID := h.tokenID(h.admin), h.tokenID(site)
	readers, admins := h.recordEventsAs(rd, ""), h.recordEvents()

	const who = "jane@example.com"
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "2.0"})
	dir3 := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "3.0"})
	testkit.MakeTarGz(t, dir3, filepath.Join(h.src, "three.tar.gz"))
	address, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}

	steam := h.jobEnds(h.startAsync(site, "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, api.ActingUserHeader, who).ID)
	folder := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: dir}).ID)
	archive := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: "three.tar.gz"}).ID)
	remote := h.jobEnds(h.startAsync(h.admin, "/api/v1/builds", api.InstallRequest{Source: source, SHA256: sum}).ID)
	self := h.jobEnds(h.startAsync(site, "/api/v1/box/self-update", api.SelfUpdateRequest{Force: true}, api.ActingUserHeader, who).ID)
	for _, j := range []jobs.Job{steam, folder, archive, remote, self} {
		if j.State != jobs.StateDone {
			t.Fatalf("job: %+v %+v", j, j.Error)
		}
	}

	cases := []struct {
		job                 jobs.Job
		target, readTarget  string
		actor, acting       string
		binary, previousBin string
	}{
		{steam, "steam:main", "steam:main", siteID, who, "", ""},
		{folder, dir, "folder", adminID, "", "", ""},
		{archive, "three.tar.gz", "archive", adminID, "", "", ""},
		{remote, source, address.Scheme + "://" + address.Host, adminID, "", "", ""},
		{self, "esm", "esm", siteID, who, exe, exe + ".previous"},
	}
	check := func(role, where string, got jobs.Job, full bool) {
		t.Helper()
		for _, c := range cases {
			if got.ID != c.job.ID {
				continue
			}
			wantTarget, wantActor, wantActing, wantBin, wantPrev := c.readTarget, "", "", "", ""
			if full {
				wantTarget, wantActor, wantActing, wantBin, wantPrev = c.target, c.actor, c.acting, c.binary, c.previousBin
			}
			if got.Target != wantTarget || got.Actor.ID != wantActor || got.Actor.ActingUser != wantActing || got.Kind != c.job.Kind ||
				got.Actor.Kind != "token" || got.Actor.Role != c.job.Actor.Role || got.State != jobs.StateDone || len(got.Steps) != len(c.job.Steps) {
				t.Errorf("%s, %s, job %s: target %q actor %+v, want target %q, actor id %q, acting user %q", role, where, c.job.Kind, got.Target, got.Actor, wantTarget, wantActor, wantActing)
			}
			if c.job.Kind == jobs.KindSelfUpdate {
				var res api.SelfUpdateResult
				resultAs(t, got, &res)
				if res.Binary != wantBin || res.Previous != wantPrev || res.To != "v9.9.9" {
					t.Errorf("%s, %s: the self-update result: %+v, want binary %q and previous %q", role, where, res, wantBin, wantPrev)
				}
			}
			return
		}
		t.Errorf("%s, %s: job %s is none of the jobs", role, where, got.ID)
	}
	for role, tok := range map[string]string{"read": rd, "operator": op, "admin": h.admin, "site": site} {
		full := role != "read"
		for _, c := range cases {
			check(role, "GET /jobs/{id}", h.job(c.job.ID, tok), full)
		}
		list := jobList(t, h, tok, "")
		if len(list) != len(cases) {
			t.Fatalf("%s lists %d jobs, want %d", role, len(list), len(cases))
		}
		for _, j := range list {
			check(role, "GET /jobs", j, full)
		}
		// The filter by target is matched against what the caller sees.
		if got := jobList(t, h, tok, "?target="+url.QueryEscape(dir)); (len(got) == 1) != full || len(got) > 1 {
			t.Errorf("%s asks for the jobs on %q and gets %d", role, dir, len(got))
		}
		if got := jobList(t, h, tok, "?target=folder"); (len(got) == 1) != !full || len(got) > 1 {
			t.Errorf("%s asks for the jobs on folder and gets %d", role, len(got))
		}
	}

	// The events: live to a reader, and the ones asked for later with
	// Last-Event-ID. A reader's have nothing that a reader may not see, and a
	// job event is there for each of the jobs; the admin's have all of it.
	late := h.recordEventsAs(rd, "0")
	hidden := []string{siteID, adminID, who, dir, h.src, exe, "three.tar.gz", address.Path}
	ended := func(l *eventLog) int {
		seen := map[string]bool{}
		for _, e := range l.all() {
			var ev jobs.Job
			if e.Type == events.TypeJob && decodeJob(e, &ev) && ev.State == jobs.StateDone {
				seen[ev.ID] = true
			}
		}
		return len(seen)
	}
	for name, l := range map[string]*eventLog{"live reader": readers, "late reader": late, "admin": admins} {
		testkit.Eventually(t, 20*time.Second, func() bool { return ended(l) >= len(cases) }, "the %s stream has the end of %d of the jobs", name, ended(l))
	}
	for name, l := range map[string]*eventLog{"live reader": readers, "late reader": late} {
		n := 0
		for _, e := range l.all() {
			if e.Type != events.TypeJob {
				continue
			}
			n++
			for _, s := range hidden {
				if strings.Contains(string(e.Data), s) {
					t.Errorf("a job event on the %s stream has %q: %s", name, s, e.Data)
				}
			}
			if !strings.Contains(string(e.Data), `"kind":"token"`) {
				t.Errorf("a job event on the %s stream lost the kind of the actor: %s", name, e.Data)
			}
		}
		if n < 3*len(cases) {
			t.Errorf("%d job events on the %s stream", n, name)
		}
	}
	var all strings.Builder
	for _, e := range admins.all() {
		if e.Type == events.TypeJob {
			all.Write(e.Data)
		}
	}
	for _, s := range []string{siteID, adminID, who, dir, exe, "three.tar.gz", address.Path} {
		if !strings.Contains(all.String(), s) {
			t.Errorf("the admin's job events lack %q", s)
		}
	}

	// What a site token says it acts for is cut and scrubbed as the audit log
	// does, in the job too, for everybody.
	const secret = "esm_abcdefghijklmnopqrstuv"
	const masked = "jane esm_******** -ServerPassword=********"
	again := h.startAsync(site, "/api/v1/builds", api.InstallRequest{Source: "steam:main"}, api.ActingUserHeader, "jane "+secret+" -ServerPassword=hunter2")
	h.jobEnds(again.ID)
	for name, tok := range map[string]string{"admin": h.admin, "site": site} {
		j := h.job(again.ID, tok)
		if j.Actor.ActingUser != masked || again.Actor.ActingUser != masked {
			t.Errorf("%s: the acting user of a job is %q (started as %q)", name, j.Actor.ActingUser, again.Actor.ActingUser)
		}
	}
	h.waitAudit("end of the last job", func(e audit.Entry) bool { return e.Action == "job.end" && strings.HasPrefix(e.Target, again.ID+" ") })
	log, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
	if strings.Contains(string(log), secret) || strings.Contains(string(log), "hunter2") {
		t.Errorf("the audit log has what the acting user wrote:\n%s", log)
	}
}
