// Package tlscert is the certificate of ESM's API listener: one that ESM
// makes itself, self-signed, once, and keeps under secrets/, or one the
// operator gives; and the two fingerprints a client pins it by.
//
// A certificate ESM makes has an ECDSA P-256 key, is valid for ten years,
// and is for the listener's host names and its listen address. It is made
// again only when it is missing or cannot be read, when it is no longer for
// every one of those names, or when it ends within 30 days. A key that
// still reads is kept for the new certificate, so the public key pin stays
// the same; only the certificate's own fingerprint changes.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// Files of a certificate ESM makes, inside the root (0600, in the 0700
// secrets folder, each written atomically).
const (
	KeyFile  = "secrets/api-listener.key"
	CertFile = "secrets/api-listener.crt"
)

const (
	// Validity is how long a certificate ESM makes is valid.
	Validity = 10 * 365 * 24 * time.Hour
	// RenewBefore is how long before its end a certificate ESM made is
	// made anew.
	RenewBefore = 30 * 24 * time.Hour
	// backdate is how long before it was made a certificate is valid
	// already, for clients whose clocks are behind.
	backdate = time.Hour
	// commonName names the certificates ESM makes.
	commonName = "ESM API listener"
)

// Sources of a certificate.
const (
	SourceMade     = "made"     // made by ESM, self-signed
	SourceOperator = "operator" // tls_cert and tls_key of api_listener
)

// Info describes the certificate a listener serves, as GET /api/v1/box
// shows it. Nothing in it is secret.
type Info struct {
	// Source is SourceMade or SourceOperator.
	Source string `json:"source"`
	// File is the certificate file. For one ESM made, a client may take
	// it as the one certificate it trusts (curl --cacert).
	File string `json:"file"`
	// SHA256 is the SHA-256 of the certificate (DER), as hex pairs with
	// colons, like openssl x509 -fingerprint -sha256 prints it.
	SHA256 string `json:"sha256"`
	// SPKIPin is the SHA-256 of the certificate's public key (its DER
	// SubjectPublicKeyInfo) in base64 after "sha256//", the form curl's
	// --pinnedpubkey takes. It stays the same while the key does.
	SPKIPin   string    `json:"spki_pin"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	// DNSNames and IPAddresses are the names the certificate is for.
	DNSNames    []string `json:"dns_names"`
	IPAddresses []string `json:"ip_addresses"`
}

// Fingerprint is the SHA-256 of der as upper-case hex pairs with colons.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// SPKIPin is the pin of a DER SubjectPublicKeyInfo: "sha256//" and its
// SHA-256 in standard base64.
func SPKIPin(spki []byte) string {
	sum := sha256.Sum256(spki)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

// Describe returns the Info of leaf (without Source and File).
func Describe(leaf *x509.Certificate) Info {
	in := Info{SHA256: Fingerprint(leaf.Raw), SPKIPin: SPKIPin(leaf.RawSubjectPublicKeyInfo),
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(),
		DNSNames: append([]string{}, leaf.DNSNames...), IPAddresses: []string{}}
	for _, ip := range leaf.IPAddresses {
		in.IPAddresses = append(in.IPAddresses, ip.String())
	}
	return in
}

// Covers lists the names among dns and ips that leaf is not for (none:
// it is for all of them).
func Covers(leaf *x509.Certificate, dns []string, ips []net.IP) (missing []string) {
	for _, n := range dns {
		if leaf.VerifyHostname(n) != nil {
			missing = append(missing, n)
		}
	}
	for _, ip := range ips {
		if leaf.VerifyHostname(ip.String()) != nil {
			missing = append(missing, ip.String())
		}
	}
	return missing
}

// Load reads the operator's certificate and key (PEM): the certificate
// file holds the server's own certificate first, then its chain. It fails
// with an error that names both files when either cannot be read or the
// key is not the certificate's.
func Load(certFile, keyFile string) (tls.Certificate, Info, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, Info{}, fmt.Errorf("load tls_cert %s and tls_key %s: %w", certFile, keyFile, err)
	}
	leaf := cert.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, Info{}, fmt.Errorf("load tls_cert %s: %w", certFile, err)
		}
		cert.Leaf = leaf
	}
	in := Describe(leaf)
	in.Source, in.File = SourceOperator, certFile
	return cert, in, nil
}

// Made returns the certificate ESM made for the listener, for the host
// names dns and the addresses ips, making it (and saying why in note) when
// there is none yet, when it cannot be read or does not go with its key,
// when it is not for every one of these names, or when it ends within
// RenewBefore of now. The key is made only when there is no key that
// reads. note is "" when the stored certificate is used as it is.
func Made(st *store.Store, dns []string, ips []net.IP, now time.Time) (cert tls.Certificate, in Info, note string, err error) {
	key, keyWhy := readKey(st)
	var why string
	if key == nil {
		why = keyWhy
	} else if leaf, w := readCert(st, key); leaf == nil {
		why = w
	} else if missing := Covers(leaf, dns, ips); len(missing) > 0 {
		why = "the certificate is not for " + strings.Join(missing, ", ")
	} else if now.After(leaf.NotAfter.Add(-RenewBefore)) {
		why = "the certificate ends " + leaf.NotAfter.UTC().Format("2006-01-02")
	} else if now.Before(leaf.NotBefore) {
		why = "the certificate is not valid before " + leaf.NotBefore.UTC().Format(time.RFC3339)
	} else {
		return pair(st, key, leaf, "")
	}
	note = why + "; the key is kept, so the public key pin stays the same"
	if key == nil {
		note = why
		if keyWhy != noKeyYet {
			note = why + "; with a new key, so the public key pin changes too"
		}
		if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return tls.Certificate{}, Info{}, "", err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return tls.Certificate{}, Info{}, "", err
		}
		if err := st.WriteFile(KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return tls.Certificate{}, Info{}, "", err
		}
	}
	leaf, err := newCert(key, dns, ips, now)
	if err != nil {
		return tls.Certificate{}, Info{}, "", err
	}
	if err := st.WriteFile(CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o600); err != nil {
		return tls.Certificate{}, Info{}, "", err
	}
	return pair(st, key, leaf, note)
}

// noKeyYet is why a first certificate is made.
const noKeyYet = "there was none yet"

// pair puts key and leaf together as the listener serves them.
func pair(st *store.Store, key *ecdsa.PrivateKey, leaf *x509.Certificate, note string) (tls.Certificate, Info, string, error) {
	in := Describe(leaf)
	in.Source = SourceMade
	if p, err := st.Abs(CertFile); err == nil {
		in.File = p
	}
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}, in, note, nil
}

// readKey reads the key ESM made; nil, with why, when there is none that
// reads.
func readKey(st *store.Store) (*ecdsa.PrivateKey, string) {
	b, err := st.ReadFile(KeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, noKeyYet
	}
	if err != nil {
		return nil, "the key cannot be read: " + err.Error()
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, "the key file holds no PEM private key"
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, "the key cannot be read: " + err.Error()
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, "the key is not an ECDSA P-256 key"
	}
	return ek, ""
}

// readCert reads the certificate ESM made and checks that it goes with
// key; nil, with why, when it does not.
func readCert(st *store.Store, key *ecdsa.PrivateKey) (*x509.Certificate, string) {
	b, err := st.ReadFile(CertFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "there was a key, but no certificate"
	}
	if err != nil {
		return nil, "the certificate cannot be read: " + err.Error()
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, "the certificate file holds no PEM certificate"
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, "the certificate cannot be read: " + err.Error()
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, "the certificate does not go with the key"
	}
	return leaf, ""
}

// newCert makes a self-signed certificate of key for dns and ips, for TLS
// servers, valid from a little before now for Validity. It has no key
// usage extension, which RFC 5280 leaves optional outside CA certificates,
// so a client that takes it as its own issuer (curl --cacert) finds no key
// usage to refuse it for.
func newCert(key *ecdsa.PrivateKey, dns []string, ips []net.IP, now time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial.Add(serial, big.NewInt(1)),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-backdate).UTC().Truncate(time.Second),
		NotAfter:              now.Add(Validity).UTC().Truncate(time.Second),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
