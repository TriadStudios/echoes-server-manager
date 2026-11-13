package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
)

// Login and setup take a JSON body only (a form on another site cannot
// send one) and refuse a request from another origin. Both checks come
// before anything else, so nothing else is needed to answer here.
func TestLoginAndSetupNeedJSONFromTheSameOrigin(t *testing.T) {
	h := New(Options{}).Handler()
	for _, path := range []string{"/api/v1/login", "/api/v1/setup"} {
		cases := []struct {
			ctype, origin string
			status        int
			code          string
		}{
			{"", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"multipart/form-data; boundary=x", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"text/plain", "", http.StatusUnsupportedMediaType, "unsupported_media_type"},
			{"application/json", "http://evil.example", http.StatusForbidden, "origin"},
			{"application/json", "http://example.com:8741", http.StatusForbidden, "origin"},
			{"application/json", "null", http.StatusForbidden, "origin"},
			{"application/json", "https://example.com:8740", http.StatusForbidden, "origin"},
			{"text/plain", "http://evil.example", http.StatusForbidden, "origin"},
		}
		for _, c := range cases {
			r := httptest.NewRequest(http.MethodPost, "http://example.com:8740"+path, strings.NewReader(`{"username":"a","password":"b","code":"c"}`))
			if c.ctype != "" {
				r.Header.Set("Content-Type", c.ctype)
			}
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			var eb ErrorBody
			json.Unmarshal(rec.Body.Bytes(), &eb)
			if rec.Code != c.status || eb.Error.Code != c.code {
				t.Errorf("%s with %q from %q: %d %s, want %d %s", path, c.ctype, c.origin, rec.Code, rec.Body, c.status, c.code)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s with %q from %q: a cookie was set", path, c.ctype, c.origin)
			}
		}
	}
}

// A name typed at login or setup is recorded (in the audit log and its
// events) only when it names an existing user: anything else may be a
// password typed into the wrong field, or junk sent to fill the log. The
// placeholders say which.
func TestLoginAndSetupRecordOnlyRealNames(t *testing.T) {
	rg := newRig(t, Options{FailDelay: time.Millisecond})
	sub := rg.bus.Subscribe(events.Filter{Types: []string{events.TypeAudit}}, 64)
	if _, err := rg.auth.EnsureSetupCode(); err != nil {
		t.Fatal(err)
	}
	junk := "-password=" + strings.Repeat(" ", 1000) + "hunter2" + strings.Repeat("<", 100000)
	body := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	rg.status(rg.do("POST", "/api/v1/setup", body(SetupRequest{Code: "WRONG", Username: "-password=hunter2", Password: "x"}), jsonHeader), http.StatusUnauthorized)
	rg.status(rg.do("POST", "/api/v1/login", body(LoginRequest{Username: junk, Password: "x"}), jsonHeader), http.StatusUnauthorized)
	rg.status(rg.do("POST", "/api/v1/login", body(LoginRequest{Username: "hunter2hunter2", Password: "x"}), jsonHeader), http.StatusUnauthorized)
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	rg.status(rg.do("POST", "/api/v1/login", body(LoginRequest{Username: "ADMIN", Password: "wrong password!!"}), jsonHeader), http.StatusUnauthorized)
	log := rg.auditLog()
	if strings.Contains(log, "hunter2") || len(log) > 8<<10 {
		t.Fatalf("typed names are in the audit log (%d bytes): %.300s", len(log), log)
	}
	entries, err := rg.audit.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Action+" "+e.Actor.ID+" "+e.Target)
	}
	want := []string{"setup (invalid) (invalid)", "login (invalid) ", "login (unknown) ", "login admin "}
	if !slices.Equal(got, want) {
		t.Fatalf("audit entries %q, want %q", got, want)
	}
	for range want {
		select {
		case e := <-sub.C:
			if strings.Contains(string(e.Data), "hunter2") || len(e.Data) > 4<<10 {
				t.Fatalf("an audit event carries a typed name: %.200s", e.Data)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing audit event")
		}
	}
}

// A failed login answers only after a fixed pause; a good one at once.
func TestFailedLoginIsSlowed(t *testing.T) {
	const pause = 400 * time.Millisecond
	rg := newRig(t, Options{FailDelay: pause})
	if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rg.status(rg.do("POST", "/api/v1/login", `{"username":"admin","password":"wrong password!!"}`, jsonHeader), http.StatusUnauthorized)
	if d := time.Since(start); d < pause {
		t.Fatalf("a failed login answered after %s, want at least %s", d, pause)
	}
	start = time.Now()
	rg.status(rg.do("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, jsonHeader), http.StatusOK)
	if d := time.Since(start); d >= pause {
		t.Fatalf("a good login took %s", d)
	}
}

// Failed logins from loopback (a tunnel, or a proxy on the same box) count
// against the names tried, not the shared address: made-up names lock no
// one out. Behind a proxy (behind_tls_proxy) the same holds for any
// address; a client of its own address keeps the limit per address.
func TestSharedAddressLoginsLockNobodyOut(t *testing.T) {
	for _, c := range []struct {
		remote string
		proxy  bool
		locked bool
	}{
		{"127.0.0.1:5000", false, false},
		{"[::1]:5000", false, false},
		{"198.51.100.7:5000", true, false},
		{"198.51.100.7:5000", false, true},
	} {
		cfg := config.Defaults("")
		cfg.BehindTLSProxy = c.proxy
		rg := newRig(t, Options{Config: cfg, FailDelay: time.Millisecond})
		if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		h := header("Content-Type", "application/json", "remote", c.remote)
		for i := range auth.DefaultRateMax {
			rg.status(rg.do("POST", "/api/v1/login", fmt.Sprintf(`{"username":"made-up-%d","password":"wrong password!!"}`, i), h), http.StatusUnauthorized)
		}
		rec := rg.do("POST", "/api/v1/login", `{"username":"admin","password":"correct horse battery"}`, h)
		if locked := rec.Code == http.StatusTooManyRequests; locked != c.locked || (!locked && rec.Code != http.StatusOK) {
			t.Errorf("%s (proxy %v): the admin's login answered %d after made-up names", c.remote, c.proxy, rec.Code)
		}
	}
}
