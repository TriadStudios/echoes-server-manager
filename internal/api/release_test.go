package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// releaseLookups serves the release list with tag over TLS on loopback and
// returns a checker for this ESM (v1.0.0) that asks it, and the number of
// lookups so far.
func releaseLookups(t *testing.T, tag string, enabled bool) (*release.Checker, *atomic.Int32) {
	t.Helper()
	var looks atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		looks.Add(1)
		json.NewEncoder(w).Encode(release.Release{Tag: tag, URL: "https://example.invalid/" + tag})
	}))
	t.Cleanup(srv.Close)
	c := &release.Client{API: srv.URL + "/latest", HTTP: srv.Client()}
	note := ""
	if !enabled {
		note = "update_check is off in esm.json"
	}
	return release.NewChecker(c, "v1.0.0", time.Hour, enabled, note), &looks
}

func releaseStatus(t *testing.T, rec *httptest.ResponseRecorder) release.Status {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("release check: %d %s", rec.Code, rec.Body)
	}
	var st release.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"enabled", "current", "latest", "available", "url", "note", "reused"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("the release status has no %q: %s", k, rec.Body)
		}
	}
	return st
}

// POST /box/release-check looks for a newer release by hand and answers
// with the release status of GET /box. It changes nothing, so the read
// role may call it, with a token or a login; a login's browser session
// sends the CSRF token like for every POST, and without a login it is
// refused. A second check right after gives the same result without a
// lookup. Each call is in the audit log.
func TestReleaseCheckByHand(t *testing.T) {
	checker, looks := releaseLookups(t, "v1.1.0", true)
	rg := newRig(t, Options{Version: "v1.0.0", Release: checker, FailDelay: time.Millisecond})
	if rec := rg.do("POST", "/api/v1/box/release-check", "{}", jsonHeader); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a login: %d %s", rec.Code, rec.Body)
	}
	if _, err := rg.auth.CreateUser("viewer", firstPassword, auth.RoleRead); err != nil {
		t.Fatal(err)
	}
	session, csrf, _ := rg.login("viewer", firstPassword, rigRemote)
	for _, bad := range []string{"", "wrong-csrf"} {
		rec := rg.do("POST", "/api/v1/box/release-check", "{}", sessionHeader(session, bad, rigRemote))
		if rec.Code != http.StatusForbidden || errCode(rec) != "csrf" {
			t.Fatalf("a session with CSRF token %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if n := looks.Load(); n != 0 {
		t.Fatalf("refused calls looked the release up %d times", n)
	}
	st := releaseStatus(t, rg.do("POST", "/api/v1/box/release-check", "{}", sessionHeader(session, csrf, rigRemote)))
	if !st.Enabled || st.Current != "v1.0.0" || st.Latest != "v1.1.0" || !st.Available || st.Reused || st.Checked.IsZero() || looks.Load() != 1 {
		t.Fatalf("a read session's check: %+v (%d lookups)", st, looks.Load())
	}

	reader, _, err := rg.auth.CreateToken("reader", auth.RoleRead)
	if err != nil {
		t.Fatal(err)
	}
	// No body is fine too.
	again := releaseStatus(t, rg.do("POST", "/api/v1/box/release-check", "", header("Authorization", "Bearer "+reader)))
	if !again.Reused || again.Latest != "v1.1.0" || !again.Checked.Equal(st.Checked) || !strings.Contains(again.Note, "without asking") || looks.Load() != 1 {
		t.Fatalf("a read token's check right after: %+v (%d lookups)", again, looks.Load())
	}
	if rec := rg.do("POST", "/api/v1/box/release-check", `{"force":true}`, header("Content-Type", "application/json", "Authorization", "Bearer "+reader)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a body with a field: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.do("GET", "/api/v1/box/release-check", "", header("Authorization", "Bearer "+reader)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}

	entries := rg.entries("box.release_check")
	ok := 0
	for _, e := range entries {
		if e.OK {
			ok++
			if e.Target != "esm" || (e.Actor.Kind != audit.ActorUser && e.Actor.Kind != audit.ActorToken) {
				t.Errorf("entry %+v", e)
			}
		}
	}
	if ok != 2 {
		t.Fatalf("%d entries of checks that answered, want 2: %+v", ok, entries)
	}
}

// With update_check off the check by hand answers the off status and looks
// nothing up.
func TestReleaseCheckWhenOff(t *testing.T) {
	checker, looks := releaseLookups(t, "v1.1.0", false)
	rg := newRig(t, Options{Version: "v1.0.0", Release: checker})
	st := releaseStatus(t, rg.socket("POST", "/api/v1/box/release-check", "{}"))
	if st.Enabled || st.Available || st.Latest != "" || st.Note != "update_check is off in esm.json" || st.Current != "v1.0.0" || looks.Load() != 0 {
		t.Fatalf("off: %+v (%d lookups)", st, looks.Load())
	}
}
