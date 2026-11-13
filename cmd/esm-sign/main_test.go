package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

func TestKeygenAndSign(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	var out bytes.Buffer
	if code := run([]string{"-keygen", keyFile}, "", &out, &out); code != 0 {
		t.Fatalf("keygen: %d %s", code, out.String())
	}
	seed, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), strings.TrimSpace(string(seed))) {
		t.Fatal("keygen printed the private seed")
	}
	pub, err := release.ParsePublicKey(out.String())
	if err != nil {
		t.Fatalf("keygen printed %q: %v", out.String(), err)
	}
	if code := run([]string{"-keygen", keyFile}, "", &out, &out); code == 0 {
		t.Fatal("keygen overwrote an existing key file")
	}

	sums := filepath.Join(dir, "SHA256SUMS")
	sig := filepath.Join(dir, "SHA256SUMS.sig")
	os.WriteFile(sums, []byte("version v1.0.0\n"+strings.Repeat("ab", 32)+"  esm-linux-amd64\n"), 0o644)
	if err := sign(sums, sig, "v1.0.0", "", pub); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("no key: %v", err)
	}
	if err := sign(sums, sig, "v1.0.0", string(seed), release.BuiltinKey()); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("a key that is not ESM's: %v", err)
	}
	if _, err := os.Stat(sig); err == nil {
		t.Fatal("a signature was written by a foreign key")
	}
	if err := sign(sums, sig, "v1.0.1", string(seed), pub); err == nil {
		t.Fatal("signed a SHA256SUMS of another version")
	}
	if err := sign(sums, sig, "v1.0.0", string(seed), pub); err != nil {
		t.Fatal(err)
	}
	msg, _ := os.ReadFile(sums)
	s, _ := os.ReadFile(sig)
	if err := release.Verify(pub, msg, s); err != nil {
		t.Fatal(err)
	}
}
