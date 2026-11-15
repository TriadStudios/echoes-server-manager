package page_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

// The Box screen gives a login's own session the form that changes its
// password (current password, the new one twice); a token gets none, and
// the old "cannot change a password" is gone. The script sends the form to
// the API's change route like every change, with the CSRF token, and a
// change without that token is refused. The login form says how a
// forgotten password is set anew.
func TestPasswordForm(t *testing.T) {
	s := newSite(t)
	s.setup()
	box := s.checkPage("/box", "session", s.csrf, s.session)
	for _, want := range []string{
		`<form class="panel stack" data-form="password" aria-labelledby="password-h">`,
		`name="current" autocomplete="current-password" required`,
		`name="password" autocomplete="new-password" minlength="12" required`,
		`name="password2" autocomplete="new-password" minlength="12" required`,
		`href="#password-h">Change password</a>`,
		"esm user passwd admin",
	} {
		if !strings.Contains(box, want) {
			t.Errorf("the Box screen of a session lacks %q", want)
		}
	}
	if strings.Contains(box, "cannot change a password") {
		t.Error("the Box screen still says a password cannot be changed")
	}
	if b := s.checkPage("/box", s.admin, ""); strings.Contains(b, `data-form="password"`) {
		t.Error("a token is offered the password form")
	}
	if _, login := s.get("/setup", ""); !strings.Contains(login, `data-form="login"`) || !strings.Contains(login, "esm user passwd &lt;name&gt;") {
		t.Errorf("the login form does not say how to set a forgotten password:\n%s", login)
	}
	_, js := s.get("/static/esm.js", "")
	for _, need := range []string{
		"password: async function (f)",
		"val(f, 'password') !== val(f, 'password2')",
		"call('POST', '/api/v1/session/password', { current_password: val(f, 'current'), new_password: val(f, 'password') })",
		"done('password')",
	} {
		if !strings.Contains(js, need) {
			t.Errorf("esm.js lacks %q", need)
		}
	}
	if _, b := s.get("/box?done=password", "session"); !strings.Contains(b, "Password changed. Your other sessions have ended") {
		t.Error("the notice after a change")
	}

	// What the script sends, without the CSRF token: refused.
	req, _ := http.NewRequest("POST", s.base+"/api/v1/session/password",
		strings.NewReader(`{"current_password":"a long password 1","new_password":"another long password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.session})
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a change without the CSRF token: %d", resp.StatusCode)
	}
	// And with it, as the script sends it: changed, and the session goes on.
	var res api.PasswordResult
	s.must(http.StatusOK, "POST", "/api/v1/session/password", "session",
		api.PasswordChangeRequest{CurrentPassword: "a long password 1", NewPassword: "another long password"}, &res)
	if res.Username != "admin" {
		t.Fatalf("change: %+v", res)
	}
	if resp, _ := s.get("/box", "session"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the session after its change: %d", resp.StatusCode)
	}
	if st := s.api("POST", "/api/v1/login", "", api.LoginRequest{Username: "admin", Password: "another long password"}, nil); st != http.StatusOK {
		t.Fatalf("the new password: %d", st)
	}
}
