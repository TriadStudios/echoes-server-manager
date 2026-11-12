package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

const adminPassword = "correct horse battery"

// rawPost posts body with the given content type and Origin (none when
// empty) and returns the answer with its body read.
func (h *harness) rawPost(path, contentType, origin, body string, cookie *http.Cookie, csrf string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.base+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(auth.CSRFHeader, csrf)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func errorCode(b []byte) string {
	var eb api.ErrorBody
	json.Unmarshal(b, &eb)
	return eb.Error.Code
}

// setCookie returns the raw Set-Cookie header for the session cookie.
func setCookie(resp *http.Response) string {
	for _, v := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(v, auth.SessionCookieName+"=") {
			return v
		}
	}
	return ""
}

// Login and setup take JSON only and refuse other origins (a request
// without Origin, as the command line and curl send, is fine). The session
// id travels only in an HttpOnly cookie whose Max-Age is the session's
// lifetime and which is Secure behind TLS, its own or a proxy's; the CSRF
// token is a separate value; logout clears the cookie.
func TestSessionCookieRules(t *testing.T) {
	t.Parallel()
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprintf("behind_tls_proxy=%v", proxy), func(t *testing.T) {
			h := newESM(t, func(cfg map[string]any) { cfg["behind_tls_proxy"] = proxy })
			m := regexp.MustCompile(`ESM setup code: ([A-Z2-7]{24})`).FindStringSubmatch(h.stderr.String())
			if m == nil {
				t.Fatal("no setup code printed")
			}
			setup := fmt.Sprintf(`{"code":%q,"username":"admin","password":%q}`, m[1], adminPassword)
			// Browsers reach ESM over https behind the proxy: the page's
			// origin is https there and http without it, and the other
			// scheme is another origin.
			hostPort := strings.TrimPrefix(h.base, "http://")
			origin, otherScheme := "http://"+hostPort, "https://"+hostPort
			if proxy {
				origin, otherScheme = otherScheme, origin
			}
			for _, c := range []struct{ ctype, origin, code string }{
				{"application/x-www-form-urlencoded", "", "unsupported_media_type"},
				{"text/plain", "", "unsupported_media_type"},
				{"", "", "unsupported_media_type"},
				{"application/json", "http://evil.example", "origin"},
				{"text/plain", "http://evil.example", "origin"},
				{"application/json", otherScheme, "origin"},
			} {
				if _, b := h.rawPost("/api/v1/setup", c.ctype, c.origin, setup, nil, ""); errorCode(b) != c.code {
					t.Fatalf("setup with %q from %q: %s, want %s", c.ctype, c.origin, b, c.code)
				}
			}
			// The refusals did not use up the setup code or the attempts.
			if resp, b := h.rawPost("/api/v1/setup", "application/json", origin, setup, nil, ""); resp.StatusCode != http.StatusCreated {
				t.Fatalf("setup from the same origin: %d %s", resp.StatusCode, b)
			}

			login := fmt.Sprintf(`{"username":"admin","password":%q}`, adminPassword)
			for _, c := range []struct{ ctype, origin, code string }{
				{"application/x-www-form-urlencoded", "", "unsupported_media_type"},
				{"text/plain", "", "unsupported_media_type"},
				{"multipart/form-data; boundary=x", "", "unsupported_media_type"},
				{"application/json", "http://evil.example", "origin"},
				{"application/json", "null", "origin"},
				{"application/json", otherScheme, "origin"},
			} {
				resp, b := h.rawPost("/api/v1/login", c.ctype, c.origin, login, nil, "")
				if errorCode(b) != c.code || setCookie(resp) != "" {
					t.Fatalf("login with %q from %q: %s %q, want %s and no cookie", c.ctype, c.origin, b, setCookie(resp), c.code)
				}
			}
			// Without Origin (command line, curl) and from the same origin.
			if resp, b := h.rawPost("/api/v1/login", "application/json; charset=utf-8", "", login, nil, ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("login without Origin: %d %s", resp.StatusCode, b)
			}
			resp, body := h.rawPost("/api/v1/login", "application/json", origin, login, nil, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("login: %d %s", resp.StatusCode, body)
			}
			raw := setCookie(resp)
			var cookie *http.Cookie
			for _, c := range resp.Cookies() {
				if c.Name == auth.SessionCookieName {
					cookie = c
				}
			}
			if cookie == nil || cookie.Value == "" {
				t.Fatalf("no session cookie: %q", raw)
			}
			want := []string{"Path=/", "Max-Age=43200", "HttpOnly", "SameSite=Strict"}
			if proxy {
				want = append(want, "Secure")
			}
			for _, w := range want {
				if !strings.Contains(raw, "; "+w) {
					t.Errorf("Set-Cookie %q lacks %s", raw, w)
				}
			}
			if !proxy && strings.Contains(raw, "Secure") {
				t.Errorf("Set-Cookie %q is Secure on plain http without behind_tls_proxy", raw)
			}
			var lr api.LoginResult
			json.Unmarshal(body, &lr)
			if lr.CSRFToken == "" || lr.CSRFToken == cookie.Value || strings.Contains(lr.CSRFToken, cookie.Value) ||
				strings.Contains(cookie.Value, lr.CSRFToken) {
				t.Fatalf("csrf token %q next to session %q", lr.CSRFToken, cookie.Value)
			}
			if bytes.Contains(body, []byte(cookie.Value)) {
				t.Fatalf("the login answer has the session id: %s", body)
			}
			req := newRequest(t, "GET", h.base+"/api/v1/session", nil)
			req.AddCookie(cookie)
			var si json.RawMessage
			if st := h.send(h.http, req, &si); st != http.StatusOK || bytes.Contains(si, []byte(cookie.Value)) ||
				!bytes.Contains(si, []byte(lr.CSRFToken)) {
				t.Fatalf("session: %d %s", st, si)
			}
			var box api.Box
			h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
			if box.BehindTLSProxy != proxy || box.TLS {
				t.Fatalf("box: tls %v, behind_tls_proxy %v", box.TLS, box.BehindTLSProxy)
			}

			// Logout clears the cookie and ends the session.
			resp, body = h.rawPost("/api/v1/logout", "application/json", origin, "{}", cookie, lr.CSRFToken)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("logout: %d %s", resp.StatusCode, body)
			}
			clear := "esm_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict"
			if proxy {
				clear = "esm_session=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict"
			}
			if got := setCookie(resp); got != clear {
				t.Fatalf("logout Set-Cookie %q, want %q", got, clear)
			}
			req = newRequest(t, "GET", h.base+"/api/v1/session", nil)
			req.AddCookie(cookie)
			if st := h.send(h.http, req, nil); st != http.StatusUnauthorized {
				t.Fatalf("the session lives on after logout: %d", st)
			}

			// The session id is in no file and no log.
			auditLog, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
			if bytes.Contains(auditLog, []byte(cookie.Value)) || strings.Contains(h.stderr.String(), cookie.Value) {
				t.Fatal("the session id is in the audit log or the service log")
			}
			refused := 0
			for _, line := range strings.Split(string(auditLog), "\n") {
				var e audit.Entry
				if json.Unmarshal([]byte(line), &e) == nil && e.Action == "login" && !e.OK {
					refused++
				}
			}
			if refused != 6 {
				t.Fatalf("%d refused logins in the audit log, want 6", refused)
			}
		})
	}
}
