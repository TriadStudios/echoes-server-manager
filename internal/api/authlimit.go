package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

// Limits on what callers without a valid login cost ESM, per client
// address (an IPv4 address, or the /64 of an IPv6 address, see clientKey),
// on every TCP listener; the unix socket is never limited.
//
// A call that needs a login and comes without a valid one (no credentials,
// a token that is wrong, unknown or deleted, a session that ended, an
// Authorization header that is not a bearer token, a session cookie on a
// listener that takes tokens only, a call answered only on the socket) is
// a failed authentication. At AuthFailMax of them inside AuthFailWindow,
// ESM answers every such call of that address with 429 and Retry-After,
// without recording it, until the oldest of them is AuthFailWindow old;
// those refused count too, so a flood keeps itself refused. A call with a
// valid token or session is never refused by this limit, and the login has
// a limit of its own (auth.Manager.Login), which this one does not touch.
//
// What is recorded of callers without a valid login is bounded the same
// way: of the failed authentications of an address, the first in each
// window, and one entry when the address is limited; of its other calls
// that the audit log takes (failed logins and setup codes, logouts without
// a session, changes to unknown paths), at most AnonAuditMax per window,
// and one entry when it has more.
const (
	AuthFailWindow = time.Minute
	AuthFailMax    = 60
	AnonAuditMax   = 20
)

// limits are the limiters of callers without a valid login.
type limits struct {
	fails *auth.Limiter // failed authentications per address
	anon  *auth.Limiter // audit entries of calls without a login per address
	// once holds one slot per key and window: "fail:" (the failed
	// authentication recorded), "limited:" and "quiet:" (the entries that
	// say an address is limited, or not recorded any more).
	once *auth.Limiter
	now  func() time.Time
}

func newLimits(now func() time.Time) *limits {
	if now == nil {
		now = time.Now
	}
	return &limits{
		fails: auth.NewLimiter(AuthFailWindow, AuthFailMax, now),
		anon:  auth.NewLimiter(AuthFailWindow, AnonAuditMax, now),
		once:  auth.NewLimiter(AuthFailWindow, 1, now),
		now:   now,
	}
}

// first reports whether this is the first use of key in the window.
func (l *limits) first(key string) bool {
	_, err := l.once.Reserve(key)
	return err == nil
}

// clientKey is the key of a request's client address in the limits: the
// IPv4 address, or the /64 of an IPv6 address, which one host or network
// usually holds whole.
func clientKey(r *http.Request) string {
	addr := auth.ClientIP(r)
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return addr
	case ip.To4() != nil:
		return ip.To4().String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// authFailed handles a call that needs a login and came without a valid
// one: it counts the failure against the client's address, keeps every
// failure but the first in the window out of the audit log, and records
// one entry when the address becomes limited. limited is the address's
// limit before this call: when it was over it already, the answer is 429
// (and nothing is recorded); otherwise it is answer. recorded says whether
// the audit log takes the call at all (a change, or a 403): only such a
// call is the first one recorded.
func (s *Server) authFailed(r *http.Request, c *call, key string, limited, answer error, recorded bool) error {
	s.lim.fails.Fail(key)
	if limited != nil {
		c.quiet = true
		return limited
	}
	if recorded && !s.lim.first("fail:"+key) {
		c.quiet = true
	}
	if rl := s.lim.fails.Check(key); rl != nil && s.lim.first("limited:"+key) {
		var e *auth.RateLimitError
		until := s.lim.now().Add(AuthFailWindow)
		if errors.As(rl, &e) {
			until = s.lim.now().Add(e.RetryAfter)
		}
		s.auditNote(r, "auth.limited", key, fmt.Sprintf(
			"%d failed authentications from %s within %s: its calls without a valid token or login are answered 429 and not recorded until %s, longer while they go on",
			s.lim.fails.Failures(key), key, AuthFailWindow, until.UTC().Format(time.RFC3339)))
	}
	return answer
}

// anonAudit reports whether an audit entry of a call without a valid login
// may be written now (see AnonAuditMax), and records the one entry that
// says when an address has more.
func (s *Server) anonAudit(r *http.Request, key string) bool {
	if _, err := s.lim.anon.Reserve(key); err == nil {
		return true
	}
	if s.lim.first("quiet:" + key) {
		s.auditNote(r, "audit.quiet", key, fmt.Sprintf(
			"more than %d calls without a valid login from %s within %s: the next are not recorded until %s",
			AnonAuditMax, key, AuthFailWindow, s.lim.now().Add(AuthFailWindow).UTC().Format(time.RFC3339)))
	}
	return false
}

// auditNote writes an entry about a client address.
func (s *Server) auditNote(r *http.Request, action, key, msg string) {
	if s.o.Audit == nil {
		return
	}
	e := audit.Entry{Actor: audit.Actor{Kind: audit.ActorAnonymous, ID: auth.ClientIP(r)}, Action: action, Target: key, Error: msg}
	if err := s.o.Audit.Append(e); err != nil {
		s.log.Error("cannot write the audit log", "err", err)
	}
}
