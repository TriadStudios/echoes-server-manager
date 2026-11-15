package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// MaxHostNames is the most host_names an API listener may have.
const MaxHostNames = 64

// APIListener is a second TCP listener of ESM for the API alone, so that a
// control panel, a bot or a website on another machine or in a container
// reaches ESM without a reverse proxy in front of it. It always speaks TLS,
// and it takes API tokens only unless Page is set. It is off unless
// esm.json has an api_listener object, which ESM never writes itself.
type APIListener struct {
	// Listen is the IP address of this box and the port it binds, like
	// 192.0.2.10:8743. There is no default. Every address of the box
	// (0.0.0.0, [::] or an empty host) is refused unless AllAddresses is
	// set too.
	Listen string `json:"listen"`
	// AllAddresses must be set for a Listen on every address of the box,
	// so that is never the default and never an accident.
	AllAddresses bool `json:"all_addresses,omitempty"`
	// TLSCert and TLSKey are a certificate (PEM, the server's own first,
	// then the chain) and its key from the operator, set together; a
	// relative path is taken from the root. Without them ESM makes a
	// self-signed certificate once and keeps it in secrets/.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// HostNames are the names and addresses clients use for the listener
	// (host for any port, or host:port). With the listen address itself
	// they are the Host values it answers to, and the names a certificate
	// ESM makes is for.
	HostNames []string `json:"host_names,omitempty"`
	// Page serves the web page, with its login and setup, on the listener
	// too. Off by default: then only API tokens are taken there.
	Page bool `json:"page,omitempty"`
}

// EveryAddress reports whether host, the host of a listen address, means
// every address of the box: empty, 0.0.0.0 or ::.
func EveryAddress(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsUnspecified()
}

// Validate checks the listener; mainListen is the listen address of the
// main listener.
func (l *APIListener) Validate(mainListen string) error {
	if l.Listen == "" {
		return errors.New("listen is required: the IP address of this box and the port to listen on, like 192.0.2.10:8743")
	}
	host, port, err := SplitListen(l.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	every := EveryAddress(host)
	switch {
	case every && !l.AllAddresses:
		return fmt.Errorf("listen %q is every address of the box: name one address of this box, or set \"all_addresses\": true as well", l.Listen)
	case !every && l.AllAddresses:
		return fmt.Errorf("all_addresses is set, but listen %q names one address: set listen to 0.0.0.0:<port> or [::]:<port>, or leave all_addresses out", l.Listen)
	case !every && net.ParseIP(host) == nil:
		return fmt.Errorf("listen %q must be an IP address of this box and a port, like 192.0.2.10:8743 (a host name is not bound)", l.Listen)
	}
	if mh, mp, err := SplitListen(mainListen); err == nil && port != 0 && port == mp &&
		(every || EveryAddress(mh) || sameHost(host, mh)) {
		return fmt.Errorf("listen %s is the address of the main listener (listen %s): give the API listener a port of its own", l.Listen, mainListen)
	}
	if (l.TLSCert == "") != (l.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together (or neither, for a certificate ESM makes)")
	}
	for _, p := range []string{l.TLSCert, l.TLSKey} {
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return errors.New("tls_cert and tls_key must be file paths without control characters")
		}
	}
	if len(l.HostNames) > MaxHostNames {
		return fmt.Errorf("host_names: at most %d", MaxHostNames)
	}
	for _, h := range l.HostNames {
		if err := checkHostName(h); err != nil {
			return fmt.Errorf("host_names: %w", err)
		}
	}
	if l.AllAddresses && len(l.HostNames) == 0 {
		return errors.New("with all_addresses, host_names must name the host names or addresses clients use: the listener answers no other")
	}
	return nil
}

// sameHost reports whether two listen hosts are one address ("localhost"
// counts as 127.0.0.1).
func sameHost(a, b string) bool {
	norm := func(h string) string {
		if strings.EqualFold(h, "localhost") {
			return "127.0.0.1"
		}
		if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
			return ip.String()
		}
		return strings.ToLower(h)
	}
	return norm(a) == norm(b)
}

// hostNameRe is a DNS host name: labels of letters, digits and "-" (not
// first or last), each at most 63 characters, with an optional final dot.
var hostNameRe = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// checkHostName checks one host_names entry: a host name or an IP address,
// with an optional port.
func checkHostName(h string) error {
	bad := fmt.Errorf("%q must be a host name or IP address, with an optional :port, without a scheme, path or wildcard", h)
	host := h
	if hh, p, err := net.SplitHostPort(h); err == nil {
		if n, ok := ParsePort(p); !ok || n < 1 {
			return bad
		}
		host = hh
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ip.IsUnspecified() {
			return fmt.Errorf("%q is every address, not one a client can use", h)
		}
		return nil
	}
	if host == "" || len(host) > 253 || !hostNameRe.MatchString(host) {
		return bad
	}
	return nil
}

// CertNames are the names a certificate of the listener must be for: the
// host names of HostNames (without a port, lower case) and the IP
// addresses among them, and the listen address when it is one address.
func (l *APIListener) CertNames() (dns []string, ips []net.IP) {
	addIP := func(ip net.IP) {
		if !slices.ContainsFunc(ips, ip.Equal) {
			ips = append(ips, ip)
		}
	}
	if host, _, err := SplitListen(l.Listen); err == nil && !EveryAddress(host) {
		if ip := net.ParseIP(host); ip != nil {
			addIP(ip)
		}
	}
	for _, h := range l.HostNames {
		host := h
		if hh, _, err := net.SplitHostPort(h); err == nil {
			host = hh
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); ip != nil {
			addIP(ip)
			continue
		}
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if host != "" && !slices.Contains(dns, host) {
			dns = append(dns, host)
		}
	}
	return dns, ips
}

// CertFiles returns the absolute paths of TLSCert and TLSKey ("" when they
// are not set); a relative path is taken from root.
func (l *APIListener) CertFiles(root string) (cert, key string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	return abs(l.TLSCert), abs(l.TLSKey)
}
