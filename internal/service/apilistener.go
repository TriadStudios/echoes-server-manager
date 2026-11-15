package service

import (
	"crypto/tls"
	"fmt"
	"slices"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/netbind"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// listenAPI binds the API listener (api_listener) with TLS: with the
// operator's certificate (tls_cert, tls_key), read now, or with the one ESM
// made, made first when there is none that fits (see tlscert.Made). It
// fails, and ESM does not start, when the operator's files cannot be read
// or do not go together, or when the address cannot be bound.
func (s *Service) listenAPI(l *config.APIListener) error {
	dns, ips := l.CertNames()
	var (
		cert tls.Certificate
		info tlscert.Info
	)
	if l.TLSCert != "" {
		certFile, keyFile := l.CertFiles(s.cfg.Root)
		c, in, err := tlscert.Load(certFile, keyFile)
		if err != nil {
			return fmt.Errorf("api_listener: %w", err)
		}
		cert, info = c, in
		if missing := tlscert.Covers(c.Leaf, dns, ips); len(missing) > 0 {
			s.log.Warn("the API listener's certificate (tls_cert) is not for every name of the listener: a client that checks the name refuses it for these",
				"names", missing, "file", certFile)
		}
		if now := time.Now(); now.After(c.Leaf.NotAfter) || now.Before(c.Leaf.NotBefore) {
			s.log.Warn("the API listener's certificate (tls_cert) is not valid now", "file", certFile,
				"not_before", c.Leaf.NotBefore, "not_after", c.Leaf.NotAfter)
		}
	} else {
		c, in, note, err := tlscert.Made(s.st, dns, ips, time.Now())
		if err != nil {
			return fmt.Errorf("api_listener: the certificate ESM makes: %w", err)
		}
		cert, info = c, in
		if note != "" {
			s.log.Info("made a certificate for the API listener", "why", note, "file", in.File,
				"sha256", in.SHA256, "spki_pin", in.SPKIPin, "dns_names", in.DNSNames, "ip_addresses", in.IPAddresses)
		}
	}
	ln, err := netbind.Listen("tcp", l.Listen)
	if err != nil {
		return fmt.Errorf("api_listener: listen on %s: %w", l.Listen, err)
	}
	s.apiTCP = tls.NewListener(ln, apiTLSConfig(cert))
	names := slices.Clone(l.HostNames)
	if names == nil {
		names = []string{}
	}
	s.apiInfo = &api.APIListenerInfo{Listen: ln.Addr().String(), AllAddresses: l.AllAddresses, HostNames: names,
		Page: l.Page, Certificate: info}
	return nil
}

// apiTLSConfig is the TLS of the API listener: TLS 1.2 at least, with Go's
// default cipher suites and curves.
func apiTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
}

// takes says what the API listener takes, for the log.
func takes(page bool) string {
	if page {
		return "API tokens, and the page with its logins"
	}
	return "API tokens only"
}
