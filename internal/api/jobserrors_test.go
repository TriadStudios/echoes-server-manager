package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/jobs"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// The text of the error of a failed install or self-update is the one the
// call would have answered, and may name a path of the box: the binary and its
// update lock, the import folder, SteamCMD, the file an OS error names. The
// read role gets the code of the error and a text that names none, in the job,
// in the list, in the live events and in the events asked for later; the
// operator, admin and site roles get the text.
func TestTheReadRoleSeesNoPathInTheErrorOfAFailedJob(t *testing.T) {
	jr := newJobRig(t)
	live := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}, Public: true}, 256)
	defer live.Close()
	liveFull := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}}, 256)
	defer liveFull.Close()

	// What the code has made of such failures: the lock of an update that
	// cannot be made, the new binary that cannot be written, a rename, the
	// import folder, SteamCMD, and a lock that is there.
	cases := []struct {
		name, kind string
		err        error
		code, path string
	}{
		{"the update lock", jobs.KindSelfUpdate, selfUpdateError(fmt.Errorf("cannot make the update lock /opt/esm/esm.update.lock: %w", errors.New("open /opt/esm/esm.update.lock: permission denied"))),
			"update_failed", "/opt/esm"},
		{"writing next to the binary", jobs.KindSelfUpdate, selfUpdateError(fmt.Errorf("cannot write next to /opt/esm/esm: open /opt/esm/.esm-update-1: permission denied")),
			"update_failed", "/opt/esm"},
		{"a lock that is there", jobs.KindSelfUpdate, selfUpdateError(fmt.Errorf("%w: its lock /opt/esm/esm.update.lock is from 2026-03-01T12:00:00Z", release.ErrBusy)),
			"busy", "/opt/esm"},
		{"a rename", jobs.KindInstall, errors.New("rename /srv/esm/tmp/build-1/tree /srv/esm/builds/1.0: permission denied"),
			"internal", "/srv/esm"},
		{"the import folder", jobs.KindInstall, fault.Invalidf("there is no server-1.0 in the import folder /srv/esm/imports"),
			"invalid", "/srv/esm"},
		{"SteamCMD", jobs.KindInstall, fault.New(fault.Failed, "steamcmd_missing", "SteamCMD is not at /opt/steamcmd/steamcmd.sh: install it there"),
			"steamcmd_missing", "/opt/steamcmd"},
		{"an error that has no code of its own to say it in", jobs.KindInstall, fault.New(fault.Failed, "something_new", "it broke at /srv/esm/x"),
			"something_new", "/srv/esm"},
	}
	actor := audit.Actor{Kind: "token", ID: jr.ids[auth.RoleSite], Role: "site"}
	var ids []string
	full := map[string]string{} // the text of each error as the call answers it
	for _, c := range cases {
		j, err := jr.s.jobs.Start(jobs.Spec{Kind: c.kind, Target: "x", Actor: actor}, func(context.Context) (any, error) { return nil, c.err })
		if err != nil {
			t.Fatal(err)
		}
		done := jr.waitJob(j.ID)
		code, msg := classifyJob(c.err)
		if done.State != jobs.StateFailed || done.Error == nil || done.Error.Code != c.code || code != c.code || !strings.Contains(msg, c.path) {
			t.Fatalf("%s: the job as the runner made it: %+v %+v (code %q, text %q)", c.name, done, done.Error, code, msg)
		}
		ids = append(ids, j.ID)
		full[j.ID] = msg
	}

	check := func(role auth.Role, where string, got jobs.Job, c int) {
		t.Helper()
		if got.Error == nil || got.Error.Code != cases[c].code {
			t.Errorf("%s, %s, %s: error %+v, want code %s", role, where, cases[c].name, got.Error, cases[c].code)
			return
		}
		want := full[got.ID]
		if role == auth.RoleRead {
			want = pathFreeError(cases[c].code)
			if strings.Contains(got.Error.Message, cases[c].path) {
				t.Errorf("%s, %s, %s: the text names a path: %q", role, where, cases[c].name, got.Error.Message)
			}
		}
		if got.Error.Message != want {
			t.Errorf("%s, %s, %s: text %q, want %q", role, where, cases[c].name, got.Error.Message, want)
		}
	}
	index := map[string]int{}
	for c, id := range ids {
		index[id] = c
	}
	for _, role := range []auth.Role{auth.RoleRead, auth.RoleOperator, auth.RoleAdmin, auth.RoleSite} {
		for c, id := range ids {
			check(role, "GET /jobs/{id}", jr.jobAs(role, id), c)
		}
		rec := jr.call(role, "GET", "/api/v1/jobs", "")
		var list JobList
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Jobs) != len(cases) {
			t.Fatalf("%s lists the jobs: %d %s", role, rec.Code, rec.Body)
		}
		for _, j := range list.Jobs {
			check(role, "GET /jobs", j, index[j.ID])
		}
	}

	// The events, live and asked for later.
	seen := map[string]bool{}
	timeout := time.After(10 * time.Second)
	for len(seen) < len(cases) {
		select {
		case e := <-live.C:
			var j jobs.Job
			if json.Unmarshal(e.Data, &j) == nil && j.State == jobs.StateFailed {
				seen[j.ID] = true
				check(auth.RoleRead, "a live event", j, index[j.ID])
			}
		case <-timeout:
			t.Fatalf("%d of %d end events for a reader", len(seen), len(cases))
		}
	}
	for _, mem := range []struct {
		role string
		f    events.Filter
	}{{"read", events.Filter{Types: []string{events.TypeJob}, Public: true}}, {"admin", events.Filter{Types: []string{events.TypeJob}}}} {
		replay, _ := jr.bus.Since(0, mem.f)
		n := 0
		for _, e := range replay {
			var j jobs.Job
			if json.Unmarshal(e.Data, &j) != nil || j.State != jobs.StateFailed {
				continue
			}
			n++
			role := auth.RoleAdmin
			if mem.role == "read" {
				role = auth.RoleRead
			}
			check(role, "an event from memory", j, index[j.ID])
		}
		if n != len(cases) {
			t.Errorf("%d end events from memory for the %s role, want %d", n, mem.role, len(cases))
		}
	}
	for done := 0; done < len(cases); {
		select {
		case e := <-liveFull.C:
			var j jobs.Job
			if json.Unmarshal(e.Data, &j) == nil && j.State == jobs.StateFailed {
				done++
				check(auth.RoleAdmin, "a live event", j, index[j.ID])
			}
		case <-timeout:
			t.Fatal("the end events for the others did not come")
		}
	}

	// The update, backup and restore of an instance are not changed by this:
	// they name paths below the root of ESM at most, which GET /box tells.
	if pathFreeError("update_failed") != pathFreeErrors["update_failed"] || pathFreeError("no such code") == "" {
		t.Error("pathFreeError")
	}
}

// The target and the actor of a job are cut and scrubbed as the fields of an
// audit entry are, for everybody: in the answer that starts the job, in what a
// job shows, in the events (the read role's as well as the others') and in the
// event of its end.
func TestTheTargetAndTheActorOfAJobAreCleanedForEveryone(t *testing.T) {
	jr := newJobRig(t)
	live := jr.bus.Subscribe(events.Filter{Types: []string{events.TypeJob}}, 64)
	defer live.Close()
	const token = "esm_abcdefghijklmnopqrstuv"
	c := &call{role: auth.RoleAdmin, actor: audit.Actor{Kind: "token", ID: "tok_1 " + token, Role: "admin", ActingUser: "jane " + token + " -ServerPassword=hunter2 " + strings.Repeat("u", 400)}}
	rec := httptest.NewRecorder()
	target := "https://example.com/" + token + ".zip?x=-ServerPassword=hunter2" + strings.Repeat("p", 400)
	err := jr.s.startJob(rec, c, jobs.KindInstall, target, func(context.Context) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	var started jobs.Job
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &started) != nil {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	done := jr.waitJob(started.ID)
	bodies := map[string]string{
		"the answer":  rec.Body.String(),
		"GET /jobs/x": jr.call(auth.RoleAdmin, "GET", "/api/v1/jobs/"+started.ID, "").Body.String(),
		"GET /jobs":   jr.call(auth.RoleSite, "GET", "/api/v1/jobs", "").Body.String(),
	}
	timeout := time.After(10 * time.Second)
	for n := 0; n < 2; n++ {
		select {
		case e := <-live.C:
			bodies["an event"+fmt.Sprint(n)] = string(e.Data)
		case <-timeout:
			t.Fatal("no events")
		}
	}
	for what, body := range bodies {
		for _, secret := range []string{token, "hunter2"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s has %q: %.300s", what, secret, body)
			}
		}
		if !strings.Contains(body, "esm_********") {
			t.Errorf("%s has no mask where the token was: %.300s", what, body)
		}
	}
	for _, v := range []string{done.Target, done.Actor.ID, done.Actor.ActingUser, started.Target, started.Actor.ID, started.Actor.ActingUser} {
		if len(v) > audit.MaxField {
			t.Errorf("a field of %d bytes, at most %d", len(v), audit.MaxField)
		}
	}
	if done.Actor.Kind != "token" || done.Actor.Role != "admin" {
		t.Errorf("actor %+v", done.Actor)
	}
}

// jobAs reads a job with the token of role.
func (jr *jobRig) jobAs(role auth.Role, id string) jobs.Job {
	jr.t.Helper()
	rec := jr.call(role, "GET", "/api/v1/jobs/"+id, "")
	var j jobs.Job
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
		jr.t.Fatalf("GET job %s as %s: %d %s", id, role, rec.Code, rec.Body)
	}
	return j
}
