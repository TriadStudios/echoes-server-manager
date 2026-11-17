package auth

import (
	"errors"
	"testing"
)

// The site role is no step of the ladder read < operator < admin: it
// includes read and operator and is included by admin, and it never includes
// admin, however the others are asked.
func TestSiteRoleIsBesideTheLadder(t *testing.T) {
	roles := []Role{RoleRead, RoleOperator, RoleAdmin, RoleSite}
	// have -> need -> allowed
	want := map[Role]map[Role]bool{
		RoleRead:     {RoleRead: true, RoleOperator: false, RoleAdmin: false, RoleSite: false},
		RoleOperator: {RoleRead: true, RoleOperator: true, RoleAdmin: false, RoleSite: false},
		RoleAdmin:    {RoleRead: true, RoleOperator: true, RoleAdmin: true, RoleSite: true},
		RoleSite:     {RoleRead: true, RoleOperator: true, RoleAdmin: false, RoleSite: true},
	}
	for _, have := range roles {
		for _, need := range roles {
			if got := have.Allows(need); got != want[have][need] {
				t.Errorf("%q.Allows(%q) = %v, want %v", have, need, got, want[have][need])
			}
		}
		for _, bad := range []Role{"", "root", "Site", "SITE", " site"} {
			if have.Allows(bad) || bad.Allows(have) {
				t.Errorf("%q and %q allow each other", have, bad)
			}
		}
	}
}

func TestParseRoleTakesSiteForTokensOnly(t *testing.T) {
	for _, s := range []string{"read", "operator", "admin", "site"} {
		r, err := ParseRole(s)
		if err != nil || string(r) != s || !r.Valid() {
			t.Errorf("ParseRole(%q) = %q, %v", s, r, err)
		}
	}
	for _, s := range []string{"", "Site", "SITE", "site ", "root", "sites"} {
		if _, err := ParseRole(s); !errors.Is(err, ErrBadRole) {
			t.Errorf("ParseRole(%q): %v", s, err)
		}
	}
	for r, can := range map[Role]bool{RoleRead: true, RoleOperator: true, RoleAdmin: true, RoleSite: false, "": false} {
		if r.CanLogin() != can {
			t.Errorf("%q.CanLogin() = %v", r, !can)
		}
	}
}

// A token may have the site role, and keeps it across a restart; a login may
// not: it cannot be made with it, and a user the file names with it (made
// by hand) gets no session.
func TestSiteRoleIsForTokensNotLogins(t *testing.T) {
	m, st, c := newManager(t)
	plain, tok, err := m.CreateToken("panel", RoleSite)
	if err != nil || tok.Role != RoleSite {
		t.Fatalf("site token: %+v %v", tok, err)
	}
	if got, err := m.AuthenticateToken(plain); err != nil || got.Role != RoleSite {
		t.Fatalf("authenticate: %+v %v", got, err)
	}
	m2, err := New(st, Options{Iterations: 1000, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m2.AuthenticateToken(plain); err != nil || got.Role != RoleSite {
		t.Fatalf("after a restart: %+v %v", got, err)
	}
	if role, err := m2.TokenRole(tok.ID); err != nil || role != RoleSite {
		t.Fatalf("TokenRole: %q %v", role, err)
	}

	if _, err := m.CreateUser("web", goodPassword, RoleSite); !errors.Is(err, ErrBadRole) {
		t.Fatalf("a login with the site role: %v", err)
	}
	if len(m.Users()) != 0 {
		t.Fatalf("users after the refusal: %+v", m.Users())
	}
	h, err := HashPassword(goodPassword, 1000)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.users = append(m.users, User{Username: "byhand", Role: RoleSite, Password: h, Created: c.Now()})
	m.mu.Unlock()
	s, err := m.Login("", "byhand", goodPassword)
	if !errors.Is(err, ErrForbidden) || s != nil {
		t.Fatalf("login of a user with the site role: %+v %v", s, err)
	}
	if n := len(m.sessions); n != 0 {
		t.Fatalf("%d sessions after the refused login", n)
	}
}
