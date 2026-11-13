package release

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		newer, ok       bool
	}{
		{"v1.2.3", "v1.2.2", true, true},
		{"v1.2.3", "v1.2.3", false, true},
		{"v1.10.0", "v1.9.9", true, true},
		{"v2.0.0", "v10.0.0", false, true},
		{"v1.2.3", "v1.2.3-rc.1", true, true},
		{"v1.2.3-rc.2", "v1.2.3-rc.1", true, true},
		{"v1.2.3-rc.1", "v1.2.3", false, true},
		{"v1.2.3", "dev", false, false},
		{"v1.2.3", "test", false, false},
		{"latest", "v1.0.0", false, false},
	} {
		newer, ok := Newer(c.latest, c.current)
		if newer != c.newer || ok != c.ok {
			t.Errorf("Newer(%q, %q) = %v, %v; want %v, %v", c.latest, c.current, newer, ok, c.newer, c.ok)
		}
	}
	if !ValidTag("v0.1.0") || ValidTag("0.1.0") || ValidTag("v0.1") || ValidTag("v1.2.3;rm") {
		t.Error("ValidTag")
	}
}

// Pre-releases are ordered as semantic versioning says: identifier by
// identifier, numbers as numbers, a number below any identifier with
// letters, and a shorter list below a longer one that starts the same.
func TestNewerOrdersPreReleases(t *testing.T) {
	// Each version is older than every one after it.
	order := []string{
		"v1.0.0-1",
		"v1.0.0-2",
		"v1.0.0-10",
		"v1.0.0-10a",
		"v1.0.0-9z",
		"v1.0.0-alpha",
		"v1.0.0-alpha.1",
		"v1.0.0-alpha.9",
		"v1.0.0-alpha.10",
		"v1.0.0-alpha.beta",
		"v1.0.0-beta",
		"v1.0.0-beta.2",
		"v1.0.0-beta.11",
		"v1.0.0-rc.1",
		"v1.0.0-rc.9",
		"v1.0.0-rc.10",
		"v1.0.0-rc.10.1",
		"v1.0.0",
		"v1.0.1-rc.1",
	}
	for i, older := range order {
		for j, newer := range order {
			got, ok := Newer(newer, older)
			if !ok || got != (j > i) {
				t.Errorf("Newer(%q, %q) = %v, %v; want %v", newer, older, got, ok, j > i)
			}
		}
	}
	// Leading zeros do not change a number.
	if got, ok := Newer("v1.0.0-rc.010", "v1.0.0-rc.9"); !got || !ok {
		t.Errorf("rc.010 is not newer than rc.9")
	}
	if got, _ := Newer("v1.0.0-rc.01", "v1.0.0-rc.1"); got {
		t.Errorf("rc.01 is newer than rc.1")
	}
	// Numbers longer than an int still compare by value.
	if got, _ := Newer("v1.0.0-rc.100000000000000000000", "v1.0.0-rc.99999999999999999999"); !got {
		t.Errorf("a long number")
	}
}

func TestBuiltinKeyParses(t *testing.T) {
	if len(BuiltinKey()) != ed25519.PublicKeySize {
		t.Fatalf("PublicKey %q is not a base64 Ed25519 public key", PublicKey)
	}
}

func TestParseSums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got, err := ParseSums([]byte("version v1.2.3\n" + sum + "  esm-linux-amd64\n" + sum + " *esm-windows-amd64.exe\r\n\n"))
	if err != nil || got.Version != "v1.2.3" || got.Files["esm-linux-amd64"] != sum || got.Files["esm-windows-amd64.exe"] != sum || len(got.Files) != 2 {
		t.Fatalf("ParseSums: %v %v", got, err)
	}
	for _, bad := range []string{
		"",
		sum + "  esm\n",
		"version 1.2.3\n" + sum + "  esm\n",
		"version v1.2.3 \n" + sum + "  esm\n",
		"version v1.2.3\nxyz  esm\n",
		"version v1.2.3\n" + strings.Repeat("AB", 32) + "  esm\n",
		"version v1.2.3\n" + sum + "  ../esm\n",
		"version v1.2.3\n" + sum + "  dir/esm\n",
		"version v1.2.3\n" + sum + "\n",
		"version v1.2.3\n" + sum + "  esm\n" + sum + "  esm\n",
	} {
		if _, err := ParseSums([]byte(bad)); err == nil {
			t.Errorf("ParseSums(%q) accepted", bad)
		}
	}
}

func TestSignAndVerify(t *testing.T) {
	pub, priv := testKey(t)
	msg := []byte("abc\n")
	sig := Sign(priv, msg)
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, []byte("abd\n"), sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("changed message: %v", err)
	}
	other, _ := testKey(t)
	if err := Verify(other, msg, sig); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("other key: %v", err)
	}
	if err := Verify(pub, msg, []byte("not base64!")); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := ParseSeed("c2hvcnQ="); err == nil {
		t.Fatal("short seed accepted")
	}
}

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// fakeRelease serves a release over TLS on loopback.
type fakeRelease struct {
	tag    string
	files  map[string][]byte // asset name -> content
	served map[string]bool
}

func (f *fakeRelease) start(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	f.served = map[string]bool{}
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			rel := Release{Tag: f.tag, URL: srv.URL + "/page"}
			for name := range f.files {
				rel.Assets = append(rel.Assets, Asset{Name: name, URL: srv.URL + "/files/" + name})
			}
			json.NewEncoder(w).Encode(rel)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/files/")
		b, ok := f.files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.served[name] = true
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &Client{API: srv.URL + "/latest", HTTP: srv.Client()}
}

// signedRelease makes a release whose binary for this system is bin, with
// SHA256SUMS listing sums (nil: the true sum) signed by priv.
func signedRelease(tag string, bin []byte, priv ed25519.PrivateKey, sums map[string]string) *fakeRelease {
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	if sums == nil {
		h := sha256.Sum256(bin)
		sums = map[string]string{name: hex.EncodeToString(h[:])}
	}
	var b strings.Builder
	b.WriteString(VersionPrefix + tag + "\n")
	for n, s := range sums {
		fmt.Fprintf(&b, "%s  %s\n", s, n)
	}
	return &fakeRelease{tag: tag, files: map[string][]byte{
		name:     bin,
		SumsName: []byte(b.String()),
		SigName:  Sign(priv, []byte(b.String())),
	}}
}

func oldBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "esm"+map[bool]string{true: ".exe"}[runtime.GOOS == "windows"])
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestUpdateGood(t *testing.T) {
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	if st := c.Check(context.Background(), "v1.0.0"); !st.Available || st.Latest != "v1.1.0" || st.Note != "" {
		t.Fatalf("check: %+v", st)
	}
	exe := oldBinary(t)
	res, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "v1.0.0" || res.To != "v1.1.0" || res.Previous != exe+".previous" {
		t.Fatalf("result %+v", res)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary now %q", b)
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "old binary" {
		t.Fatalf("previous %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".esm-update-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// Up to date now, and a development build needs force.
	if _, err := c.Update(context.Background(), "v1.1.0", exe, runtime.GOOS, runtime.GOARCH, false); !errors.Is(err, ErrUpToDate) {
		t.Fatalf("same version: %v", err)
	}
	if _, err := c.Update(context.Background(), "dev", exe, runtime.GOOS, runtime.GOARCH, false); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("dev: %v", err)
	}
	if _, err := c.Update(context.Background(), "dev", exe, runtime.GOOS, runtime.GOARCH, true); err != nil {
		t.Fatalf("dev with force: %v", err)
	}
}

// refused runs an update that must fail with a message containing want and
// leave the old binary in place.
func refused(t *testing.T, c *Client, want string) {
	t.Helper()
	exe := oldBinary(t)
	_, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("update: %v; want an error with %q", err, want)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatalf("binary changed to %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Fatalf("files left next to the binary: %v", entries)
	}
}

func TestUpdateBadSignature(t *testing.T) {
	_, priv := testKey(t)
	builtin, _ := testKey(t) // the release is signed by another key
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = builtin
	refused(t, c, "does not verify")
	if fr.served[AssetName(runtime.GOOS, runtime.GOARCH)] {
		t.Error("the binary was downloaded although the signature failed")
	}
}

func TestUpdateBadHash(t *testing.T) {
	pub, priv := testKey(t)
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	h := sha256.Sum256([]byte("the binary that was signed"))
	fr := signedRelease("v1.1.0", []byte("a binary swapped in later"), priv, map[string]string{name: hex.EncodeToString(h[:])})
	_, c := fr.start(t)
	c.Key = pub
	refused(t, c, "it was not used")
}

func TestUpdateWrongAsset(t *testing.T) {
	pub, priv := testKey(t)
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	// The signed list names only another system's binary.
	h := sha256.Sum256([]byte("new binary"))
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, map[string]string{"esm-plan9-arm": hex.EncodeToString(h[:])})
	_, c := fr.start(t)
	c.Key = pub
	refused(t, c, "does not list "+name)

	// No binary for this system at all.
	fr = signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	delete(fr.files, name)
	_, c = fr.start(t)
	c.Key = pub
	refused(t, c, "no binary for")

	// No signature file.
	fr = signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	delete(fr.files, SigName)
	_, c = fr.start(t)
	c.Key = pub
	refused(t, c, "cannot be checked")
}

func TestOnlyHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a plain http URL was fetched")
	}))
	defer srv.Close()
	c := &Client{API: srv.URL + "/latest"}
	if _, err := c.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "only https") {
		t.Fatalf("http: %v", err)
	}
}

func TestLatestRefusesOddTags(t *testing.T) {
	fr := &fakeRelease{tag: "v1.0.0; rm -rf", files: map[string][]byte{}}
	_, c := fr.start(t)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("odd tag accepted")
	}
}

func TestCheckerDisabled(t *testing.T) {
	k := NewChecker(&Client{API: "https://127.0.0.1:1/never"}, "v1.0.0", 0, false, "update_check is off")
	if st := k.Status(); st.Enabled || st.Note != "update_check is off" || st.Current != "v1.0.0" {
		t.Fatalf("%+v", st)
	}
}
