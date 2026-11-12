package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{}).Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestPageHeadersAndFallback(t *testing.T) {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	for _, p := range []string{"/", "/style.css", "/instances/alpha", "/dashboard"} {
		rec := serve(t, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: %d", p, rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Security-Policy") != csp {
			t.Errorf("GET %s: CSP %q", p, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("GET %s: headers %v", p, h)
		}
	}
	if rec := serve(t, http.MethodHead, "/"); rec.Code != http.StatusOK {
		t.Errorf("HEAD /: %d", rec.Code)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/api/v1/nothing", http.StatusNotFound, "not_found"},
		{"GET", "/api/", http.StatusNotFound, "not_found"},
		{"POST", "/api/v2/instances", http.StatusNotFound, "not_found"},
		{"DELETE", "/api/v1/instances", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"GET", "/api/v1/instances/alpha/start", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"POST", "/somewhere", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"GET", "/api/v1/instances", http.StatusUnauthorized, "unauthorized"},
	}
	for _, c := range cases {
		rec := serve(t, c.method, c.path)
		var eb ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
			t.Errorf("%s %s: body is not the error shape: %q", c.method, c.path, rec.Body)
			continue
		}
		if rec.Code != c.status || eb.Error.Code != c.code || eb.Error.Message == "" {
			t.Errorf("%s %s: %d %+v, want %d %s", c.method, c.path, rec.Code, eb, c.status, c.code)
		}
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s: content type %q", c.method, c.path, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s %s: no CSP", c.method, c.path)
		}
	}
	rec := serve(t, "DELETE", "/api/v1/instances/alpha/start")
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Errorf("Allow %q", allow)
	}
}

func TestRolesOfRoutes(t *testing.T) {
	for _, r := range Routes() {
		switch {
		case r.Path == "/api/v1/tokens" || strings.HasPrefix(r.Path, "/api/v1/tokens/") || r.Path == "/api/v1/audit":
			if r.Role != "admin" {
				t.Errorf("%s %s needs admin, has %q", r.Method, r.Path, r.Role)
			}
		case r.Path == "/api/v1/instances/{name}/backups/{id}/download":
			// A backup is a copy of the whole world: operators and admins.
			if r.Role != "operator" {
				t.Errorf("%s %s needs operator, has %q", r.Method, r.Path, r.Role)
			}
		case r.Method == "GET" && r.Path != "/api/v1/setup":
			if r.Role != "read" {
				t.Errorf("%s %s should be readable with the read role, has %q", r.Method, r.Path, r.Role)
			}
		case r.Role == "":
			if !oneOf(r.Path, "/api/v1/setup", "/api/v1/login", "/api/v1/logout") {
				t.Errorf("%s %s needs no login", r.Method, r.Path)
			}
		case r.Role == "read":
			t.Errorf("%s %s changes things but allows the read role", r.Method, r.Path)
		}
	}
}

func oneOf(s string, list ...string) bool {
	for _, x := range list {
		if s == x {
			return true
		}
	}
	return false
}
