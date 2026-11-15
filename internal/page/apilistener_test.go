package page_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// The Box screen shows the API listener: its address, what it takes, its
// host names, its certificate with the file, the SHA-256 fingerprint and
// the public key pin, to every role that sees the screen (GET /box shows
// them to the read role too). Without api_listener it says it is off.
func TestBoxShowsTheAPIListener(t *testing.T) {
	s := newSiteConfig(t, map[string]any{"api_listener": map[string]any{"listen": "127.0.0.1:0", "host_names": []string{"esm.example.com"}}})
	s.setup()
	info := s.svc.APIListener()
	c := info.Certificate
	read := s.token("read")
	for _, who := range []string{"session", read} {
		// The page writes "+" as "&#43;", which a browser shows as "+".
		body := html.UnescapeString(s.checkPage("/box", who, map[bool]string{true: s.csrf, false: ""}[who == "session"]))
		for _, want := range []string{`id="apilisten-h"`, info.Listen + " with TLS", "API tokens only", "esm.example.com",
			"made by ESM", c.SHA256, c.SPKIPin, c.File, "esm tls", "--pinnedpubkey"} {
			if !strings.Contains(body, want) {
				t.Errorf("/box as %.7s does not show %q", who, want)
			}
		}
	}

	off := newSite(t)
	off.setup()
	body := off.checkPage("/box", "session", off.csrf)
	if !strings.Contains(body, "off (api_listener)") || strings.Contains(body, `id="apilisten-h"`) {
		t.Errorf("/box without an API listener:\n%s", body)
	}
}

// tlsClient trusts the API listener by its public key pin and follows no
// redirects.
func tlsClient(pin string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
			if tlscert.SPKIPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
				return errors.New("wrong pin")
			}
			return nil
		}}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       20 * time.Second,
	}
}

// With "page": true the page works on the API listener as on the main
// one, over https: the login cookie is Secure, every screen is served to
// the session, a change needs the CSRF token and an https Origin. Without
// it the API listener serves no page and takes no login, and the page on
// the main listener is unchanged.
func TestPageOnTheAPIListener(t *testing.T) {
	for _, page := range []bool{true, false} {
		s := newSiteConfig(t, map[string]any{"api_listener": map[string]any{"listen": "127.0.0.1:0", "page": page}})
		s.setup()
		info := s.svc.APIListener()
		hc := tlsClient(info.Certificate.SPKIPin)
		base := "https://" + info.Listen
		send := func(method, path, cookie, csrf, origin string, body any) (*http.Response, string) {
			t.Helper()
			var r io.Reader
			if body != nil {
				b, _ := json.Marshal(body)
				r = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(method, base+path, r)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			if cookie != "" {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookie})
			}
			if csrf != "" {
				req.Header.Set(auth.CSRFHeader, csrf)
			}
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, string(b)
		}
		login := api.LoginRequest{Username: "admin", Password: "a long password 1"}
		resp, body := send("POST", "/api/v1/login", "", "", base, login)
		if !page {
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, api.TokensOnly) || len(resp.Cookies()) != 0 {
				t.Fatalf("login without the page: %d %s", resp.StatusCode, body)
			}
			if resp, body := send("GET", "/", "", "", "", nil); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "serves no web page") {
				t.Fatalf("page without the page: %d %s", resp.StatusCode, body)
			}
			if resp, _ := send("GET", "/box", s.session, "", "", nil); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("box without the page: %d", resp.StatusCode)
			}
			// The main listener's page is as it was.
			s.checkPage("/box", "session", s.csrf)
			continue
		}
		if resp.StatusCode != http.StatusOK || len(resp.Cookies()) != 1 || !resp.Cookies()[0].Secure {
			t.Fatalf("login: %d %s %+v", resp.StatusCode, body, resp.Cookies())
		}
		var lr api.LoginResult
		json.Unmarshal([]byte(body), &lr)
		cookie := resp.Cookies()[0].Value
		for _, path := range []string{"/", "/box", "/servers", "/builds", "/audit"} {
			resp, body := send("GET", path, cookie, "", "", nil)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
				t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
			}
			checkHeaders(t, path, resp.Header)
			checkHTML(t, path+" on the API listener", body, lr.CSRFToken)
		}
		// Without a login, a screen sends to the login form, which is there.
		if resp, _ := send("GET", "/box", "", "", "", nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?next=%2Fbox" {
			t.Fatalf("box without a login: %d %v", resp.StatusCode, resp.Header)
		}
		if resp, body := send("GET", "/setup", "", "", "", nil); resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-form="login"`) {
			t.Fatalf("login form: %d", resp.StatusCode)
		}
		req := api.TokenRequest{Name: "panel", Role: "read"}
		if resp, _ := send("POST", "/api/v1/tokens", cookie, "", base, req); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("change without CSRF: %d", resp.StatusCode)
		}
		if resp, _ := send("POST", "/api/v1/tokens", cookie, lr.CSRFToken, "http://"+info.Listen, req); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("change from an http origin: %d", resp.StatusCode)
		}
		if resp, body := send("POST", "/api/v1/tokens", cookie, lr.CSRFToken, base, req); resp.StatusCode != http.StatusCreated {
			t.Fatalf("change: %d %s", resp.StatusCode, body)
		}
	}
}

// An address with too many failed authentications still gets to the login
// and logs in: the page takes the 429 of the limit as "not logged in", and
// the login has its own limit only. With the session, every screen works.
func TestLoginWorksFromALimitedAddress(t *testing.T) {
	s := newSite(t)
	s.setup()
	for range api.AuthFailMax + 5 {
		s.api("GET", "/api/v1/session", "esm_"+strings.Repeat("w", 43), nil, nil)
	}
	if st := s.api("GET", "/api/v1/session", "", nil, nil); st != http.StatusTooManyRequests {
		t.Fatalf("the address is not limited: %d", st)
	}
	resp, _ := s.get("/box", "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?next=%2Fbox" {
		t.Fatalf("a screen without a login: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, body := s.get("/setup?next=%2Fbox", ""); resp.StatusCode != http.StatusOK || !strings.Contains(body, `data-form="login"`) {
		t.Fatalf("the login form: %d", resp.StatusCode)
	}
	var lr api.LoginResult
	s.must(http.StatusOK, "POST", "/api/v1/login", "", api.LoginRequest{Username: "admin", Password: "a long password 1"}, &lr)
	s.csrf = lr.CSRFToken
	s.checkPage("/box", "session", s.csrf)
}
