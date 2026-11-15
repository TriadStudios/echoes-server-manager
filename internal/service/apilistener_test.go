package service

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// pinned is a TLS client config that trusts a server by the public key pin
// of its certificate alone, as curl --pinnedpubkey does.
func pinned(pin string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || tlscert.SPKIPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
			return errors.New("the public key pin does not match")
		}
		return nil
	}}
}

// getSession calls GET /api/v1/session on the API listener with a bearer
// token over the TLS config c.
func getSession(t *testing.T, addr string, c *tls.Config, token string) (int, api.SessionInfo, error) {
	t.Helper()
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: c}, Timeout: 10 * time.Second}
	defer cl.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "https://"+addr+"/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cl.Do(req)
	if err != nil {
		return 0, api.SessionInfo{}, err
	}
	defer resp.Body.Close()
	var si api.SessionInfo
	json.NewDecoder(resp.Body).Decode(&si)
	return resp.StatusCode, si, nil
}

func startWith(t *testing.T, root string, log io.Writer) *Service {
	t.Helper()
	s, err := Start(Options{Root: root, Version: "test", Stderr: log, AuthOptions: auth.Options{Iterations: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// With api_listener set, ESM listens there with TLS only, with a
// certificate it made once: plain http gets no answer of the API, TLS
// below 1.2 is refused, a client that pins the public key (or trusts the
// certificate file, for the listener's names) connects and a wrong pin does
// not. The start log names both fingerprints. The certificate is the same
// after a restart; a new host name makes a new one with the same key, so
// the pin stays. Shutdown closes the listener.
func TestAPIListenerOverTLS(t *testing.T) {
	root := shortRoot(t)
	al := map[string]any{"listen": "127.0.0.1:0", "host_names": []string{"esm.example.com"}}
	writeConfig(t, root, map[string]any{"api_listener": al})
	log := &syncBuffer{}
	s := startWith(t, root, log)
	info := s.APIListener()
	if info == nil || !strings.HasPrefix(info.Listen, "127.0.0.1:") || info.Listen == "127.0.0.1:0" || info.Page ||
		info.Certificate.Source != tlscert.SourceMade {
		t.Fatalf("api listener %+v", info)
	}
	c := info.Certificate
	for _, want := range []string{"API listener started", c.SHA256, c.SPKIPin, "made a certificate for the API listener", "there was none yet"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the start log does not say %q:\n%s", want, log.String())
		}
	}
	admin, _, err := s.auth.CreateToken("panel", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	// A pinned client connects; its token is who it is.
	st, si, err := getSession(t, info.Listen, pinned(c.SPKIPin), admin)
	if err != nil || st != http.StatusOK || si.Kind != "token" || si.Role != "admin" {
		t.Fatalf("pinned client: %d %+v %v", st, si, err)
	}
	// A wrong pin does not get through.
	if _, _, err := getSession(t, info.Listen, pinned("sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="), admin); err == nil {
		t.Fatal("a wrong pin connected")
	}
	// The certificate file is all a client needs to trust, for the
	// listener's names and address; not for others, and not without it.
	b, err := os.ReadFile(c.File)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(b)
	for _, name := range []string{"127.0.0.1", "esm.example.com"} {
		if st, _, err := getSession(t, info.Listen, &tls.Config{RootCAs: pool, ServerName: name}, admin); err != nil || st != http.StatusOK {
			t.Errorf("trusting the file, as %s: %d %v", name, st, err)
		}
	}
	if _, _, err := getSession(t, info.Listen, &tls.Config{RootCAs: pool, ServerName: "other.example.com"}, admin); err == nil {
		t.Error("the certificate was taken for another name")
	}
	if _, _, err := getSession(t, info.Listen, &tls.Config{}, admin); err == nil {
		t.Error("the certificate was taken without trusting it")
	}
	// TLS 1.1 is refused.
	old := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}
	if _, _, err := getSession(t, info.Listen, old, admin); err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("TLS 1.1: %v", err)
	}
	// Plain http gets no answer of the API.
	conn, err := net.DialTimeout("tcp", info.Listen, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "GET /api/v1/session HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", info.Listen, admin)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || strings.Contains(string(body), "token") {
			t.Fatalf("plain http: %d %s", resp.StatusCode, body)
		}
	}
	conn.Close()
	// The main listener knows nothing of the API listener's names.
	if resp, err := http.Get("http://" + s.Addr() + "/api/v1/session"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("main listener: %v %v", resp, err)
	} else {
		resp.Body.Close()
	}

	// Shutdown closes it.
	stop(t, s)
	if c, err := net.DialTimeout("tcp", info.Listen, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("the API listener still takes connections after shutdown")
	}
	for _, f := range []string{tlscert.KeyFile, tlscert.CertFile} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(f))); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}

	// The next start serves the same certificate and says nothing of it.
	log = &syncBuffer{}
	s = startWith(t, root, log)
	if got := s.APIListener().Certificate; got.SHA256 != c.SHA256 || got.SPKIPin != c.SPKIPin {
		t.Fatalf("after a restart: %+v, was %+v", got, c)
	}
	if strings.Contains(log.String(), "made a certificate") {
		t.Fatalf("a certificate was made again:\n%s", log.String())
	}
	if st, _, err := getSession(t, s.APIListener().Listen, pinned(c.SPKIPin), admin); err != nil || st != http.StatusOK {
		t.Fatalf("pinned client after a restart: %d %v", st, err)
	}
	stop(t, s)

	// A new host name: a new certificate for it, with the same key.
	al["host_names"] = []string{"esm.example.com", "panel.example.org"}
	writeConfig(t, root, map[string]any{"api_listener": al})
	log = &syncBuffer{}
	s = startWith(t, root, log)
	defer stop(t, s)
	got := s.APIListener().Certificate
	if got.SHA256 == c.SHA256 || got.SPKIPin != c.SPKIPin || !strings.Contains(log.String(), "not for panel.example.org") {
		t.Fatalf("new name: %+v, was %+v\n%s", got, c, log.String())
	}
	pool = x509.NewCertPool()
	b, _ = os.ReadFile(got.File)
	pool.AppendCertsFromPEM(b)
	if st, _, err := getSession(t, s.APIListener().Listen, &tls.Config{RootCAs: pool, ServerName: "panel.example.org"}, admin); err != nil || st != http.StatusOK {
		t.Fatalf("the new name: %d %v", st, err)
	}
}

// writeOperatorPair writes a certificate and key of the operator's into
// dir: a self-signed one made here, for esm.example.com.
func writeOperatorPair(t *testing.T, dir string) (leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "operator"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), DNSNames: []string{"esm.example.com"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "op.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(filepath.Join(dir, "op.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	leaf, _ = x509.ParseCertificate(der)
	return leaf
}

// With tls_cert and tls_key the API listener serves the operator's
// certificate (paths taken from the root), and ESM makes none. A key that
// is not the certificate's, or a file that is missing, keeps ESM from
// starting with an error that names the files; a certificate that is not
// for the listener's address is logged.
func TestAPIListenerOperatorCertificate(t *testing.T) {
	root := shortRoot(t)
	leaf := writeOperatorPair(t, filepath.Join(root, "certs"))
	al := map[string]any{"listen": "127.0.0.1:0", "tls_cert": "certs/op.crt", "tls_key": "certs/op.key", "host_names": []string{"esm.example.com"}}
	writeConfig(t, root, map[string]any{"api_listener": al})
	log := &syncBuffer{}
	s := startWith(t, root, log)
	info := s.APIListener()
	if info.Certificate.Source != tlscert.SourceOperator || info.Certificate.SHA256 != tlscert.Fingerprint(leaf.Raw) ||
		info.Certificate.File != filepath.Join(root, "certs", "op.crt") {
		t.Fatalf("certificate %+v", info.Certificate)
	}
	if !strings.Contains(log.String(), "not for every name") || !strings.Contains(log.String(), "127.0.0.1") {
		t.Errorf("the log does not say the certificate is not for 127.0.0.1:\n%s", log.String())
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(tlscert.CertFile))); err == nil {
		t.Error("ESM made a certificate of its own")
	}
	admin, _, _ := s.auth.CreateToken("panel", auth.RoleAdmin)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if st, _, err := getSession(t, info.Listen, &tls.Config{RootCAs: pool, ServerName: "esm.example.com"}, admin); err != nil || st != http.StatusOK {
		t.Fatalf("operator's certificate: %d %v", st, err)
	}
	stop(t, s)

	// A key of another certificate.
	other := t.TempDir()
	writeOperatorPair(t, other)
	al["tls_key"] = filepath.Join(other, "op.key")
	writeConfig(t, root, map[string]any{"api_listener": al})
	_, err := Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "api_listener") || !strings.Contains(err.Error(), "op.crt") ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched key: %v", err)
	}
	// A missing file.
	al["tls_key"] = "certs/missing.key"
	writeConfig(t, root, map[string]any{"api_listener": al})
	if _, err := Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}}); err == nil || !strings.Contains(err.Error(), "missing.key") {
		t.Fatalf("missing key: %v", err)
	}
	// Nothing was left listening by the failed starts: a good start works.
	al["tls_key"] = "certs/op.key"
	writeConfig(t, root, map[string]any{"api_listener": al})
	stop(t, startWith(t, root, &syncBuffer{}))
}
