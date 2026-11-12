package auth

import (
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientIP returns the IP of the direct peer. Forwarding headers are not
// trusted.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SessionCookie builds the esm_session cookie: HttpOnly (scripts cannot
// read it), SameSite=Strict, Path=/, Max-Age equal to life (the lifetime of
// the session), and Secure when browsers reach ESM over TLS (its own, or a
// TLS proxy in front of it).
func SessionCookie(id string, life time.Duration, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   max(int(life/time.Second), 1),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// ClearSessionCookie builds a cookie that removes esm_session (Max-Age=0).
func ClearSessionCookie(secure bool) *http.Cookie {
	c := SessionCookie("", 0, secure)
	c.MaxAge = -1
	return c
}

// SafeMethod reports methods that never change state.
func SafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// CheckCSRF compares the X-CSRF-Token value with the session's token in
// constant time.
func CheckCSRF(s *Session, header string) error {
	if s == nil || header == "" || s.CSRF == "" ||
		subtle.ConstantTimeCompare([]byte(header), []byte(s.CSRF)) != 1 {
		return ErrCSRF
	}
	return nil
}

// CheckOrigin accepts a missing Origin header; when present it must name
// the scheme browsers reach ESM with ("http", or "https" behind TLS, ESM's
// own or a TLS proxy's) and the same host (and port) as the Host header.
func CheckOrigin(origin, host, scheme string) error {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Scheme != scheme {
		return ErrOrigin
	}
	if !strings.EqualFold(stripDefaultPort(u.Host, scheme), stripDefaultPort(host, scheme)) {
		return ErrOrigin
	}
	return nil
}

// Scheme is the scheme browsers reach ESM with: "https" when https is true
// (ESM's own TLS, or a TLS proxy in front of it), else "http".
func Scheme(https bool) string {
	if https {
		return "https"
	}
	return "http"
}

func stripDefaultPort(hostport, scheme string) string {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	if (scheme == "http" && p == "80") || (scheme == "https" && p == "443") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return hostport
}

// CheckCookieRequest applies the rules for a request authenticated by the
// session cookie: an Origin header, when present, must match scheme and
// Host; and every state-changing method must carry X-CSRF-Token equal to
// the session's.
func CheckCookieRequest(r *http.Request, s *Session, scheme string) error {
	if err := CheckOrigin(r.Header.Get("Origin"), r.Host, scheme); err != nil {
		return err
	}
	if SafeMethod(r.Method) {
		return nil
	}
	return CheckCSRF(s, r.Header.Get(CSRFHeader))
}

// CheckLoginRequest applies the rules for the calls that take a password
// without a session, login and setup. The body must be declared as JSON
// (Content-Type: application/json), which a plain form on another site
// cannot send. An Origin header, when present, must match scheme and host,
// so a page of another origin cannot log a browser in. A request without
// Origin (the command line, curl, scripts) is accepted: browsers send
// Origin with every POST.
func CheckLoginRequest(r *http.Request, scheme string) error {
	if err := CheckOrigin(r.Header.Get("Origin"), r.Host, scheme); err != nil {
		return err
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return ErrNotJSON
	}
	return nil
}

// BearerToken returns the token from "Authorization: Bearer <token>".
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}
