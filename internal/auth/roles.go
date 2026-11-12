package auth

import "fmt"

// Role is what a user, token or socket client may do.
type Role string

// Roles, from least to most powerful.
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
)

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

// Valid reports whether r is one of the three roles.
func (r Role) Valid() bool { return r.rank() > 0 }

// Allows reports whether r includes everything need may do.
func (r Role) Allows(need Role) bool {
	return r.Valid() && need.Valid() && r.rank() >= need.rank()
}

// ParseRole parses "read", "operator" or "admin".
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("%w (got %q)", ErrBadRole, s)
	}
	return r, nil
}
