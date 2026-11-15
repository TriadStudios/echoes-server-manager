package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const newPassword = "a brand new password"

func (m *Manager) openSessions(username string) int {
	for _, l := range m.Logins() {
		if l.Username == username {
			return l.Sessions
		}
	}
	return -1
}

// A change needs the current password and passes the password rules
// first. A new password the rules refuse tests nothing and counts nothing;
// a wrong current password counts against the user name and the address
// like a failed login, so the limit holds across the change and the login.
// A change ends the user's other sessions and keeps the one named.
func TestChangePassword(t *testing.T) {
	m, st, _ := newManager(t)
	if _, err := m.CreateUser("admin", goodPassword, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	keep, _ := m.Login("192.0.2.1", "admin", goodPassword)
	other, _ := m.Login("192.0.2.9", "admin", goodPassword)
	for range DefaultRateMax + 1 {
		if _, err := m.ChangePassword("192.0.2.1", "admin", goodPassword, "short", keep.ID); !errors.Is(err, ErrWeakPassword) {
			t.Fatalf("a weak new password: %v", err)
		}
		if _, err := m.ChangePassword("192.0.2.1", "admin", goodPassword, strings.Repeat("x", MaxPasswordLength+1), keep.ID); !errors.Is(err, ErrLongPassword) {
			t.Fatalf("a new password over %d bytes: %v", MaxPasswordLength, err)
		}
	}
	if err := m.Limiter().Check(userKey("admin"), "ip:192.0.2.1"); err != nil {
		t.Fatalf("refused new passwords counted as failures: %v", err)
	}
	for i := range DefaultRateMax {
		if _, err := m.ChangePassword("192.0.2.1", "admin", "wrong password "+string(rune('a'+i)), newPassword, keep.ID); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("wrong current password %d: %v", i, err)
		}
	}
	var rl *RateLimitError
	if _, err := m.ChangePassword("192.0.2.1", "admin", goodPassword, newPassword, keep.ID); !errors.As(err, &rl) {
		t.Fatalf("the right password at the limit: %v", err)
	}
	if _, err := m.Login("192.0.2.200", "admin", goodPassword); !errors.As(err, &rl) {
		t.Fatalf("a login from another address after failed changes: %v", err)
	}
	if _, err := m.Login("192.0.2.1", "nobody", goodPassword); !errors.As(err, &rl) {
		t.Fatalf("a login of another name from the address of failed changes: %v", err)
	}
	m.Limiter().Reset(userKey("admin"), "ip:192.0.2.1")

	ended, err := m.ChangePassword("192.0.2.1", "ADMIN", goodPassword, newPassword, keep.ID)
	if err != nil || ended != 1 {
		t.Fatalf("change: %d %v", ended, err)
	}
	if _, err := m.Session(keep.ID); err != nil {
		t.Fatalf("the session the change was made in: %v", err)
	}
	if _, err := m.Session(other.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("another session after the change: %v", err)
	}
	if _, err := m.Login("192.0.2.1", "admin", goodPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("the old password: %v", err)
	}
	if _, err := m.Login("192.0.2.1", "admin", newPassword); err != nil {
		t.Fatalf("the new password: %v", err)
	}
	raw, _ := st.ReadFile(usersFile)
	if strings.Contains(string(raw), newPassword) || strings.Contains(string(raw), goodPassword) {
		t.Fatal("users.json holds a password")
	}
	// keep "" ends every session.
	if ended, err := m.ChangePassword("", "admin", newPassword, goodPassword, ""); err != nil || ended != 2 || m.openSessions("admin") != 0 {
		t.Fatalf("change that keeps no session: %d %v, %d open", ended, err, m.openSessions("admin"))
	}
}

// A reset needs no current password, ends every session of the user and
// forgets the failed logins of the name, so the new password works at once.
func TestResetPassword(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.CreateUser("admin", goodPassword, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser("ops", goodPassword, RoleOperator); err != nil {
		t.Fatal(err)
	}
	a, _ := m.Login("192.0.2.1", "admin", goodPassword)
	b, _ := m.Login("192.0.2.2", "admin", goodPassword)
	ops, _ := m.Login("192.0.2.3", "ops", goodPassword)
	for range DefaultRateMax {
		m.Login("192.0.2.4", "admin", "wrong password!")
	}
	if _, _, err := m.ResetPassword("nobody", newPassword); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown user: %v", err)
	}
	if _, _, err := m.ResetPassword("admin", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("a weak password: %v", err)
	}
	if _, _, err := m.ResetPassword("admin", strings.Repeat("x", MaxPasswordLength+1)); !errors.Is(err, ErrLongPassword) {
		t.Fatalf("a password over %d bytes: %v", MaxPasswordLength, err)
	}
	stale := m.Users()[0].Password
	u, ended, err := m.ResetPassword("Admin", newPassword)
	if err != nil || u.Username != "admin" || ended != 2 {
		t.Fatalf("reset: %+v %d %v", u, ended, err)
	}
	for _, s := range []*Session{a, b} {
		if _, err := m.Session(s.ID); !errors.Is(err, ErrNoSession) {
			t.Fatalf("a session after the reset: %v", err)
		}
	}
	if _, err := m.Session(ops.ID); err != nil {
		t.Fatalf("another user's session after the reset: %v", err)
	}
	// A login whose check of the old password ran while the reset was made
	// opens no session that would outlive it.
	if _, err := m.newSession("admin", RoleAdmin, stale); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("a session for the password from before the reset: %v", err)
	}
	if _, err := m.Login("192.0.2.5", "admin", newPassword); err != nil {
		t.Fatalf("the new password at once: %v", err)
	}
	if _, err := m.Login("192.0.2.5", "admin", goodPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("the old password: %v", err)
	}
}

// The last login is kept in logins.json, not in users.json (which an older
// ESM could not read with a field it does not know), and read back at the
// next start. A broken logins.json does not keep ESM from starting.
func TestLastLoginIsKeptApart(t *testing.T) {
	m, st, c := newManager(t)
	if _, err := m.CreateUser("admin", goodPassword, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if l := m.Logins(); len(l) != 1 || !l[0].LastLogin.IsZero() || l[0].Sessions != 0 || l[0].Role != RoleAdmin {
		t.Fatalf("before a login: %+v", l)
	}
	c.Add(time.Hour)
	at := c.Now()
	if _, err := m.Login("192.0.2.1", "ADMIN", goodPassword); err != nil {
		t.Fatal(err)
	}
	if l := m.Logins(); !l[0].LastLogin.Equal(at) || l[0].Sessions != 1 {
		t.Fatalf("after a login: %+v", l)
	}
	users, _ := st.ReadFile(usersFile)
	logins, _ := st.ReadFile(loginsFile)
	if strings.Contains(string(users), "last_login") || !strings.Contains(string(logins), `"admin"`) {
		t.Fatalf("users.json:\n%s\nlogins.json:\n%s", users, logins)
	}
	again, err := New(st, Options{Iterations: 1000, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if l := again.Logins(); !l[0].LastLogin.Equal(at) || l[0].Sessions != 0 {
		t.Fatalf("after a restart: %+v", l)
	}
	if err := st.WriteFile(loginsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := New(st, Options{Iterations: 1000, Now: c.Now})
	if err != nil {
		t.Fatalf("a broken logins.json: %v", err)
	}
	if _, err := broken.Login("192.0.2.1", "admin", goodPassword); err != nil {
		t.Fatal(err)
	}
	if l := broken.Logins(); !l[0].LastLogin.Equal(at) {
		t.Fatalf("after a broken logins.json: %+v", l)
	}
}
