package auth

import "fmt"

// Role is what a user, token or socket client may do.
type Role string

// Roles. Read, operator and admin go from least to most powerful; site is
// beside that ladder, not a step of it.
const (
	// RoleRead may make every GET call except listing tokens, the audit log
	// and downloading backups.
	RoleRead Role = "read"
	// RoleOperator may also start, stop, restart, update, back up and
	// restore instances and download their backups (not install builds: a
	// build runs with ESM's own rights).
	RoleOperator Role = "operator"
	// RoleAdmin may do everything.
	RoleAdmin Role = "admin"
	// RoleSite is for the one token of a website's admin panel. It may do
	// everything an operator may, and of what only an admin may, just the
	// few calls the API opens to it route by route and, where a call is
	// open in part, field by field: changing an instance's settings,
	// installing a build from a Steam source, pruning builds, and looking
	// for and taking a newer ESM release (but not ending ESM for good).
	// Allows does not say that it includes the admin role, so a route that
	// needs admin stays closed to it unless the route itself is opened.
	// Only an API token has this role, never a login.
	RoleSite Role = "site"
)

// rank orders the roles of the ladder. The site role has none: Allows
// matches it by what it includes, not by a place in the order.
func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Valid reports whether r is one of the four roles a token may have.
func (r Role) Valid() bool { return r.rank() > 0 || r == RoleSite }

// CanLogin reports whether a login (a user and its browser sessions) may
// have the role: every role but site, which is for API tokens only.
func (r Role) CanLogin() bool { return r.rank() > 0 }

// Allows reports whether r includes everything need may do. The site role
// includes read, operator and itself; admin includes the site role; no
// other role does.
func (r Role) Allows(need Role) bool {
	switch {
	case !r.Valid() || !need.Valid():
		return false
	case r == RoleSite:
		return need.rank() <= RoleOperator.rank()
	case need == RoleSite:
		return r == RoleAdmin
	}
	return r.rank() >= need.rank()
}

// ParseRole parses "read", "operator", "admin" or "site", the roles of a
// token (CanLogin says which of them a login may have).
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("%w (got %q)", ErrBadRole, s)
	}
	return r, nil
}
