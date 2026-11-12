package auth

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
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

// SessionCookie builds the esm_session cookie: HttpOnly, SameSite=Strict,
// Path=/, and Secure when the listener uses TLS.
func SessionCookie(id string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// ClearSessionCookie builds a cookie that removes esm_session.
func ClearSessionCookie(secure bool) *http.Cookie {
	c := SessionCookie("", secure)
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
// the same host (and port) as the Host header.
func CheckOrigin(origin, host string) error {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ErrOrigin
	}
	if !strings.EqualFold(stripDefaultPort(u.Host, u.Scheme), stripDefaultPort(host, u.Scheme)) {
		return ErrOrigin
	}
	return nil
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
// session cookie: an Origin header, when present, must match Host; and every
// state-changing method must carry X-CSRF-Token equal to the session's.
func CheckCookieRequest(r *http.Request, s *Session) error {
	if err := CheckOrigin(r.Header.Get("Origin"), r.Host); err != nil {
		return err
	}
	if SafeMethod(r.Method) {
		return nil
	}
	return CheckCSRF(s, r.Header.Get(CSRFHeader))
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
