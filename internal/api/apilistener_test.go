package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
)

const apiHost = "192.0.2.10:8743"

// onAPI sends a request to the API listener's handler, for apiHost, from
// 192.0.2.1:1234 unless remote says otherwise.
func (r *rig) onAPI(method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = apiHost
	for k, v := range hdr {
		switch k {
		case "remote":
			req.RemoteAddr = v
		case "host":
			req.Host = v
		default:
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	r.s.APIListenerHandler().ServeHTTP(rec, req)
	return rec
}

// errorCode is the code of an error answer ("" for none).
func errorCode(rec *httptest.ResponseRecorder) string {
	var eb ErrorBody
	json.Unmarshal(rec.Body.Bytes(), &eb)
	return eb.Error.Code
}

func apiRig(t *testing.T, page bool, cfg *config.Config) *rig {
	t.Helper()
	return newRig(t, Options{Page: stubPage, Config: cfg, FailDelay: time.Millisecond, Listen: "127.0.0.1:8740",
		APIListener: &APIListenerInfo{Listen: apiHost, HostNames: []string{"esm.example.com", "panel.example.org:8443"}, Page: page}})
}

func bearer(tok string) map[string]string { return header("Authorization", "Bearer "+tok) }

// Without page, the API listener takes API tokens only, each with its role:
// a read token reads and changes nothing, an admin token does what an admin
// does. A session cookie is refused (401 tokens_only) even when it is a
// valid admin session, and so are login, setup and logout (403
// tokens_only); the page is not served there (404). A call answered only on
// the unix socket stays refused, and a call of browser sessions only
// refuses a token as on the main listener.
func TestAPIListenerTakesTokensOnly(t *testing.T) {
	rg := apiRig(t, false, nil)
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	read, _, _ := rg.auth.CreateToken("reader", auth.RoleRead)
	admin, _, _ := rg.auth.CreateToken("panel", auth.RoleAdmin)
	sess, err := rg.auth.Login("", "admin", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	cookie := header("Cookie", auth.SessionCookieName+"="+sess.ID, "X-CSRF-Token", sess.CSRF, "Content-Type", "application/json")

	// Tokens, with their roles.
	rec := rg.onAPI("GET", "/api/v1/session", "", bearer(read))
	var si SessionInfo
	json.Unmarshal(rec.Body.Bytes(), &si)
	if rec.Code != http.StatusOK || si.Kind != "token" || si.Role != "read" {
		t.Fatalf("read token: %d %s", rec.Code, rec.Body)
	}
	newToken := `{"name":"x","role":"read"}`
	if rec := rg.onAPI("POST", "/api/v1/tokens", newToken, bearer(read)); rec.Code != http.StatusForbidden || errorCode(rec) != "forbidden" {
		t.Fatalf("read token made a token: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("GET", "/api/v1/tokens", "", bearer(read)); rec.Code != http.StatusForbidden {
		t.Fatalf("read token listed the tokens: %d", rec.Code)
	}
	if rec := rg.onAPI("POST", "/api/v1/tokens", newToken, bearer(admin)); rec.Code != http.StatusCreated {
		t.Fatalf("admin token: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("GET", "/api/v1/audit", "", bearer(admin)); rec.Code != http.StatusOK {
		t.Fatalf("admin token, audit: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("GET", "/api/v1/session", "", nil); rec.Code != http.StatusUnauthorized || errorCode(rec) != "unauthorized" {
		t.Fatalf("no login: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("GET", "/api/v1/session", "", bearer("esm_wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d %s", rec.Code, rec.Body)
	}

	// The same session works on the main listener, and not here.
	if rec := rg.do("GET", "/api/v1/session", "", header("Cookie", auth.SessionCookieName+"="+sess.ID, "host", "127.0.0.1:8740")); rec.Code != http.StatusOK {
		t.Fatalf("the session on the main listener: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/session", ""},
		{"GET", "/api/v1/tokens", ""},
		{"POST", "/api/v1/tokens", newToken},
		{"POST", "/api/v1/session/password", `{"current_password":"correct horse battery","new_password":"another long password"}`},
	} {
		rec := rg.onAPI(c.method, c.path, c.body, cookie)
		if rec.Code != http.StatusUnauthorized || errorCode(rec) != TokensOnly || !strings.Contains(rec.Body.String(), "Bearer") {
			t.Errorf("%s %s with a session cookie: %d %s, want 401 tokens_only", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if _, err := rg.auth.Session(sess.ID); err != nil {
		t.Fatalf("the session ended: %v", err)
	}
	// A token with the cookie is the token.
	h := bearer(read)
	h["Cookie"] = auth.SessionCookieName + "=" + sess.ID
	if rec := rg.onAPI("GET", "/api/v1/session", "", h); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kind":"token"`) {
		t.Fatalf("token and cookie: %d %s", rec.Code, rec.Body)
	}

	// Logging in and out, and setup, are not answered.
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`},
		{"POST", "/api/v1/setup", `{"code":"x","username":"a","password":"a long password 1"}`},
		{"GET", "/api/v1/setup", ""},
		{"POST", "/api/v1/logout", "{}"},
	} {
		for _, hd := range []map[string]string{jsonHeader, bearer(admin)} {
			rec := rg.onAPI(c.method, c.path, c.body, hd)
			if rec.Code != http.StatusForbidden || errorCode(rec) != TokensOnly || len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s %s: %d %s, want 403 tokens_only", c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	if _, err := rg.auth.Session(sess.ID); err != nil {
		t.Fatalf("a logout on the API listener ended the session: %v", err)
	}

	// No page.
	for _, path := range []string{"/", "/instances/alpha", "/style.css", "/setup"} {
		rec := rg.onAPI("GET", path, "", bearer(admin))
		if rec.Code != http.StatusNotFound || errorCode(rec) != "not_found" || strings.Contains(rec.Body.String(), "<") ||
			rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s: %d %s, want the 404 of no page", path, rec.Code, rec.Body)
		}
	}
	// The page is there on the main listener.
	if rec := rg.do("GET", "/", "", header("host", "127.0.0.1:8740")); rec.Code != http.StatusOK {
		t.Fatalf("the page on the main listener: %d", rec.Code)
	}

	// Socket only stays socket only; session only stays session only.
	rec = rg.onAPI("PUT", "/api/v1/users/admin/password", `{"password":"another long password"}`, bearer(admin))
	if rec.Code != http.StatusForbidden || errorCode(rec) != "socket_only" {
		t.Fatalf("socket-only call: %d %s", rec.Code, rec.Body)
	}
	rec = rg.onAPI("POST", "/api/v1/session/password", `{"current_password":"correct horse battery","new_password":"another long password"}`, bearer(admin))
	if rec.Code != http.StatusForbidden || errorCode(rec) != "session_only" {
		t.Fatalf("session-only call: %d %s", rec.Code, rec.Body)
	}
	// Unknown paths and methods answer as anywhere.
	if rec := rg.onAPI("GET", "/api/v1/nothing", "", bearer(admin)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
	if rec := rg.onAPI("DELETE", "/api/v1/tokens", "", bearer(admin)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method: %d", rec.Code)
	}
	// What was refused there for lack of a role or of a token is in the
	// audit log as on the main listener.
	if log := rg.auditLog(); !strings.Contains(log, `"action":"token.create"`) || !strings.Contains(log, "takes API tokens only") ||
		!strings.Contains(log, "answered only on the box") || strings.Contains(log, sess.ID) {
		t.Fatalf("audit log:\n%s", log)
	}
}

// With "page": true the page, login and setup work on the API listener as
// on the main one, and it is always https: the session cookie is Secure,
// an Origin must say https, and every change needs the CSRF token.
func TestAPIListenerWithPage(t *testing.T) {
	rg := apiRig(t, true, nil)
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if rec := rg.onAPI("GET", "/", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<") {
		t.Fatalf("page: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("GET", "/api/v1/setup", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("setup status: %d %s", rec.Code, rec.Body)
	}
	login := `{"username":"admin","password":"correct horse battery"}`
	if rec := rg.onAPI("POST", "/api/v1/login", login, header("Content-Type", "application/json", "Origin", "http://"+apiHost)); rec.Code != http.StatusForbidden || errorCode(rec) != "origin" {
		t.Fatalf("login from an http origin: %d %s", rec.Code, rec.Body)
	}
	rec := rg.onAPI("POST", "/api/v1/login", login, header("Content-Type", "application/json", "Origin", "https://"+apiHost))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var lr LoginResult
	json.Unmarshal(rec.Body.Bytes(), &lr)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie %+v", cookies)
	}
	ck := auth.SessionCookieName + "=" + cookies[0].Value
	if rec := rg.onAPI("GET", "/api/v1/session", "", header("Cookie", ck)); rec.Code != http.StatusOK {
		t.Fatalf("session: %d %s", rec.Code, rec.Body)
	}
	newToken := `{"name":"x","role":"read"}`
	if rec := rg.onAPI("POST", "/api/v1/tokens", newToken, header("Cookie", ck, "Content-Type", "application/json")); rec.Code != http.StatusForbidden || errorCode(rec) != "csrf" {
		t.Fatalf("change without CSRF: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("POST", "/api/v1/tokens", newToken, header("Cookie", ck, "X-CSRF-Token", lr.CSRFToken, "Origin", "http://"+apiHost)); rec.Code != http.StatusForbidden || errorCode(rec) != "origin" {
		t.Fatalf("change from an http origin: %d %s", rec.Code, rec.Body)
	}
	if rec := rg.onAPI("POST", "/api/v1/tokens", newToken, header("Cookie", ck, "X-CSRF-Token", lr.CSRFToken, "Origin", "https://"+apiHost)); rec.Code != http.StatusCreated {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	rec = rg.onAPI("POST", "/api/v1/logout", "{}", header("Cookie", ck, "X-CSRF-Token", lr.CSRFToken))
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 || !rec.Result().Cookies()[0].Secure {
		t.Fatalf("logout: %d %+v", rec.Code, rec.Result().Cookies())
	}
	// Socket only stays socket only, also for an admin's session there.
	rec = rg.onAPI("POST", "/api/v1/login", login, jsonHeader)
	ck = auth.SessionCookieName + "=" + rec.Result().Cookies()[0].Value
	json.Unmarshal(rec.Body.Bytes(), &lr)
	rec = rg.onAPI("PUT", "/api/v1/users/admin/password", `{"password":"another long password"}`, header("Cookie", ck, "X-CSRF-Token", lr.CSRFToken))
	if rec.Code != http.StatusForbidden || errorCode(rec) != "socket_only" {
		t.Fatalf("socket-only call with a session: %d %s", rec.Code, rec.Body)
	}
}

// The API listener answers only for its host names and its listen address
// (with or without a port); any other Host, loopback names and the main
// listener's names included, gets 421 as on the main listener. The main
// listener does not take the API listener's names.
func TestAPIListenerHostCheck(t *testing.T) {
	rg := apiRig(t, true, nil)
	allowed := []string{apiHost, "192.0.2.10", "192.0.2.10:443", "esm.example.com", "ESM.Example.com.", "esm.example.com:8743",
		"panel.example.org:8443"}
	refused := []string{"", "evil.example", "evil.example:8743", "localhost:8743", "127.0.0.1:8743", "[::1]:8743",
		"localhost:8740", "127.0.0.1:8740", "192.0.2.11:8743", "panel.example.org", "panel.example.org:8743", "0.0.0.0:8743"}
	for _, path := range []string{"/api/v1/nothing", "/", "/instances/alpha"} {
		for _, host := range allowed {
			if rec := rg.onAPI("GET", path, "", header("host", host)); rec.Code == http.StatusMisdirectedRequest {
				t.Errorf("%s with Host %q refused", path, host)
			}
		}
		for _, host := range refused {
			rec := rg.onAPI("GET", path, "", header("host", host))
			if rec.Code != http.StatusMisdirectedRequest || errorCode(rec) != "misdirected_request" ||
				!strings.Contains(rec.Body.String(), "host_names in api_listener") || rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("%s with Host %q: %d %s, want 421", path, host, rec.Code, rec.Body)
			}
		}
	}
	for _, host := range []string{"esm.example.com", apiHost} {
		if rec := rg.do("GET", "/api/v1/nothing", "", header("host", host)); rec.Code != http.StatusMisdirectedRequest ||
			!strings.Contains(rec.Body.String(), "public_hosts") {
			t.Errorf("main listener with Host %q: %d %s", host, rec.Code, rec.Body)
		}
	}
	if rec := rg.do("GET", "/api/v1/nothing", "", header("host", "localhost:8740")); rec.Code != http.StatusNotFound {
		t.Errorf("main listener: %d %s", rec.Code, rec.Body)
	}

	// On every address it answers for its host names only.
	every := newRig(t, Options{APIListener: &APIListenerInfo{Listen: "0.0.0.0:8743", AllAddresses: true, HostNames: []string{"192.0.2.10"}}})
	for host, ok := range map[string]bool{"192.0.2.10:8743": true, "192.0.2.10": true, "0.0.0.0:8743": false, "192.0.2.11:8743": false} {
		req := httptest.NewRequest("GET", "/api/v1/nothing", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		every.s.APIListenerHandler().ServeHTTP(rec, req)
		if (rec.Code != http.StatusMisdirectedRequest) != ok {
			t.Errorf("every address, Host %q: %d", host, rec.Code)
		}
	}
}

// The login limit applies on the API listener (with the page on), and
// there it always counts per client address too: its clients reach it
// directly, so behind_tls_proxy, which makes the main listener count per
// user name only, does not apply to it. Made-up names from one address
// lock that address out there, and not on the main listener.
func TestAPIListenerLoginLimitPerAddress(t *testing.T) {
	cfg := config.Defaults("")
	cfg.BehindTLSProxy = true
	for _, on := range []string{"main", "api"} {
		rg := apiRig(t, true, cfg)
		if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		send := rg.do
		hd := header("Content-Type", "application/json", "remote", "198.51.100.7:5000", "host", "127.0.0.1:8740")
		if on == "api" {
			send = rg.onAPI
			hd = header("Content-Type", "application/json", "remote", "198.51.100.7:5000")
		}
		for i := range auth.DefaultRateMax {
			rec := send("POST", "/api/v1/login", fmt.Sprintf(`{"username":"made-up-%d","password":"wrong password!!"}`, i), hd)
			rg.status(rec, http.StatusUnauthorized)
		}
		rec := send("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, hd)
		switch {
		case on == "main" && rec.Code != http.StatusOK:
			t.Errorf("main listener behind a proxy: %d %s", rec.Code, rec.Body)
		case on == "api" && (rec.Code != http.StatusTooManyRequests || errorCode(rec) != "rate_limited" || rec.Header().Get("Retry-After") == ""):
			t.Errorf("API listener: %d %s, want 429 for the address", rec.Code, rec.Body)
		}
		if on == "api" {
			// Per user name as well, from any address.
			for i := range auth.DefaultRateMax {
				h := header("Content-Type", "application/json", "remote", fmt.Sprintf("198.51.100.%d:5000", 20+i))
				rg.status(rg.onAPI("POST", "/api/v1/login", `{"username":"admin","password":"wrong password!!"}`, h), http.StatusUnauthorized)
			}
			h := header("Content-Type", "application/json", "remote", "198.51.100.50:5000")
			if rec := rg.onAPI("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, h); rec.Code != http.StatusTooManyRequests {
				t.Errorf("the name's limit: %d %s", rec.Code, rec.Body)
			}
		}
	}
}
