package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// hostSet is the host names the TCP listener answers to. A browser sends
// the name it used in the Host header; a page of another site that points
// its own name at 127.0.0.1 (DNS rebinding) sends that name, so checking
// the header keeps such a page from reaching ESM at all.
type hostSet struct {
	exact   map[string]bool // "host:port", host lower case, IPs normalized
	anyPort map[string]bool // public_hosts entries given without a port
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
	for _, p := range public {
		if h, pt, err := net.SplitHostPort(p); err == nil {
			hs.exact[joinHost(h, pt)] = true
		} else {
			hs.anyPort[normHost(p)] = true
		}
	}
	return hs
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
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return false
	}
	return hs.exact[joinHost(host, port)] || hs.anyPort[normHost(host)]
}

// checkHost answers 421 to a request on the TCP listener whose Host is not
// one of ESM's own (see hostSet). It reports whether the request may go on.
func (s *Server) checkHost(w http.ResponseWriter, r *http.Request) bool {
	if r.Context().Value(socketKey) == true || s.hosts.allows(r.Host, s.o.TLS) {
		return true
	}
	writeError(w, http.StatusMisdirectedRequest, "misdirected_request",
		"this ESM does not answer for that host name: use localhost, or add the name to public_hosts in esm.json")
	return false
}
