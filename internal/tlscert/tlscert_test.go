package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

var (
	names = []string{"esm.example.com"}
	ips   = []net.IP{net.ParseIP("192.0.2.10")}
)

// The fingerprints are those openssl and curl print and take: SHA-256 hex
// pairs with colons, and sha256// with the base64 SHA-256 of the public key
// (DER SubjectPublicKeyInfo).
func TestFingerprints(t *testing.T) {
	der := []byte("certificate")
	sum := sha256.Sum256(der)
	fp := Fingerprint(der)
	if !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(fp) ||
		strings.ReplaceAll(fp, ":", "") != strings.ToUpper(hex.EncodeToString(sum[:])) {
		t.Fatalf("fingerprint %q", fp)
	}
	spki := []byte("public key")
	sum = sha256.Sum256(spki)
	if pin := SPKIPin(spki); pin != "sha256//"+base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatalf("pin %q", pin)
	}
}

// A certificate ESM makes is made once: a self-signed ECDSA P-256 server
// certificate for the names, valid for ten years, kept in two files only
// its owner may read, and the same one, with the same fingerprints, at the
// next start.
func TestMadeOnceAndKept(t *testing.T) {
	st := openStore(t)
	now := time.Now()
	cert, in, note, err := Made(st, names, ips, now)
	if err != nil {
		t.Fatal(err)
	}
	if note != "there was none yet" {
		t.Errorf("note %q", note)
	}
	leaf := cert.Leaf
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || leaf.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatalf("key %T", leaf.PublicKey)
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil ||
		leaf.Issuer.String() != leaf.Subject.String() {
		t.Fatalf("not self-signed: %v", err)
	}
	if !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) || leaf.IsCA || leaf.KeyUsage != 0 {
		t.Fatalf("usage %v, ca %v, key usage %v", leaf.ExtKeyUsage, leaf.IsCA, leaf.KeyUsage)
	}
	if d := leaf.NotAfter.Sub(now); d < Validity-time.Minute || d > Validity+time.Minute || leaf.NotBefore.After(now) {
		t.Fatalf("valid %s to %s", leaf.NotBefore, leaf.NotAfter)
	}
	if len(Covers(leaf, names, ips)) != 0 || !slices.Equal(in.DNSNames, names) || !slices.Equal(in.IPAddresses, []string{"192.0.2.10"}) {
		t.Fatalf("names %q %q", in.DNSNames, in.IPAddresses)
	}
	if in.Source != SourceMade || in.SHA256 != Fingerprint(leaf.Raw) || in.SPKIPin != SPKIPin(leaf.RawSubjectPublicKeyInfo) {
		t.Fatalf("info %+v", in)
	}
	if want, _ := st.Abs(CertFile); in.File != want {
		t.Fatalf("file %q, want %q", in.File, want)
	}
	// A client that trusts the file alone accepts the certificate for its
	// names, and no other.
	b, err := os.ReadFile(in.File)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		t.Fatal("the certificate file is not PEM")
	}
	for _, n := range []string{"esm.example.com", "192.0.2.10"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: n}); err != nil {
			t.Errorf("verify for %s: %v", n, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.com"}); err == nil {
		t.Error("verified for another name")
	}
	for _, f := range []string{KeyFile, CertFile} {
		fi, err := st.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", f, fi.Mode().Perm())
		}
	}
	if fi, err := st.Stat("secrets"); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("secrets folder: %v %v", fi.Mode(), err)
	}
	if kb, _ := st.ReadFile(KeyFile); !strings.HasPrefix(string(kb), "-----BEGIN PRIVATE KEY-----") {
		t.Errorf("key file: %.40q", kb)
	}

	// The next start: the same certificate, nothing written.
	before, _ := st.ReadFile(CertFile)
	_, again, note, err := Made(st, names, ips, now.Add(24*time.Hour))
	if err != nil || note != "" {
		t.Fatalf("again: %q %v", note, err)
	}
	if again.SHA256 != in.SHA256 || again.SPKIPin != in.SPKIPin {
		t.Fatalf("the certificate changed: %+v, was %+v", again, in)
	}
	if after, _ := st.ReadFile(CertFile); string(after) != string(before) {
		t.Fatal("the certificate file was written again")
	}
	// Fewer names are still covered: kept.
	if _, kept, note, err := Made(st, nil, ips, now); err != nil || note != "" || kept.SHA256 != in.SHA256 {
		t.Fatalf("fewer names: %q %v", note, err)
	}
}

// A certificate that is not for a name of the listener any more, that
// ends within 30 days, or that does not go with its key is made again; a
// key that still reads is kept, so the public key pin stays. A key that
// does not read is made again too, and the pin changes.
func TestMadeAgain(t *testing.T) {
	st := openStore(t)
	now := time.Now()
	_, first, _, err := Made(st, names, ips, now)
	if err != nil {
		t.Fatal(err)
	}
	more := []string{"esm.example.com", "panel.example.org"}
	cert, in, note, err := Made(st, more, ips, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "not for panel.example.org") || !strings.Contains(note, "pin stays the same") {
		t.Errorf("note %q", note)
	}
	if in.SHA256 == first.SHA256 || in.SPKIPin != first.SPKIPin || len(Covers(cert.Leaf, more, ips)) != 0 {
		t.Fatalf("new names: %+v, first %+v", in, first)
	}
	// A new address too.
	other := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::5")}
	if _, in2, note, err := Made(st, more, other, now); err != nil || !strings.Contains(note, "not for 2001:db8::5") || in2.SPKIPin != first.SPKIPin {
		t.Fatalf("new address: %q %v", note, err)
	} else {
		in = in2
	}
	// Near its end.
	late := now.Add(Validity - RenewBefore + time.Hour)
	_, renewed, note, err := Made(st, more, other, late)
	if err != nil || !strings.Contains(note, "certificate ends") || renewed.SHA256 == in.SHA256 || renewed.SPKIPin != first.SPKIPin {
		t.Fatalf("near its end: %q %v", note, err)
	}
	if renewed.NotAfter.Before(late.Add(Validity - time.Minute)) {
		t.Fatalf("renewed until %s", renewed.NotAfter)
	}

	// A certificate of another key.
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf, err := newCert(otherKey, more, other, now)
	if err != nil {
		t.Fatal(err)
	}
	st.WriteFile(CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0o600)
	if _, in, note, err := Made(st, more, other, now); err != nil || !strings.Contains(note, "does not go with the key") || in.SPKIPin != first.SPKIPin {
		t.Fatalf("other key: %q %v", note, err)
	}
	// A broken certificate file.
	st.WriteFile(CertFile, []byte("not pem"), 0o600)
	if _, in, note, err := Made(st, more, other, now); err != nil || !strings.Contains(note, "no PEM certificate") || in.SPKIPin != first.SPKIPin {
		t.Fatalf("broken certificate: %q %v", note, err)
	}
	// A broken key: a new key, so a new pin.
	st.WriteFile(KeyFile, []byte("not pem"), 0o600)
	_, fresh, note, err := Made(st, more, other, now)
	if err != nil || !strings.Contains(note, "no PEM private key") || !strings.Contains(note, "pin changes") || fresh.SPKIPin == first.SPKIPin {
		t.Fatalf("broken key: %q %v", note, err)
	}
	if _, again, note, err := Made(st, more, other, now); err != nil || note != "" || again.SPKIPin != fresh.SPKIPin {
		t.Fatalf("after a new key: %q %v", note, err)
	}
}

// writePair makes a certificate of the operator's (a CA-less self-signed
// one) and writes it with its key.
func writePair(t *testing.T, dir string, dns ...string) (certFile, keyFile string, leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "operator"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), DNSNames: dns,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "op.crt"), filepath.Join(dir, "op.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), 0o600)
	leaf, _ = x509.ParseCertificate(der)
	return certFile, keyFile, leaf
}

// The operator's certificate is used as it is, and a file that cannot be
// read, or a key that is not the certificate's, is a clear error that
// names both files.
func TestOperatorCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, leaf := writePair(t, dir, "esm.example.com")
	cert, in, err := Load(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if in.Source != SourceOperator || in.File != certFile || in.SHA256 != Fingerprint(leaf.Raw) ||
		in.SPKIPin != SPKIPin(leaf.RawSubjectPublicKeyInfo) || cert.Leaf == nil || !slices.Equal(in.DNSNames, []string{"esm.example.com"}) {
		t.Fatalf("info %+v", in)
	}
	if _, _, err := Load(filepath.Join(dir, "missing.crt"), keyFile); err == nil || !strings.Contains(err.Error(), "missing.crt") ||
		!strings.Contains(err.Error(), "op.key") {
		t.Fatalf("missing file: %v", err)
	}
	other := t.TempDir()
	_, otherKey, _ := writePair(t, other, "esm.example.com")
	if _, _, err := Load(certFile, otherKey); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched key: %v", err)
	}
}
