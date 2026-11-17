package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// siteRig is an API listener (tokens only) with one token of each role, and
// a stand-in for the ESM update that records how it was asked.
type siteRig struct {
	*rig
	tok map[auth.Role]string
	ids map[auth.Role]string

	mu      sync.Mutex
	updates []SelfUpdateRequest
}

func newSiteRig(t *testing.T) *siteRig {
	t.Helper()
	sr := &siteRig{tok: map[auth.Role]string{}, ids: map[auth.Role]string{}}
	sr.rig = newRig(t, Options{Page: stubPage, FailDelay: time.Millisecond, Listen: "127.0.0.1:8740",
		APIListener: &APIListenerInfo{Listen: apiHost, HostNames: []string{"esm.example.com"}},
		SelfUpdate: func(ctx context.Context, force, exitAnyway bool) (release.Result, bool, error) {
			sr.mu.Lock()
			defer sr.mu.Unlock()
			sr.updates = append(sr.updates, SelfUpdateRequest{Force: force, ExitAnyway: exitAnyway})
			return release.Result{From: "v1.0.0", To: "v1.1.0"}, !exitAnyway, nil
		}})
	for _, r := range []auth.Role{auth.RoleRead, auth.RoleOperator, auth.RoleAdmin, auth.RoleSite} {
		plain, tok, err := sr.auth.CreateToken("panel-"+string(r), r)
		if err != nil {
			t.Fatal(err)
		}
		sr.tok[r], sr.ids[r] = plain, tok.ID
	}
	return sr
}

func (sr *siteRig) updateCalls() []SelfUpdateRequest {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return append([]SelfUpdateRequest(nil), sr.updates...)
}

// asRole sends a request to the API listener with the token of role and the
// given further headers.
func (sr *siteRig) asRole(role auth.Role, method, path, body string, more map[string]string) *httptest.ResponseRecorder {
	sr.t.Helper()
	h := bearer(sr.tok[role])
	h["Content-Type"] = "application/json"
	for k, v := range more {
		h[k] = v
	}
	return sr.onAPI(method, path, body, h)
}

func (sr *siteRig) lastAudit() audit.Entry {
	sr.t.Helper()
	got, err := sr.audit.Recent(1)
	if err != nil || len(got) != 1 {
		sr.t.Fatalf("the audit log: %v %+v", err, got)
	}
	return got[0]
}

func (sr *siteRig) auditCount() int {
	sr.t.Helper()
	got, err := sr.audit.Recent(1000)
	if err != nil {
		sr.t.Fatal(err)
	}
	return len(got)
}

// An admin makes a site token with POST /tokens (the role is the one the
// command line's esm token create --role site asks for), the list shows it,
// and the token says what it is. Nothing else can: not the site role
// itself, not an operator.
func TestSiteTokenIsMadeByAnAdmin(t *testing.T) {
	sr := newSiteRig(t)
	rec := sr.asRole(auth.RoleAdmin, "POST", "/api/v1/tokens", `{"name":"panel","role":"site"}`, nil)
	var nt NewToken
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &nt) != nil || nt.Role != "site" || nt.Name != "panel" ||
		!strings.HasPrefix(nt.Token, "esm_") {
		t.Fatalf("admin makes a site token: %d %s", rec.Code, rec.Body)
	}
	if e := sr.lastAudit(); e.Action != "token.create" || !e.OK || e.Target != nt.ID || strings.Contains(sr.auditLog(), nt.Token) {
		t.Fatalf("audit entry %+v", e)
	}
	var tl TokenList
	rec = sr.asRole(auth.RoleAdmin, "GET", "/api/v1/tokens", "", nil)
	json.Unmarshal(rec.Body.Bytes(), &tl)
	found := false
	for _, tv := range tl.Tokens {
		found = found || (tv.ID == nt.ID && tv.Role == "site")
	}
	if rec.Code != http.StatusOK || !found {
		t.Fatalf("token list: %d %s", rec.Code, rec.Body)
	}
	rec = sr.onAPI("GET", "/api/v1/session", "", bearer(nt.Token))
	var si SessionInfo
	if json.Unmarshal(rec.Body.Bytes(), &si); rec.Code != http.StatusOK || si.Kind != "token" || si.Role != "site" || si.ID != nt.ID {
		t.Fatalf("session of the site token: %d %s", rec.Code, rec.Body)
	}

	for _, role := range []auth.Role{auth.RoleSite, auth.RoleOperator, auth.RoleRead} {
		rec := sr.asRole(role, "POST", "/api/v1/tokens", `{"name":"more","role":"site"}`, nil)
		if rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" {
			t.Errorf("%s token makes a token: %d %s", role, rec.Code, rec.Body)
		}
	}
	for _, role := range []string{"Site", "SITE", "site ", "panel"} {
		rec := sr.asRole(auth.RoleAdmin, "POST", "/api/v1/tokens", `{"name":"x","role":"`+role+`"}`, nil)
		if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "invalid" ||
			!strings.Contains(rec.Body.String(), "read, operator, admin or site") {
			t.Errorf("role %q: %d %s", role, rec.Code, rec.Body)
		}
	}
}

// What a site token may not do stays refused with 403 (and is recorded),
// whoever the header says it acts for. The audit events are the admin's
// alone, so the event stream does not give them to it.
func TestSiteTokenIsRefusedWhatIsAdminsAlone(t *testing.T) {
	sr := newSiteRig(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/tokens", ""},
		{"POST", "/api/v1/tokens", `{"role":"admin"}`},
		{"DELETE", "/api/v1/tokens/" + sr.ids[auth.RoleAdmin], "{}"},
		{"GET", "/api/v1/audit", ""},
		{"GET", "/api/v1/users", ""},
		{"POST", "/api/v1/instances", `{"name":"x"}`},
		{"DELETE", "/api/v1/instances/x", "{}"},
		{"PUT", "/api/v1/instances/x/secrets", `{"admin_password":"a long password"}`},
		{"PUT", "/api/v1/steam/main/password", `{"password":"x"}`},
		{"DELETE", "/api/v1/builds/1.0", "{}"},
		{"GET", "/api/v1/box/doctor", ""},
		{"PUT", "/api/v1/users/admin/password", `{"password":"a long password"}`},
		{"GET", "/api/v1/events?types=audit", ""},
	} {
		before := sr.auditCount()
		rec := sr.asRole(auth.RoleSite, c.method, c.path, c.body, header(ActingUserHeader, "admin"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("site token: %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
			continue
		}
		e := sr.lastAudit()
		if sr.auditCount() != before+1 || e.OK || e.Actor.Kind != "token" || e.Actor.Role != "site" || e.Actor.ID != sr.ids[auth.RoleSite] ||
			e.Actor.ActingUser != "admin" {
			t.Errorf("site token: %s %s: audit entry %+v", c.method, c.path, e)
		}
	}
}

// The header names the person a site token acts for. It is recorded with
// each audit entry of the call (refused calls too), in characters up to 64,
// and used for nothing else.
func TestActingUserIsRecordedFromSiteTokens(t *testing.T) {
	sr := newSiteRig(t)
	for _, who := range []string{
		"jane", "jane@example.com", "Jane Q. Public", "Zoë Ångström", "田中 太郎", "a",
		strings.Repeat("a", 64), strings.Repeat("é", 64), strings.Repeat("田", 64),
	} {
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, who))
		if rec.Code != http.StatusOK {
			t.Errorf("acting user %q: %d %s", who, rec.Code, rec.Body)
			continue
		}
		e := sr.lastAudit()
		if e.Action != "box.release_check" || !e.OK || e.Actor.Kind != "token" || e.Actor.ID != sr.ids[auth.RoleSite] ||
			e.Actor.Role != "site" || e.Actor.ActingUser != who {
			t.Errorf("acting user %q: audit entry %+v", who, e)
		}
	}
	if !strings.Contains(sr.auditLog(), `"actor":{"kind":"token","id":"`+sr.ids[auth.RoleSite]+`","role":"site","acting_user":"jane"}`) {
		t.Errorf("the audit line of a call for jane:\n%s", sr.auditLog())
	}

	// No header, or an empty one: the call is the token's own.
	for _, h := range []map[string]string{nil, header(ActingUserHeader, "")} {
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/release-check", "{}", h)
		if e := sr.lastAudit(); rec.Code != http.StatusOK || e.Actor.ActingUser != "" || e.Actor.Role != "site" {
			t.Errorf("header %v: %d, entry %+v", h, rec.Code, e)
		}
	}
	if line := strings.Split(strings.TrimSpace(sr.auditLog()), "\n"); strings.Contains(line[len(line)-1], "acting_user") {
		t.Errorf("an entry with nobody to act for has the field: %s", line[len(line)-1])
	}

	// It is a note: it never makes anyone of the token, whatever it says.
	for _, who := range []string{"admin", "socket", "operator", sr.ids[auth.RoleAdmin], "root"} {
		if rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/tokens", `{"name":"x","role":"read"}`, header(ActingUserHeader, who)); rec.Code != http.StatusForbidden {
			t.Errorf("a site token that says it acts for %q: %d %s", who, rec.Code, rec.Body)
		}
		var si SessionInfo
		rec := sr.asRole(auth.RoleSite, "GET", "/api/v1/session", "", header(ActingUserHeader, who))
		if json.Unmarshal(rec.Body.Bytes(), &si); si.Role != "site" || si.Kind != "token" || si.ID != sr.ids[auth.RoleSite] {
			t.Errorf("session of a site token that acts for %q: %s", who, rec.Body)
		}
	}
	// A token that looks like one is not kept.
	secret := "esm_" + strings.Repeat("Ab1-", 11)
	sr.asRole(auth.RoleSite, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, secret))
	if e := sr.lastAudit(); e.Actor.ActingUser != "esm_********" || strings.Contains(sr.auditLog(), secret) {
		t.Errorf("a token as the acting user: %+v", e)
	}
}

// A value that is too long, has a character that does not print, is not
// UTF-8, or is sent twice is refused with 400, in the audit log as a failed
// call of the token without a name to act for; no real call is made.
func TestActingUserIsRefusedWhenItIsNotPrintable(t *testing.T) {
	sr := newSiteRig(t)
	for name, who := range map[string]string{
		"65 characters":       strings.Repeat("a", 65),
		"65 two-byte letters": strings.Repeat("é", 65),
		"a long one":          strings.Repeat("jane ", 1000),
		"control character":   "ja\x01ne",
		"escape":              "ja\x1bne",
		"line break":          "jane\nsmith",
		"carriage return":     "jane\rsmith",
		"tab":                 "jane\tsmith",
		"delete":              "ja\x7fne",
		"next line":           "ja\u0085ne",
		"a text direction":    "\u202ejane",
		"zero width space":    "ja\u200bne",
		"no-break space":      "jane\u00a0smith",
		"line separator":      "jane\u2028smith",
		"not UTF-8":           "ja\xffne",
		"NUL":                 "ja\x00ne",
	} {
		before := sr.auditCount()
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, who))
		if rec.Code != http.StatusBadRequest || errorCode(rec) != "bad_acting_user" || !strings.Contains(rec.Body.String(), "64 printable characters") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
			continue
		}
		if strings.Contains(rec.Body.String(), "jane") || strings.Contains(rec.Body.String(), "\x01") {
			t.Errorf("%s: the answer repeats the value: %s", name, rec.Body)
		}
		if e := sr.lastAudit(); sr.auditCount() != before+1 || e.OK || e.Action != "box.release_check" || e.Actor.ID != sr.ids[auth.RoleSite] ||
			e.Actor.ActingUser != "" || e.Error == "" {
			t.Errorf("%s: audit entry %+v", name, e)
		}
	}
	// Twice, which of the two would be meant.
	req := httptest.NewRequest("POST", "/api/v1/box/release-check", strings.NewReader("{}"))
	req.Host = apiHost
	req.Header.Set("Authorization", "Bearer "+sr.tok[auth.RoleSite])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add(ActingUserHeader, "jane")
	req.Header.Add(ActingUserHeader, "john")
	rec := httptest.NewRecorder()
	sr.s.APIListenerHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || errorCode(rec) != "bad_acting_user" {
		t.Errorf("sent twice: %d %s", rec.Code, rec.Body)
	}
	// A read has no audit entry, but is refused the same.
	if rec := sr.asRole(auth.RoleSite, "GET", "/api/v1/session", "", header(ActingUserHeader, "ja\x01ne")); rec.Code != http.StatusBadRequest {
		t.Errorf("GET with a bad acting user: %d %s", rec.Code, rec.Body)
	}
	// A wrong header is no failed authentication: the token was fine, so an
	// address that sends many does not run into the limit of failed ones.
	for range AuthFailMax + 10 {
		sr.asRole(auth.RoleSite, "GET", "/api/v1/session", "", header(ActingUserHeader, "ja\x01ne"))
	}
	if rec := sr.onAPI("GET", "/api/v1/session", "", bearer("esm_"+strings.Repeat("x", 43))); rec.Code != http.StatusUnauthorized {
		t.Errorf("a wrong token after many wrong headers: %d %s, want 401, not the limit", rec.Code, rec.Body)
	}
	if rec := sr.asRole(auth.RoleSite, "GET", "/api/v1/session", "", header(ActingUserHeader, "jane")); rec.Code != http.StatusOK {
		t.Errorf("the token after them: %d %s", rec.Code, rec.Body)
	}
}

// From every other caller the header is not looked at: not recorded, and
// never an error, however wrong. They are what they are: a token of the
// other roles, a login's session and the unix socket.
func TestActingUserIsIgnoredFromEveryoneButSiteTokens(t *testing.T) {
	sr := newSiteRig(t)
	if _, err := sr.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sess, err := sr.auth.Login("", "admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	main := func(h map[string]string) map[string]string { h["host"] = "127.0.0.1:8740"; return h }
	wrong := strings.Repeat("x", 100) + "\x01\n"
	for name, send := range map[string]func(who string) *httptest.ResponseRecorder{
		"read token": func(who string) *httptest.ResponseRecorder {
			return sr.asRole(auth.RoleRead, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, who))
		},
		"operator token": func(who string) *httptest.ResponseRecorder {
			return sr.asRole(auth.RoleOperator, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, who))
		},
		"admin token": func(who string) *httptest.ResponseRecorder {
			return sr.asRole(auth.RoleAdmin, "POST", "/api/v1/box/release-check", "{}", header(ActingUserHeader, who))
		},
		"login": func(who string) *httptest.ResponseRecorder {
			return sr.do("POST", "/api/v1/box/release-check", "{}", main(header(ActingUserHeader, who,
				"Cookie", auth.SessionCookieName+"="+sess.ID, "X-CSRF-Token", sess.CSRF, "Content-Type", "application/json")))
		},
		"socket": func(who string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("POST", "/api/v1/box/release-check", strings.NewReader("{}"))
			req.Header.Set(ActingUserHeader, who)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			sr.s.SocketHandler().ServeHTTP(rec, req)
			return rec
		},
	} {
		for _, who := range []string{"jane", wrong, ""} {
			rec := send(who)
			if rec.Code != http.StatusOK {
				t.Errorf("%s with the header %q: %d %s", name, who, rec.Code, rec.Body)
				continue
			}
			if e := sr.lastAudit(); e.Action != "box.release_check" || e.Actor.ActingUser != "" || strings.Contains(sr.auditLog(), "acting_user") {
				t.Errorf("%s with the header %q: audit entry %+v", name, who, e)
			}
		}
	}
}

// A site token installs a build from a Steam source, and from nothing else:
// any folder, archive or address is refused with 403 before ESM looks at it
// (this ESM has no builds to look at: a call that got through would not
// answer 403), and the refusal is in the audit log with the source, without
// a password or query of an address.
func TestSiteTokenInstallsFromSteamSourcesOnly(t *testing.T) {
	sr := newSiteRig(t)
	for _, src := range []string{
		"", " ", "main", "steam", "steamy:main", "STEAM:main", "Steam:main", "xsteam:main", "/steam:main", "./steam:main",
		"/opt/esm/imports/build", "build.tar.gz", "build.zip", "../steam:main", `C:\steam:main`,
		"https://example.com/build.tar.gz", "http://example.com/build.tar.gz", "file:///etc/passwd",
		"https://user:hunter2@example.com/build.tar.gz?token=abc123#frag",
	} {
		body, _ := json.Marshal(InstallRequest{Source: src, SHA256: strings.Repeat("0", 64)})
		before := sr.auditCount()
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/builds", string(body), header(ActingUserHeader, "jane"))
		if rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" || !strings.Contains(rec.Body.String(), "Steam source") {
			t.Errorf("source %q: %d %s", src, rec.Code, rec.Body)
			continue
		}
		e := sr.lastAudit()
		if sr.auditCount() != before+1 || e.OK || e.Action != "build.install" || e.Actor.Role != "site" || e.Actor.ActingUser != "jane" {
			t.Errorf("source %q: audit entry %+v", src, e)
		}
	}
	if log := sr.auditLog(); strings.Contains(log, "hunter2") || strings.Contains(log, "abc123") {
		t.Errorf("the audit log keeps what an address carried:\n%s", log)
	}

	// A Steam source goes on to the Steam sources (this ESM has none: 422
	// no_source says it got there), written as the install itself reads it.
	for _, src := range []string{"steam:main", " steam:main", "steam:main \n", "\tsteam:main", "steam:", "steam:nope"} {
		for _, role := range []auth.Role{auth.RoleSite, auth.RoleAdmin} {
			body, _ := json.Marshal(InstallRequest{Source: src})
			rec := sr.asRole(role, "POST", "/api/v1/builds", string(body), nil)
			if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "no_source" {
				t.Errorf("%s token, source %q: %d %s", role, src, rec.Code, rec.Body)
			}
		}
	}
	rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/builds", `{"source":"steam:main","sha256":"`+strings.Repeat("0", 64)+`"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(rec) != "invalid" || !strings.Contains(rec.Body.String(), "sha256") {
		t.Errorf("a Steam source with a sha256: %d %s", rec.Code, rec.Body)
	}
	// The other roles are as they were.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleRead} {
		rec := sr.asRole(role, "POST", "/api/v1/builds", `{"source":"steam:main"}`, nil)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "needs the admin role") {
			t.Errorf("%s token installs: %d %s", role, rec.Code, rec.Body)
		}
	}
}

// A site token updates ESM itself, but never with exit_anyway: that ends ESM
// with nothing to start it again, so ESM would stay down because of the
// site. What is checked is the request that goes on: the last of a field
// sent twice counts for the check as it does for the update. An admin may.
func TestSiteTokenUpdatesESMButNeverExitsAnyway(t *testing.T) {
	sr := newSiteRig(t)
	for _, c := range []struct {
		body string
		want SelfUpdateRequest
	}{
		{`{}`, SelfUpdateRequest{}},
		{``, SelfUpdateRequest{}},
		{`{"force":true}`, SelfUpdateRequest{Force: true}},
		{`{"exit_anyway":false}`, SelfUpdateRequest{}},
		{`{"force":true,"exit_anyway":false}`, SelfUpdateRequest{Force: true}},
		{`{"exit_anyway":true,"exit_anyway":false}`, SelfUpdateRequest{}},
	} {
		n := len(sr.updateCalls())
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/self-update", c.body, header(ActingUserHeader, "jane"))
		calls := sr.updateCalls()
		if rec.Code != http.StatusOK || len(calls) != n+1 || calls[n] != c.want {
			t.Errorf("site token, %q: %d %s, update asked %+v", c.body, rec.Code, rec.Body, calls)
			continue
		}
		if e := sr.lastAudit(); !e.OK || e.Action != "box.self_update" || e.Actor.ActingUser != "jane" || e.Actor.Role != "site" {
			t.Errorf("site token, %q: audit entry %+v", c.body, e)
		}
	}
	for _, body := range []string{
		`{"exit_anyway":true}`, `{"force":true,"exit_anyway":true}`, `{"exit_anyway":false,"exit_anyway":true}`,
	} {
		n := len(sr.updateCalls())
		rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/self-update", body, header(ActingUserHeader, "jane"))
		if rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" || !strings.Contains(rec.Body.String(), "exit_anyway") || len(sr.updateCalls()) != n {
			t.Errorf("site token, %q: %d %s, update asked %+v", body, rec.Code, rec.Body, sr.updateCalls())
			continue
		}
		if e := sr.lastAudit(); e.OK || e.Action != "box.self_update" || e.Target != "esm" || e.Actor.ActingUser != "jane" || !strings.Contains(e.Error, "exit_anyway") {
			t.Errorf("site token, %q: audit entry %+v", body, e)
		}
	}
	// An admin may; the others never could.
	n := len(sr.updateCalls())
	if rec := sr.asRole(auth.RoleAdmin, "POST", "/api/v1/box/self-update", `{"exit_anyway":true}`, nil); rec.Code != http.StatusOK ||
		len(sr.updateCalls()) != n+1 || !sr.updateCalls()[n].ExitAnyway {
		t.Errorf("admin with exit_anyway: %d %s", rec.Code, rec.Body)
	}
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleRead} {
		if rec := sr.asRole(role, "POST", "/api/v1/box/self-update", `{}`, nil); rec.Code != http.StatusForbidden || len(sr.updateCalls()) != n+1 {
			t.Errorf("%s token updates ESM: %d %s", role, rec.Code, rec.Body)
		}
	}
	// A site token may look for a release, as any role may.
	if rec := sr.asRole(auth.RoleSite, "POST", "/api/v1/box/release-check", `{}`, nil); rec.Code != http.StatusOK {
		t.Errorf("release check by a site token: %d %s", rec.Code, rec.Body)
	}
}
