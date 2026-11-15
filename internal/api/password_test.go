package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

const (
	firstPassword  = "correct horse battery"
	secondPassword = "a second long password"
	weakPassword   = "short pw"
	// The address the rig's requests come from unless they say otherwise,
	// and another one.
	rigRemote   = "192.0.2.1:1234"
	otherRemote = "198.51.100.7:5000"
)

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// login logs name in from remote and returns the session cookie value and
// the CSRF token ("" for both when the login is refused) with the status.
func (r *rig) login(name, password, remote string) (session, csrf string, status int) {
	r.t.Helper()
	rec := r.do("POST", "/api/v1/login", jsonBody(LoginRequest{Username: name, Password: password}),
		header("Content-Type", "application/json", "remote", remote))
	if rec.Code != http.StatusOK {
		return "", "", rec.Code
	}
	var lr LoginResult
	json.Unmarshal(rec.Body.Bytes(), &lr)
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			session = c.Value
		}
	}
	if session == "" || lr.CSRFToken == "" {
		r.t.Fatalf("login of %s: no session: %s", name, rec.Body)
	}
	return session, lr.CSRFToken, rec.Code
}

// sessionHeader is the header of a request with the session cookie, and
// with the CSRF token when csrf is not "".
func sessionHeader(session, csrf, remote string) map[string]string {
	h := header("Content-Type", "application/json", "Cookie", auth.SessionCookieName+"="+session, "remote", remote)
	if csrf != "" {
		h[auth.CSRFHeader] = csrf
	}
	return h
}

// open reports whether a session still answers GET /session.
func (r *rig) open(session string) bool {
	r.t.Helper()
	rec := r.do("GET", "/api/v1/session", "", header("Cookie", auth.SessionCookieName+"="+session))
	return rec.Code == http.StatusOK
}

// socket sends a request on the unix socket's handler.
func (r *rig) socket(method, target, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.s.SocketHandler().ServeHTTP(rec, req)
	return rec
}

func errCode(rec *httptest.ResponseRecorder) string {
	var eb ErrorBody
	json.Unmarshal(rec.Body.Bytes(), &eb)
	return eb.Error.Code
}

// entries returns the audit entries of one action, oldest first.
func (r *rig) entries(action string) []audit.Entry {
	r.t.Helper()
	all, err := r.audit.Recent(1000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []audit.Entry
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// noPasswords fails when the audit log holds any of the passwords.
func (r *rig) noPasswords(passwords ...string) {
	r.t.Helper()
	log := r.auditLog()
	for _, p := range passwords {
		if strings.Contains(log, p) {
			r.t.Errorf("the audit log holds a password (%d characters):\n%s", len(p), log)
		}
	}
}

func (r *rig) change(session, csrf, remote, current, next string) *httptest.ResponseRecorder {
	r.t.Helper()
	return r.do("POST", "/api/v1/session/password", jsonBody(PasswordChangeRequest{CurrentPassword: current, NewPassword: next}),
		sessionHeader(session, csrf, remote))
}

// A login changes its own password with the current one, through the
// page's session: the CSRF token is needed like for every change, the
// password rules apply, a token or the socket cannot do it, and nothing
// refused counts as a failed login. The change ends the login's other
// sessions and keeps the one it was made in. The audit log has an entry
// for every try and never a password.
func TestChangePasswordKeepsThisSessionOnly(t *testing.T) {
	rg := newRig(t, Options{FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", firstPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	here, csrf, _ := rg.login("admin", firstPassword, rigRemote)
	other, _, _ := rg.login("admin", firstPassword, otherRemote)
	tokenPlain, tok, err := rg.auth.CreateToken("script", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	body := jsonBody(PasswordChangeRequest{CurrentPassword: firstPassword, NewPassword: secondPassword})

	// Refusals, each as often as a failed login could be, so that one of
	// them counting would lock the login out below.
	for range auth.DefaultRateMax {
		if rec := rg.change(here, "", rigRemote, firstPassword, secondPassword); rec.Code != http.StatusForbidden || errCode(rec) != "csrf" {
			t.Fatalf("without the CSRF token: %d %s", rec.Code, rec.Body)
		}
		if rec := rg.change(here, "wrong-csrf", rigRemote, firstPassword, secondPassword); rec.Code != http.StatusForbidden || errCode(rec) != "csrf" {
			t.Fatalf("with a wrong CSRF token: %d %s", rec.Code, rec.Body)
		}
		if rec := rg.change(here, csrf, rigRemote, firstPassword, weakPassword); rec.Code != http.StatusUnprocessableEntity || errCode(rec) != "invalid" ||
			!strings.Contains(rec.Body.String(), "at least 12 characters") {
			t.Fatalf("a new password the rules refuse: %d %s", rec.Code, rec.Body)
		}
		rec := rg.do("POST", "/api/v1/session/password", body, header("Content-Type", "application/json", "Authorization", "Bearer "+tokenPlain))
		if rec.Code != http.StatusForbidden || errCode(rec) != "session_only" {
			t.Fatalf("with an admin token: %d %s", rec.Code, rec.Body)
		}
		if rec := rg.socket("POST", "/api/v1/session/password", body); rec.Code != http.StatusForbidden || errCode(rec) != "session_only" {
			t.Fatalf("on the socket: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := rg.change(here, csrf, rigRemote, "", secondPassword); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("without the current password: %d %s", rec.Code, rec.Body)
	}
	if !rg.open(here) || !rg.open(other) {
		t.Fatal("a refused change ended a session")
	}
	if _, _, st := rg.login("admin", firstPassword, rigRemote); st != http.StatusOK {
		t.Fatalf("after the refusals the old password logs in: %d", st)
	}

	rec := rg.change(here, csrf, rigRemote, firstPassword, secondPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	var res PasswordResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	// The two other sessions (one from otherRemote, one from the login
	// above) end; this one goes on.
	if res.Username != "admin" || res.EndedSessions != 2 || strings.Contains(rec.Body.String(), secondPassword) {
		t.Fatalf("change answered %s", rec.Body)
	}
	if !rg.open(here) {
		t.Fatal("the session the change was made in ended")
	}
	if rg.open(other) {
		t.Fatal("another session of the login lives on")
	}
	if _, _, st := rg.login("admin", firstPassword, rigRemote); st != http.StatusUnauthorized {
		t.Fatalf("the old password after the change: %d", st)
	}
	if _, _, st := rg.login("admin", secondPassword, rigRemote); st != http.StatusOK {
		t.Fatalf("the new password: %d", st)
	}

	// One entry for every try, naming the login (or the token or socket
	// that tried), and no password anywhere in the log.
	entries := rg.entries("user.password.change")
	if want := 5*auth.DefaultRateMax + 2; len(entries) != want {
		t.Fatalf("%d change entries, want %d", len(entries), want)
	}
	last := entries[len(entries)-1]
	if !last.OK || last.Actor.Kind != audit.ActorUser || last.Actor.ID != "admin" || last.Target != "admin" {
		t.Fatalf("the entry of the change: %+v", last)
	}
	kinds := map[string]int{}
	for _, e := range entries[:len(entries)-1] {
		if e.OK {
			t.Errorf("a refused change is recorded as done: %+v", e)
		}
		kinds[e.Actor.Kind+" "+e.Actor.ID]++
	}
	if kinds["user admin"] != 3*auth.DefaultRateMax+1 || kinds[audit.ActorToken+" "+tok.ID] != auth.DefaultRateMax || kinds["socket socket"] != auth.DefaultRateMax {
		t.Fatalf("who tried: %v", kinds)
	}
	rg.noPasswords(firstPassword, secondPassword, weakPassword)
}

// A wrong current password counts against the login rate limit exactly
// like a failed login, by the user name (from any address) and by the
// client's address: failures on the form lock the login out, failed logins
// lock the form out, and a wrong password waits for the same pause.
func TestWrongCurrentPasswordCountsAsAFailedLogin(t *testing.T) {
	const pause = 150 * time.Millisecond
	t.Run("form then login", func(t *testing.T) {
		rg := newRig(t, Options{FailDelay: pause})
		if _, err := rg.auth.CreateUser("admin", firstPassword, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		here, csrf, _ := rg.login("admin", firstPassword, rigRemote)
		for i := range auth.DefaultRateMax {
			start := time.Now()
			rec := rg.change(here, csrf, rigRemote, "not the password "+string(rune('a'+i)), secondPassword)
			if rec.Code != http.StatusForbidden || errCode(rec) != "wrong_password" {
				t.Fatalf("wrong current password %d: %d %s", i, rec.Code, rec.Body)
			}
			if d := time.Since(start); d < pause {
				t.Fatalf("a wrong current password answered after %s, want at least %s", d, pause)
			}
		}
		rec := rg.change(here, csrf, rigRemote, firstPassword, secondPassword)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("the right password at the limit: %d %v %s", rec.Code, rec.Header(), rec.Body)
		}
		if _, _, st := rg.login("admin", firstPassword, otherRemote); st != http.StatusTooManyRequests {
			t.Fatalf("a login from another address after the failures on the form: %d", st)
		}
		if _, _, st := rg.login("admin", firstPassword, rigRemote); st != http.StatusTooManyRequests {
			t.Fatalf("a login from the same address after the failures on the form: %d", st)
		}
		if !rg.open(here) {
			t.Fatal("failures on the form ended the session")
		}
		if got := rg.entries("user.password.change"); len(got) != auth.DefaultRateMax+1 {
			t.Fatalf("%d entries", len(got))
		}
		rg.noPasswords(firstPassword, secondPassword)
	})
	t.Run("login then form", func(t *testing.T) {
		rg := newRig(t, Options{FailDelay: time.Millisecond})
		if _, err := rg.auth.CreateUser("admin", firstPassword, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		here, csrf, _ := rg.login("admin", firstPassword, rigRemote)
		for i := range auth.DefaultRateMax {
			if _, _, st := rg.login("admin", "not the password "+string(rune('a'+i)), otherRemote); st != http.StatusUnauthorized {
				t.Fatalf("failed login %d: %d", i, st)
			}
		}
		if rec := rg.change(here, csrf, rigRemote, firstPassword, secondPassword); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the form after failed logins: %d %s", rec.Code, rec.Body)
		}
		u := rg.auth.Users()[0]
		if !auth.VerifyPassword(u.Password, firstPassword) {
			t.Fatal("a refused change changed the password")
		}
	})
}

// A password is set without the current one only on the unix socket. Over
// TCP the call is refused whatever comes with it: nothing, an admin token,
// an admin's session with its CSRF token. On the socket it ends every
// session of the login and forgets the failed logins of its name.
func TestResetPasswordOnlyOnTheSocket(t *testing.T) {
	rg := newRig(t, Options{FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", firstPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	one, csrf, _ := rg.login("admin", firstPassword, rigRemote)
	two, _, _ := rg.login("admin", firstPassword, rigRemote)
	tokenPlain, tok, _ := rg.auth.CreateToken("script", auth.RoleAdmin)
	body := jsonBody(PasswordResetRequest{Password: secondPassword})
	for what, h := range map[string]map[string]string{
		"no login":         jsonHeader,
		"an admin token":   header("Content-Type", "application/json", "Authorization", "Bearer "+tokenPlain),
		"an admin session": sessionHeader(one, csrf, rigRemote),
	} {
		rec := rg.do("PUT", "/api/v1/users/admin/password", body, h)
		if rec.Code != http.StatusForbidden || errCode(rec) != "socket_only" {
			t.Fatalf("over TCP with %s: %d %s", what, rec.Code, rec.Body)
		}
	}
	if !auth.VerifyPassword(rg.auth.Users()[0].Password, firstPassword) || !rg.open(one) || !rg.open(two) {
		t.Fatal("a refused reset changed something")
	}
	// Failed logins of the name, from an address of their own.
	for i := range auth.DefaultRateMax {
		if _, _, st := rg.login("admin", "not the password "+string(rune('a'+i)), otherRemote); st != http.StatusUnauthorized {
			t.Fatalf("failed login %d: %d", i, st)
		}
	}

	if rec := rg.socket("PUT", "/api/v1/users/admin/password", jsonBody(PasswordResetRequest{Password: weakPassword})); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a weak password on the socket: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.socket("PUT", "/api/v1/users/nobody/password", body); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "nobody") {
		t.Fatalf("an unknown login on the socket: %d %s", rec.Code, rec.Body)
	}
	rec := rg.socket("PUT", "/api/v1/users/ADMIN/password", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset on the socket: %d %s", rec.Code, rec.Body)
	}
	var res PasswordResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Username != "admin" || res.EndedSessions != 2 || strings.Contains(rec.Body.String(), secondPassword) {
		t.Fatalf("reset answered %s", rec.Body)
	}
	if rg.open(one) || rg.open(two) {
		t.Fatal("a session lives on after the reset")
	}
	if _, _, st := rg.login("admin", firstPassword, rigRemote); st != http.StatusUnauthorized {
		t.Fatalf("the old password after the reset: %d", st)
	}
	if _, _, st := rg.login("admin", secondPassword, rigRemote); st != http.StatusOK {
		t.Fatalf("the new password at once after the reset: %d", st)
	}

	entries := rg.entries("user.password.reset")
	if len(entries) != 6 {
		t.Fatalf("%d reset entries, want 6: %+v", len(entries), entries)
	}
	who := map[string]bool{}
	for _, e := range entries[:3] {
		if e.OK || e.Target != "" {
			t.Errorf("a reset over TCP: %+v", e)
		}
		who[e.Actor.Kind+" "+e.Actor.ID] = true
	}
	if !who[audit.ActorAnonymous+" 192.0.2.1"] || !who[audit.ActorToken+" "+tok.ID] || !who[audit.ActorUser+" admin"] {
		t.Fatalf("who tried over TCP: %v", who)
	}
	if e := entries[4]; e.OK || e.Target != auth.UnknownName {
		t.Fatalf("the entry of an unknown login: %+v", e)
	}
	if e := entries[5]; !e.OK || e.Actor.Kind != audit.ActorSocket || e.Target != "admin" {
		t.Fatalf("the entry of the reset: %+v", e)
	}
	rg.noPasswords(firstPassword, secondPassword, weakPassword)
}

// GET /users lists the logins for admins: name, role, last login and open
// sessions, never a password hash.
func TestListUsers(t *testing.T) {
	rg := newRig(t, Options{FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", firstPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin, _, _ := rg.auth.CreateToken("a", auth.RoleAdmin)
	read, _, _ := rg.auth.CreateToken("r", auth.RoleRead)
	list := func() (UserList, string) {
		rec := rg.do("GET", "/api/v1/users", "", header("Authorization", "Bearer "+admin))
		rg.status(rec, http.StatusOK)
		var ul UserList
		json.Unmarshal(rec.Body.Bytes(), &ul)
		return ul, rec.Body.String()
	}
	ul, raw := list()
	if len(ul.Users) != 1 || ul.Users[0].Username != "admin" || ul.Users[0].Role != "admin" || !ul.Users[0].LastLogin.IsZero() ||
		ul.Users[0].Sessions != 0 || strings.Contains(raw, "last_login") {
		t.Fatalf("before a login: %s", raw)
	}
	rg.login("admin", firstPassword, rigRemote)
	rg.login("admin", firstPassword, rigRemote)
	ul, raw = list()
	if ul.Users[0].LastLogin.IsZero() || ul.Users[0].Sessions != 2 {
		t.Fatalf("after two logins: %s", raw)
	}
	for _, bad := range []string{"hash", "salt", "pbkdf2", "password"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("the list has %q: %s", bad, raw)
		}
	}
	if rec := rg.do("GET", "/api/v1/users", "", header("Authorization", "Bearer "+read)); rec.Code != http.StatusForbidden {
		t.Fatalf("with a read token: %d", rec.Code)
	}
}
