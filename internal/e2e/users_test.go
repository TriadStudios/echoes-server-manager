package e2e

import (
	"encoding/json"
	"fmt"
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

// browserLogin logs name in over TCP as the page does and returns the
// session cookie and the CSRF token, or the status of a refused login.
func (h *harness) browserLogin(name, password string) (*http.Cookie, string, int) {
	h.t.Helper()
	resp, body := h.rawPost("/api/v1/login", "application/json", "", fmt.Sprintf(`{"username":%q,"password":%q}`, name, password), nil, "")
	if resp.StatusCode != http.StatusOK {
		return nil, "", resp.StatusCode
	}
	var lr api.LoginResult
	json.Unmarshal(body, &lr)
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c, lr.CSRFToken, resp.StatusCode
		}
	}
	h.t.Fatalf("login of %s set no cookie", name)
	return nil, "", 0
}

// sessionOpen reports whether a session cookie still answers GET /session.
func (h *harness) sessionOpen(c *http.Cookie) bool {
	h.t.Helper()
	req := newRequest(h.t, "GET", h.base+"/api/v1/session", nil)
	req.AddCookie(c)
	return h.send(h.http, req, nil) == http.StatusOK
}

// putWithSession sends a PUT over TCP with a session cookie and its CSRF
// token.
func (h *harness) putWithSession(path string, c *http.Cookie, csrf string, body any) (int, string) {
	h.t.Helper()
	req := newRequest(h.t, "PUT", h.base+path, body)
	req.AddCookie(c)
	req.Header.Set(auth.CSRFHeader, csrf)
	var eb api.ErrorBody
	st := h.send(h.http, req, &eb)
	return st, eb.Error.Code
}

// esm user list shows the logins; esm user passwd sets a login's password
// over the unix socket, from standard input when that is no terminal, and
// ends every session of the login. The password is never taken from the
// command line or the environment, the call is refused over TCP whatever
// login or token comes with it, and the audit log records each change and
// reset without a password. A change through the page's route is
// recorded next to it.
func TestUserListAndPasswd(t *testing.T) {
	// Not parallel: it sets environment variables.
	const (
		setupPw  = "the first password 1"
		resetPw  = "a reset password 22"
		changePw = "a changed password 333"
	)
	for _, k := range []string{"ESM_PASSWORD", "ESM_NEW_PASSWORD", "PASSWORD"} {
		t.Setenv(k, "from-the-environment-1")
	}
	h := newESM(t)
	code, out, errOut := h.runCLI("user", "list")
	if code != 0 || !strings.HasPrefix(oneLine(t, "user list", out), "no logins yet") {
		t.Fatalf("user list before setup: %d %q %q", code, out, errOut)
	}
	m := regexp.MustCompile(`ESM setup code: ([A-Z2-7]{24})`).FindStringSubmatch(h.stderr.String())
	if m == nil {
		t.Fatal("no setup code printed")
	}
	if st := h.call("POST", "/api/v1/setup", "", api.SetupRequest{Code: m[1], Username: "admin", Password: setupPw}, nil); st != http.StatusCreated {
		t.Fatalf("setup: %d", st)
	}
	one, csrf, _ := h.browserLogin("admin", setupPw)
	two, _, _ := h.browserLogin("admin", setupPw)

	code, out, errOut = h.runCLI("user", "list")
	if line := oneLine(t, "user list", out); code != 0 || !regexp.MustCompile(`^1 login: admin \(admin, last login \S+ ago, 2 sessions open\)$`).MatchString(line) {
		t.Fatalf("user list: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("--json", "user", "list")
	var ul api.UserList
	if err := json.Unmarshal([]byte(oneLine(t, "user list --json", out)), &ul); code != 0 || err != nil || len(ul.Users) != 1 ||
		ul.Users[0].Username != "admin" || ul.Users[0].LastLogin.IsZero() || ul.Users[0].Sessions != 2 || strings.Contains(out, "hash") {
		t.Fatalf("user list --json: %d %v %q", code, err, out)
	}

	// Refused, and nothing is changed: the password as an argument, as a
	// flag, only in the environment, for a login that is not there, too
	// weak, and from another machine.
	for _, c := range []struct {
		stdin string
		args  []string
		want  string
	}{
		{resetPw + "\n", []string{"user", "passwd", "admin", resetPw}, "never taken from the command line"},
		{"", []string{"user", "passwd", "--password=" + resetPw, "admin"}, "not defined"},
		{"", []string{"user", "passwd", "admin"}, "standard input"},
		{resetPw + "\n", []string{"user", "passwd", "No-Such-Login-7"}, "no login of that name"},
		{"too short\n", []string{"user", "passwd", "admin"}, "at least 12 characters"},
		{resetPw + "\n", []string{"--url", h.base, "--token", h.admin, "user", "passwd", "admin"}, "only on the box"},
	} {
		code, out, errOut := h.runCLIIn(c.stdin, c.args...)
		all := out + errOut
		if code != 1 || !strings.Contains(errOut, c.want) || strings.Contains(all, resetPw) || strings.Contains(all, "No-Such-Login-7") ||
			strings.Contains(all, "from-the-environment") {
			t.Fatalf("%q: %d %q %q, want a refusal with %q", c.args, code, out, errOut, c.want)
		}
	}
	// Over TCP the call is refused for an admin token and an admin's
	// session alike.
	var eb api.ErrorBody
	if st := h.call("PUT", "/api/v1/users/admin/password", h.admin, api.PasswordResetRequest{Password: resetPw}, &eb); st != http.StatusForbidden || eb.Error.Code != "socket_only" {
		t.Fatalf("reset over TCP with an admin token: %d %+v", st, eb)
	}
	if st, code := h.putWithSession("/api/v1/users/admin/password", one, csrf, api.PasswordResetRequest{Password: resetPw}); st != http.StatusForbidden || code != "socket_only" {
		t.Fatalf("reset over TCP with an admin session: %d %s", st, code)
	}
	if !h.sessionOpen(one) || !h.sessionOpen(two) {
		t.Fatal("a refused reset ended a session")
	}
	three, _, st := h.browserLogin("admin", setupPw)
	if st != http.StatusOK {
		t.Fatalf("after the refusals the first password logs in: %d", st)
	}

	// The reset, with the password on standard input: no prompt, one line.
	code, out, errOut = h.runCLIIn(resetPw+"\n", "user", "passwd", "ADMIN")
	if code != 0 || oneLine(t, "user passwd", out) != "set the password of admin and ended its 3 sessions" || errOut != "" {
		t.Fatalf("user passwd: %d %q %q", code, out, errOut)
	}
	for i, c := range []*http.Cookie{one, two, three} {
		if h.sessionOpen(c) {
			t.Fatalf("session %d lives on after the reset", i+1)
		}
	}
	if _, _, st := h.browserLogin("admin", setupPw); st != http.StatusUnauthorized {
		t.Fatalf("the old password after the reset: %d", st)
	}
	here, csrf, st := h.browserLogin("admin", resetPw)
	if st != http.StatusOK {
		t.Fatalf("the new password after the reset: %d", st)
	}
	code, out, _ = h.runCLIIn("", "--json", "user", "passwd", "admin")
	if code != 1 || !strings.Contains(out, `"code":"usage"`) {
		t.Fatalf("user passwd --json without a password: %d %q", code, out)
	}

	// The page's change: the current password, the other sessions end.
	other, _, _ := h.browserLogin("admin", resetPw)
	resp, body := h.rawPost("/api/v1/session/password", "application/json", "",
		fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, resetPw, changePw), here, csrf)
	var pr api.PasswordResult
	json.Unmarshal(body, &pr)
	if resp.StatusCode != http.StatusOK || pr.Username != "admin" || pr.EndedSessions != 1 {
		t.Fatalf("change: %d %s", resp.StatusCode, body)
	}
	if !h.sessionOpen(here) || h.sessionOpen(other) {
		t.Fatal("the change did not keep its own session alone")
	}
	code, out, _ = h.runCLI("user", "list")
	if !strings.Contains(oneLine(t, "user list", out), ", 1 session open)") {
		t.Fatalf("user list after the change: %d %q", code, out)
	}

	// The audit log: a reset by the socket and a change by the login, each
	// naming the login, the refusals over TCP, and no password anywhere.
	b, err := os.ReadFile(filepath.Join(h.root, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	var resets, changes, refused []audit.Entry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e audit.Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch {
		case e.Action == "user.password.reset" && e.OK:
			resets = append(resets, e)
		case e.Action == "user.password.reset":
			refused = append(refused, e)
		case e.Action == "user.password.change" && e.OK:
			changes = append(changes, e)
		}
	}
	if len(resets) != 1 || resets[0].Actor.Kind != audit.ActorSocket || resets[0].Target != "admin" {
		t.Fatalf("reset entries: %+v", resets)
	}
	if len(changes) != 1 || changes[0].Actor.Kind != audit.ActorUser || changes[0].Actor.ID != "admin" || changes[0].Target != "admin" {
		t.Fatalf("change entries: %+v", changes)
	}
	// The weak password on the socket, and the two over TCP (the command
	// line asks for nothing, and sends nothing, for a login that is not
	// there).
	if len(refused) != 3 || refused[0].Actor.Kind != audit.ActorSocket || refused[0].Target != "admin" ||
		refused[1].Actor.Kind != audit.ActorToken || refused[2].Actor.Kind != audit.ActorUser || refused[2].Target != "" {
		t.Fatalf("refused resets: %+v", refused)
	}
	for what, text := range map[string]string{"audit log": string(b), "service log": h.stderr.String()} {
		for _, pw := range []string{setupPw, resetPw, changePw, "too short", "No-Such-Login-7"} {
			if strings.Contains(text, pw) {
				t.Errorf("the %s holds %q", what, pw)
			}
		}
	}
}
