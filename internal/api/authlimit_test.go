package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
)

// clock is a test clock for the limits.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// actions lists the actions of the audit log, in order.
func (r *rig) actions() []string {
	r.t.Helper()
	entries, err := r.audit.Recent(1000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

// countEvents counts the audit events that arrive within a short time.
func countEvents(sub *events.Subscription) int {
	n := 0
	for {
		select {
		case <-sub.C:
			n++
		case <-time.After(300 * time.Millisecond):
			return n
		}
	}
}

// A flood of calls with a wrong token from one address is answered 429
// after AuthFailMax failures, and the audit log (and the event stream)
// gets two entries of it: the first failure, and that the address is
// limited. While it is limited, a valid token from the same address works,
// and so does the login, which has a limit of its own; a call without a
// valid login is answered 429 before anything else. Other addresses are not
// touched, an IPv6 address counts by its /64, and after the window the
// address is answered as before.
func TestFailedAuthenticationsAreLimited(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	rg := newRig(t, Options{Now: ck.now, FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin, _, _ := rg.auth.CreateToken("panel", auth.RoleAdmin)
	sub := rg.bus.Subscribe(events.Filter{Types: []string{events.TypeAudit}}, 4096)
	from := func(addr string, kv ...string) map[string]string {
		return header(append([]string{"remote", addr}, kv...)...)
	}
	wrong := "Bearer esm_" + strings.Repeat("x", 43)

	first429 := 0
	for i := 1; i <= 3*AuthFailMax; i++ {
		rec := rg.do("POST", "/api/v1/instances/x/start", "{}", from("192.0.2.7:1234", "Authorization", wrong))
		switch {
		case rec.Code == http.StatusUnauthorized && first429 == 0:
		case rec.Code == http.StatusTooManyRequests:
			if first429 == 0 {
				first429 = i
			}
			if errorCode(rec) != "rate_limited" || rec.Header().Get("Retry-After") == "" {
				t.Fatalf("call %d: %d %s %v", i, rec.Code, rec.Body, rec.Header())
			}
		default:
			t.Fatalf("call %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if first429 != AuthFailMax+1 {
		t.Fatalf("the first 429 came with call %d, want %d", first429, AuthFailMax+1)
	}
	got := rg.actions()
	if len(got) != 2 || got[0] != "instance.start" || got[1] != "auth.limited" {
		t.Fatalf("audit log after the flood: %q", got)
	}
	if log := rg.auditLog(); !strings.Contains(log, fmt.Sprintf("%d failed authentications from 192.0.2.7", AuthFailMax)) {
		t.Fatalf("audit log:\n%s", log)
	}
	if n := countEvents(sub); n != 2 {
		t.Fatalf("%d audit events for the flood, want 2", n)
	}

	// Without credentials, with another kind of Authorization, with a
	// session that does not exist: 429, nothing recorded.
	for _, h := range []map[string]string{from("192.0.2.7:1"), from("192.0.2.7:1", "Authorization", "Basic eDp5"),
		from("192.0.2.7:1", "Cookie", auth.SessionCookieName+"=nothing")} {
		for _, c := range []struct{ method, path string }{{"GET", "/api/v1/session"}, {"POST", "/api/v1/tokens"},
			{"PUT", "/api/v1/users/admin/password"}} {
			if rec := rg.do(c.method, c.path, "{}", h); rec.Code != http.StatusTooManyRequests {
				t.Errorf("%s %s with %v while limited: %d %s", c.method, c.path, h, rec.Code, rec.Body)
			}
		}
	}
	// A valid token works.
	if rec := rg.do("GET", "/api/v1/session", "", from("192.0.2.7:1", "Authorization", "Bearer "+admin)); rec.Code != http.StatusOK {
		t.Fatalf("a valid token while limited: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.do("POST", "/api/v1/tokens", `{"name":"x","role":"read"}`, from("192.0.2.7:1", "Authorization", "Bearer "+admin)); rec.Code != http.StatusCreated {
		t.Fatalf("a change with a valid token while limited: %d %s", rec.Code, rec.Body)
	}
	// The login works, and a session from it too.
	rec := rg.do("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, from("192.0.2.7:1", "Content-Type", "application/json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login while limited: %d %s", rec.Code, rec.Body)
	}
	sess := rec.Result().Cookies()[0].Value
	if rec := rg.do("GET", "/api/v1/session", "", from("192.0.2.7:1", "Cookie", auth.SessionCookieName+"="+sess)); rec.Code != http.StatusOK {
		t.Fatalf("a session while limited: %d %s", rec.Code, rec.Body)
	}
	if got := rg.actions(); len(got) != 4 || got[2] != "token.create" || got[3] != "login" {
		t.Fatalf("audit log: %q", got)
	}
	// Other addresses are answered as always.
	if rec := rg.do("POST", "/api/v1/instances/x/start", "{}", from("192.0.2.8:1", "Authorization", wrong)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another address: %d %s", rec.Code, rec.Body)
	}
	// The socket is never limited.
	for range 2 * AuthFailMax {
		rec := httptest.NewRecorder()
		rg.s.SocketHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/session", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("socket: %d", rec.Code)
		}
	}

	// An IPv6 address counts by its /64.
	for i := range AuthFailMax {
		rg.do("GET", "/api/v1/session", "", from(fmt.Sprintf("[2001:db8::%x]:1", i+1), "Authorization", wrong))
	}
	if rec := rg.do("GET", "/api/v1/session", "", from("[2001:db8::ffff]:1", "Authorization", wrong)); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the same /64: %d", rec.Code)
	}
	if rec := rg.do("GET", "/api/v1/session", "", from("[2001:db8:0:1::1]:1", "Authorization", wrong)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another /64: %d", rec.Code)
	}

	// After the window, as before: the first failure is recorded again.
	ck.add(AuthFailWindow + time.Second)
	before := len(rg.actions())
	if rec := rg.do("POST", "/api/v1/instances/x/start", "{}", from("192.0.2.7:1", "Authorization", wrong)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after the window: %d %s", rec.Code, rec.Body)
	}
	if got := rg.actions(); len(got) != before+1 || got[len(got)-1] != "instance.start" {
		t.Fatalf("audit log after the window: %q", got)
	}
}

// What is recorded of calls without a valid login is bounded per address:
// AnonAuditMax entries a window, then one that says so; also for calls
// that the failure limit does not count (failed logins, the login's own
// limit, logouts without a session, changes to unknown paths, setup), so
// none of them fills the disk. A login that works is recorded all the
// same.
func TestUnauthenticatedCallsAreRecordedWithinLimits(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	rg := newRig(t, Options{Now: ck.now, FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	sub := rg.bus.Subscribe(events.Filter{Types: []string{events.TypeAudit}}, 4096)
	h := header("remote", "192.0.2.9:1", "Content-Type", "application/json")
	for range 40 {
		rg.do("POST", "/api/v1/logout", "{}", h)
		rg.do("POST", "/api/v1/nothing", "{}", h)
		rg.do("POST", "/api/v1/login", `{"username":"made-up","password":"wrong password!!"}`, h)
		rg.do("POST", "/api/v1/setup", `{"code":"x","username":"a","password":"a long password 1"}`, h)
		rg.do("POST", "/api/v1/login", `not json`, header("remote", "192.0.2.9:1"))
	}
	got := rg.actions()
	if len(got) != AnonAuditMax+1 || got[AnonAuditMax] != "audit.quiet" {
		t.Fatalf("%d entries for 200 calls without a login: %q", len(got), got)
	}
	if n := countEvents(sub); n != AnonAuditMax+1 {
		t.Fatalf("%d audit events, want %d", n, AnonAuditMax+1)
	}
	// Another address runs out with calls the login limit does not count;
	// a login from it that works is recorded all the same.
	h2 := header("remote", "192.0.2.10:1", "Content-Type", "application/json")
	for range 2 * AnonAuditMax {
		rg.do("POST", "/api/v1/logout", "{}", h2)
	}
	if rec := rg.do("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, h2); rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	got = rg.actions()
	if len(got) != 2*(AnonAuditMax+1)+1 || got[len(got)-2] != "audit.quiet" || got[len(got)-1] != "login" {
		t.Fatalf("the login is not recorded: %q", got)
	}
	// After the window, the first address is recorded again.
	ck.add(AuthFailWindow + time.Second)
	rg.do("POST", "/api/v1/logout", "{}", h)
	if got := rg.actions(); len(got) != 2*(AnonAuditMax+1)+2 || got[len(got)-1] != "logout" {
		t.Fatalf("after the window: %q", got)
	}
}

// On the API listener a 401 and a 429 end the connection (Connection:
// close), so that one connection cannot go on trying; other answers, and
// every answer of the main listener, keep it.
func TestAPIListenerClosesAfterAFailedAuthentication(t *testing.T) {
	rg := apiRig(t, false, nil)
	admin, _, _ := rg.auth.CreateToken("panel", auth.RoleAdmin)
	wrong := bearer("esm_" + strings.Repeat("y", 43))
	if rec := rg.onAPI("GET", "/api/v1/session", "", wrong); rec.Code != http.StatusUnauthorized || rec.Header().Get("Connection") != "close" {
		t.Fatalf("401: %d %v", rec.Code, rec.Header())
	}
	if rec := rg.onAPI("GET", "/api/v1/session", "", header("Cookie", auth.SessionCookieName+"=x")); rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("Connection") != "close" {
		t.Fatalf("401 tokens_only: %d %v", rec.Code, rec.Header())
	}
	for range AuthFailMax {
		rg.onAPI("GET", "/api/v1/session", "", wrong)
	}
	if rec := rg.onAPI("GET", "/api/v1/session", "", wrong); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Connection") != "close" {
		t.Fatalf("429: %d %v", rec.Code, rec.Header())
	}
	if rec := rg.onAPI("GET", "/api/v1/session", "", bearer(admin)); rec.Code != http.StatusOK || rec.Header().Get("Connection") != "" {
		t.Fatalf("200: %d %v", rec.Code, rec.Header())
	}
	if rec := rg.onAPI("GET", "/api/v1/nothing", "", bearer(admin)); rec.Code != http.StatusNotFound || rec.Header().Get("Connection") != "" {
		t.Fatalf("404: %d %v", rec.Code, rec.Header())
	}
	if rec := rg.do("GET", "/api/v1/session", "", header("remote", "192.0.2.50:1", "host", "127.0.0.1:8740", "Authorization", "Bearer esm_wrong")); rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("Connection") != "" {
		t.Fatalf("main listener 401: %d %v", rec.Code, rec.Header())
	}
	// A failure recorded on one listener counts on the other: the limit is
	// the address's.
	if rec := rg.do("GET", "/api/v1/session", "", header("host", "127.0.0.1:8740", "Authorization", "Bearer esm_wrong")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("main listener, same address: %d", rec.Code)
	}
}

// A call with a valid token or session never touches the limits of
// callers without a valid login: not even their lock, which a flood of
// failures from many addresses holds busy. The limits' clock counts every
// use of them.
func TestValidLoginsLeaveTheLimitsAlone(t *testing.T) {
	var uses atomic.Int64
	now := func() time.Time { uses.Add(1); return time.Now() }
	rg := newRig(t, Options{Now: now, FailDelay: time.Millisecond})
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin, _, _ := rg.auth.CreateToken("panel", auth.RoleAdmin)
	sess, err := rg.auth.Login("", "admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	valid := func() {
		t.Helper()
		before := uses.Load()
		for _, c := range []struct {
			method, path, body string
			h                  map[string]string
			want               int
		}{
			{"GET", "/api/v1/session", "", bearer(admin), http.StatusOK},
			{"POST", "/api/v1/tokens", `{"name":"x","role":"read"}`, bearer(admin), http.StatusCreated},
			{"GET", "/api/v1/tokens", "", header("Cookie", auth.SessionCookieName+"="+sess.ID), http.StatusOK},
			{"POST", "/api/v1/tokens", `{"name":"y","role":"read"}`, header("Cookie", auth.SessionCookieName+"="+sess.ID, "X-CSRF-Token", sess.CSRF), http.StatusCreated},
			{"PUT", "/api/v1/users/admin/password", `{"password":"another long password"}`, bearer(admin), http.StatusForbidden},
		} {
			if rec := rg.do(c.method, c.path, c.body, c.h); rec.Code != c.want {
				t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
			}
		}
		if n := uses.Load() - before; n != 0 {
			t.Fatalf("calls with a valid login used the limits %d times", n)
		}
	}
	valid()
	// A wrong token does use them.
	before := uses.Load()
	rg.do("GET", "/api/v1/session", "", bearer("esm_wrong"))
	if uses.Load() == before {
		t.Fatal("the limits' clock is not used: the test sees nothing")
	}
	// And while the address is limited, still nothing.
	for range AuthFailMax {
		rg.do("GET", "/api/v1/session", "", bearer("esm_wrong"))
	}
	if rec := rg.do("GET", "/api/v1/session", "", bearer("esm_wrong")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("not limited: %d", rec.Code)
	}
	valid()
}
