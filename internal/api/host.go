package api

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// hostSet is the host names a TCP listener answers to. A browser sends
// the name it used in the Host header; a page of another site that points
// its own name at 127.0.0.1 (DNS rebinding) sends that name, so checking
// the header keeps such a page from reaching ESM at all.
type hostSet struct {
	exact   map[string]bool // "host:port", host lower case, IPs normalized
	anyPort map[string]bool // entries given without a port
}

// newHostSet builds the allowed hosts for a listener bound to listen:
// localhost, 127.0.0.1 and [::1] with the listen port; the listen address
// itself when it is a specific host; and every public_hosts entry (host,
// any port; or host:port). It returns nil, no check, when listen is empty.
func newHostSet(listen string, public []string) *hostSet {
	if listen == "" {
		return nil
	}
	hs := &hostSet{exact: map[string]bool{}, anyPort: map[string]bool{}}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return hs
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		hs.exact[joinHost(h, port)] = true
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		hs.exact[joinHost(host, port)] = true
	}
	hs.add(public)
	return hs
}

// newAPIHostSet builds the allowed hosts of the API listener bound to
// listen: its host_names entries (host, any port; or host:port), and the
// listen address itself, with or without a port, when it is one address.
// Loopback names are not added: the listener answers to them only when
// they are its listen address or in host_names.
func newAPIHostSet(listen string, names []string) *hostSet {
	hs := &hostSet{exact: map[string]bool{}, anyPort: map[string]bool{}}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
			hs.anyPort[normHost(host)] = true
		}
	}
	hs.add(names)
	return hs
}

// add adds entries of the form host (any port) or host:port.
func (hs *hostSet) add(entries []string) {
	for _, p := range entries {
		if h, pt, err := net.SplitHostPort(p); err == nil {
			if n, ok := config.ParsePort(pt); ok {
				hs.exact[joinHost(h, strconv.Itoa(n))] = true
			}
		} else {
			hs.anyPort[normHost(p)] = true
		}
	}
}

// normHost is a host name in the form the set keeps: lower case, without
// brackets or a final dot, an IP address in its shortest form.
func normHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

func joinHost(host, port string) string {
	return net.JoinHostPort(normHost(host), port)
}

// allows reports whether a request with this Host header is for ESM. A
// header without a port means the scheme's default port.
func (hs *hostSet) allows(header string, tls bool) bool {
	if hs == nil {
		return true
	}
	if header == "" {
		return false
	}
	host, port, err := net.SplitHostPort(header)
	if err != nil {
		host, port = header, "80"
		if tls {
			port = "443"
		}
	}
	n, ok := config.ParsePort(port)
	if !ok || n < 1 {
		return false
	}
	// The port as a number, so "08740" is 8740.
	return hs.exact[joinHost(host, strconv.Itoa(n))] || hs.anyPort[normHost(host)]
}

// listener is how one TCP listener of ESM treats the requests it takes:
// the main one (listen in esm.json), or the API listener (api_listener).
// The unix socket has none of its own: it acts as admin and is not checked.
type listener struct {
	hosts *hostSet // nil: no Host check
	// tls says that the listener speaks TLS: a Host without a port means
	// port 443.
	tls bool
	// https says that browsers reach the listener over https, its own TLS
	// or a TLS proxy's: the session cookie is Secure, and an Origin header
	// must say https.
	https bool
	// behindProxy says that every client arrives from a proxy's address
	// (behind_tls_proxy), so the login limit counts per user name only
	// (see auth.LimitAddress).
	behindProxy bool
	// tokensOnly says that only API tokens are taken: no page, no login,
	// setup or logout, and no session cookie.
	tokensOnly bool
	// hostHint is what a 421 answer tells the client to do.
	hostHint string
}

const listenerKey ctxKey = 2

// listenerOf is the listener a request arrived on: the API listener's, or
// the main one's (also for the socket and for the page's own calls, which
// carry the context of the request they serve).
func (s *Server) listenerOf(r *http.Request) *listener {
	if l, ok := r.Context().Value(listenerKey).(*listener); ok {
		return l
	}
	return s.main
}

// checkHost answers 421 to a request on a TCP listener whose Host is not
// one of that listener's (see hostSet). It reports whether the request may
// go on.
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) bool {
	l := s.listenerOf(r)
	if r.Context().Value(socketKey) == true || l.hosts.allows(r.Host, l.tls) {
		return true
	}
	writeError(w, http.StatusMisdirectedRequest, "misdirected_request", "this ESM does not answer for that host name: "+l.hostHint)
	return false
}

// APIListenerInfo is the API listener (api_listener in esm.json): what
// GET /api/v1/box shows of it, and how APIListenerHandler treats requests.
type APIListenerInfo struct {
	// Listen is the address it is bound to, with its real port.
	Listen string `json:"listen"`
	// AllAddresses says that it listens on every address of the box.
	AllAddresses bool `json:"all_addresses"`
	// HostNames are the further names it answers to (host_names).
	HostNames []string `json:"host_names"`
	// Page says that the web page, login and setup are served on it too;
	// without it only API tokens are taken there.
	Page bool `json:"page"`
	// Certificate is the certificate it serves.
	Certificate tlscert.Info `json:"certificate"`
}

// TokensOnly is the code of a refusal on the API listener: a login, a
// setup, the page or a session cookie, which it takes only with
// api_listener.page set.
const TokensOnly = "tokens_only"

// APIListenerHandler serves requests arriving on the API listener
// (Options.APIListener, which must be set). The listener always speaks
// TLS, and its clients reach it directly, never through a proxy ESM is
// told about. Unless Page is set it takes API tokens only: the page,
// login, setup and logout are not served there, and a session cookie is
// refused. Its Host check accepts its host names and its listen address.
// Everything else (roles, limits, deadlines, audit) is as on the main
// listener, and calls answered only on the unix socket stay refused.
func (s *Server) APIListenerHandler() http.Handler {
	o := s.o.APIListener
	l := &listener{hosts: newAPIHostSet(o.Listen, o.HostNames), tls: true, https: true, tokensOnly: !o.Page,
		hostHint: "add the name to host_names in api_listener in esm.json"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.root.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), listenerKey, l)))
	})
}
