package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

func serve(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{Page: stubPage}).Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// stubPage stands in for the web page: "/" and "/instances/..." answer
// HTML, everything else is a static file server holding one stylesheet.
func stubPage(apiHandler http.Handler) http.Handler {
	files := web.Files(fstest.MapFS{"style.css": {Data: []byte("body{}")}})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/instances/") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, "<!doctype html><p>page</p>")
			return
		}
		files.ServeHTTP(w, r)
	})
}

func TestPageHeaders(t *testing.T) {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; " +
		"font-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	for p, want := range map[string]int{"/": 200, "/style.css": 200, "/instances/alpha": 200, "/dashboard": 404} {
		rec := serve(t, http.MethodGet, p)
		if rec.Code != want {
			t.Errorf("GET %s: %d, want %d", p, rec.Code, want)
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

// Without a page "/" is a JSON 404. The page is handed the API alone: its
// calls reach the API (with the same rules), and nothing else.
func TestPageHook(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Options{}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	var eb ErrorBody
	if rec.Code != http.StatusNotFound || json.Unmarshal(rec.Body.Bytes(), &eb) != nil || eb.Error.Code != "not_found" ||
		rec.Header().Get("Content-Security-Policy") != web.CSP {
		t.Fatalf("no page: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	var got http.Handler
	h := New(Options{Page: func(apiHandler http.Handler) http.Handler {
		got = apiHandler
		return stubPage(apiHandler)
	}}).Handler()
	if got == nil {
		t.Fatal("the page was not given the API")
	}
	call := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		got.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	if rec := call("/api/v1/instances"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the page's API call without a login: %d", rec.Code)
	}
	for _, p := range []string{"/", "/style.css", "/instances/alpha", "/api"} {
		if rec := call(p); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "page") || strings.Contains(rec.Body.String(), "body{") {
			t.Fatalf("the page's API handler served %s: %d %q", p, rec.Code, rec.Body)
		}
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("{}")))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST to the page: %d %v", rec.Code, rec.Header())
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
		case r.Path == "/api/v1/box/doctor":
			// The check test-binds every free port of both ranges.
			if r.Role != "admin" {
				t.Errorf("%s %s needs admin, has %q", r.Method, r.Path, r.Role)
			}
		case r.Method == "POST" && r.Path == "/api/v1/builds":
			// A build is a program that runs with ESM's own rights.
			if r.Role != "admin" {
				t.Errorf("%s %s needs admin, has %q", r.Method, r.Path, r.Role)
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

// The health check runs one at a time: while one runs, another is
// refused with 409 instead of binding the same ports again.
func TestDoctorRunsOneAtATime(t *testing.T) {
	s := New(Options{})
	s.doctorBusy.Store(true)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				t.Fatal("a second check ran while one was running")
			}
		}()
		s.SocketHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/box/doctor", nil))
	}()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "doctor_running") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A log download reads up to 16 MiB into memory: at most two run at once,
// and a third is told to come back (429) instead of piling up.
func TestLogDownloadsAreLimited(t *testing.T) {
	s := New(Options{})
	for range cap(s.logDownloads) {
		s.logDownloads <- struct{}{}
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				t.Fatal("a third download ran while two were running")
			}
		}()
		s.SocketHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances/alpha/logs/download", nil))
	}()
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "busy") || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if cap(s.logDownloads) != 2 {
		t.Fatalf("%d downloads at once", cap(s.logDownloads))
	}
}
