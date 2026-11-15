package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

// --- logins and their passwords ---

// UserView is a login as GET /users lists it: never its password hash.
type UserView struct {
	Username string    `json:"username"`
	Role     string    `json:"role"`
	Created  time.Time `json:"created"`
	// LastLogin is when the login was last used to log in; missing for
	// never (or not since ESM began to keep it).
	LastLogin time.Time `json:"last_login,omitzero"`
	// Sessions is how many sessions of the login are open now.
	Sessions int `json:"sessions"`
}

// UserList answers GET /users.
type UserList struct {
	Users []UserView `json:"users"`
}

// PasswordChangeRequest is the body of POST /session/password.
type PasswordChangeRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// PasswordResetRequest is the body of PUT /users/{name}/password.
type PasswordResetRequest struct {
	Password string `json:"password"`
}

// PasswordResult answers a change or a reset of a password: whose it was
// and how many of the login's sessions ended. No password is ever sent
// back.
type PasswordResult struct {
	Username      string `json:"username"`
	EndedSessions int    `json:"ended_sessions"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request, c *call) error {
	out := UserList{Users: []UserView{}}
	for _, l := range s.o.Auth.Logins() {
		out.Users = append(out.Users, UserView{Username: l.Username, Role: string(l.Role), Created: l.Created,
			LastLogin: l.LastLogin, Sessions: l.Sessions})
	}
	return writeJSON(w, http.StatusOK, out)
}

// changePassword is POST /session/password: the login of a browser session
// changes its own password, with its current one. Its other sessions end;
// this one goes on. A token is not a login and has no password of its own,
// so it cannot change any (nor can the socket, which sets one with
// resetPassword instead). A wrong current password counts against the
// login rate limit like a failed login and is answered after the same
// pause.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, c *call) error {
	if c.session == nil {
		return errStatus(http.StatusForbidden, "session_only",
			"a password is changed by its own login, logged in with the current password; a token cannot change one (on the box, esm user passwd sets one)")
	}
	c.target = c.session.Username
	var req PasswordChangeRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.CurrentPassword == "" {
		return fault.Invalidf("current_password: the current password is required")
	}
	ended, err := s.o.Auth.ChangePassword(auth.LimitAddress(r, s.behindProxy()), c.session.Username,
		req.CurrentPassword, req.NewPassword, c.session.ID)
	if err != nil {
		if errors.Is(err, auth.ErrWrongPassword) {
			s.failPause(r.Context())
		}
		return err
	}
	return writeJSON(w, http.StatusOK, PasswordResult{Username: c.session.Username, EndedSessions: ended})
}

// resetPassword is PUT /users/{name}/password, on the unix socket only (the
// route is local): the box owner sets a login's password without the
// current one, the way back in after a forgotten password. Every session
// of the login ends.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, c *call) error {
	name := r.PathValue("name")
	// Only a real user's name is recorded: anything else may be a password
	// typed in the wrong place (see auth.Manager.AuditName).
	c.target = s.o.Auth.AuditName(name)
	var req PasswordResetRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	u, ended, err := s.o.Auth.ResetPassword(name, req.Password)
	if errors.Is(err, auth.ErrNotFound) {
		// The name is not repeated, for the same reason.
		return fault.NotFoundf("there is no login of that name (GET /api/v1/users lists them)")
	}
	if err != nil {
		return err
	}
	c.target = u.Username
	return writeJSON(w, http.StatusOK, PasswordResult{Username: u.Username, EndedSessions: ended})
}
