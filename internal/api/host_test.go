package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The TCP listener answers only requests that name one of ESM's own hosts:
// loopback names with the listen port, the listen address itself, and the
// public_hosts entries. Anything else, the page included, gets 421, so a
// page of another site that rebinds its own name to 127.0.0.1 reaches
// nothing. The unix socket is not checked.
func TestHostCheck(t *testing.T) {
	s := New(Options{Listen: "127.0.0.1:8740", PublicHosts: []string{"esm.example.com", "Admin.Example.org:8443"}})
	allowed := []string{
		"localhost:8740", "LOCALHOST:8740", "127.0.0.1:8740", "[::1]:8740",
		"esm.example.com", "esm.example.com:443", "ESM.example.com.", "admin.example.org:8443",
	}
	refused := []string{
		"evil.example:8740", "evil.example", "localhost:8741", "localhost", "127.0.0.2:8740",
		"admin.example.org", "admin.example.org:443", "", "127.0.0.1", "[::1]",
	}
	for _, path := range []string{"/api/v1/nothing", "/", "/instances/alpha"} {
		for _, host := range allowed {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Host = host
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, r)
			if rec.Code == http.StatusMisdirectedRequest {
				t.Errorf("%s with Host %q refused", path, host)
			}
		}
		for _, host := range refused {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Host = host
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, r)
			var eb ErrorBody
			json.Unmarshal(rec.Body.Bytes(), &eb)
			if rec.Code != http.StatusMisdirectedRequest || eb.Error.Code != "misdirected_request" {
				t.Errorf("%s with Host %q: %d %s, want 421 misdirected_request", path, host, rec.Code, rec.Body)
			}
			if rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("%s with Host %q: no security headers", path, host)
			}
			// The socket does not check.
			rec = httptest.NewRecorder()
			s.SocketHandler().ServeHTTP(rec, r)
			if rec.Code == http.StatusMisdirectedRequest {
				t.Errorf("socket: %s with Host %q refused", path, host)
			}
		}
	}
	// A listener on a specific address also answers to that address; one
	// on all interfaces does not answer to an all-interfaces host.
	for listen, host := range map[string]string{"192.0.2.10:8740": "192.0.2.10:8740", "[2001:db8::5]:443": "[2001:db8::5]"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/nothing", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		New(Options{Listen: listen, TLS: true}).Handler().ServeHTTP(rec, r)
		if rec.Code == http.StatusMisdirectedRequest {
			t.Errorf("listen %s: Host %q refused", listen, host)
		}
	}
	const allInterfaces = "0.0.0.0:8740"
	r := httptest.NewRequest(http.MethodGet, "/api/v1/nothing", nil)
	r.Host = allInterfaces
	rec := httptest.NewRecorder()
	New(Options{Listen: allInterfaces, TLS: true}).Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("Host %s answered %d", allInterfaces, rec.Code)
	}
}
