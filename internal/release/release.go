// Package release finds new ESM releases and puts a newer esm binary in
// place of the running one.
//
// A release is published on GitHub with one binary per system
// (AssetName), a SHA256SUMS file that lists their SHA-256 sums, and
// SHA256SUMS.sig, an Ed25519 signature of SHA256SUMS. ESM takes a binary
// only when the signature verifies with the public key built into ESM
// (PublicKey) and the binary's own SHA-256 is the one the signed list
// names. Everything is fetched over https, with time limits and size caps.
// Nothing is ever replaced automatically: an update runs only when a
// person asks for it.
package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repo is the GitHub repository ESM releases come from.
const Repo = "TriadStudios/echoes-server-manager"

// DefaultAPI is where the newest release is looked up.
const DefaultAPI = "https://api.github.com/repos/" + Repo + "/releases/latest"

// PublicKey is the base64 Ed25519 public key that signs every release's
// SHA256SUMS. The private half is kept by the project owner, outside the
// repository, and reaches the release workflow as a secret.
const PublicKey = "EUMNTA3aSwhquvpDj+iD59eFAonmvKFROj/RbZyZksI="

// File names of a release besides the binaries.
const (
	SumsName = "SHA256SUMS"
	SigName  = "SHA256SUMS.sig"
)

// Size caps of what is fetched.
const (
	maxAPIBytes  = 1 << 20
	maxSumsBytes = 64 << 10
	maxSigBytes  = 1 << 10
	// MaxBinaryBytes is the largest binary an update downloads.
	MaxBinaryBytes = 256 << 20
)

// Time limits.
const (
	// APITimeout bounds a lookup of the newest release and the download of
	// the small files.
	APITimeout = 30 * time.Second
	// DownloadTimeout bounds the download of a binary.
	DownloadTimeout = 10 * time.Minute
)

// AssetName is the name of the release binary for a system: esm-linux-amd64,
// esm-windows-amd64.exe.
func AssetName(goos, goarch string) string {
	n := "esm-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// Asset is one file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release is what GitHub says about a release.
type Release struct {
	Tag    string  `json:"tag_name"`
	URL    string  `json:"html_url"`
	Assets []Asset `json:"assets"`
}

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// --- versions ---

var versionRe = regexp.MustCompile(`^v?([0-9]{1,9})\.([0-9]{1,9})\.([0-9]{1,9})(?:-([0-9A-Za-z.]{1,64}))?$`)

type version struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (version, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range 3 {
		v.n[i], _ = strconv.Atoi(m[i+1])
	}
	v.pre = m[4]
	return v, true
}

// ValidTag reports whether tag is a release tag such as v1.2.3 or v1.2.3-rc.1.
func ValidTag(tag string) bool { return strings.HasPrefix(tag, "v") && versionRe.MatchString(tag) }

// Newer reports whether latest is a newer version than current. ok is false
// when either is not a version like v1.2.3 (a development build is "dev").
func Newer(latest, current string) (newer, ok bool) {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false, false
	}
	for i := range 3 {
		if l.n[i] != c.n[i] {
			return l.n[i] > c.n[i], true
		}
	}
	switch {
	case l.pre == c.pre:
		return false, true
	case l.pre == "":
		return true, true // a release is newer than its pre-releases
	case c.pre == "":
		return false, true
	}
	return l.pre > c.pre, true
}

// --- keys and signatures ---

// ParseSeed reads a private key: the base64 of its 32-byte seed. Space
// around it is ignored.
func ParseSeed(text string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, errors.New("the key must be the base64 of a 32-byte Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// ParsePublicKey reads a base64 Ed25519 public key.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(b), nil
}

// NewKey makes a new key pair from 32 random bytes.
func NewKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv, nil
}

// BuiltinKey is PublicKey, parsed.
func BuiltinKey() ed25519.PublicKey {
	k, err := ParsePublicKey(PublicKey)
	if err != nil {
		return nil
	}
	return k
}

// Sign returns the signature file content for msg: the base64 signature and
// a newline.
func Sign(key ed25519.PrivateKey, msg []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(key, msg)) + "\n")
}

// ErrBadSignature is returned when a signature does not verify.
var ErrBadSignature = errors.New("the signature of " + SumsName + " does not verify with the key built into ESM")

// Verify checks a signature file made by Sign.
func Verify(pub ed25519.PublicKey, msg, sigFile []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("no valid public key is built in")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigFile)))
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// ParseSums reads a SHA256SUMS file: lines "<64 hex>  <name>" (a "*" before
// the name, binary mode, is allowed). Names must be plain file names and
// appear once.
func ParseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		name = strings.TrimPrefix(strings.TrimPrefix(name, " "), "*")
		if !ok || len(sum) != 64 || strings.ToLower(sum) != sum || name == "" || strings.ContainsAny(name, "/\\ \t") {
			return nil, fmt.Errorf("%s line %d is not \"<sha256>  <name>\"", SumsName, n)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("%s line %d is not \"<sha256>  <name>\"", SumsName, n)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%s names %s twice", SumsName, name)
		}
		out[name] = sum
	}
	return out, sc.Err()
}

// --- fetching ---

// Client looks up and fetches releases.
type Client struct {
	// API is the URL of the newest release (default DefaultAPI). It must be
	// https.
	API string
	// HTTP is the client used (default: one that follows https redirects
	// only).
	HTTP *http.Client
	// Key verifies SHA256SUMS (default BuiltinKey).
	Key ed25519.PublicKey
	// UserAgent is sent with every request (GitHub wants one).
	UserAgent string
}

func (c *Client) api() string {
	if c.API == "" {
		return DefaultAPI
	}
	return c.API
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{CheckRedirect: httpsOnly}
}

func (c *Client) key() ed25519.PublicKey {
	if c.Key != nil {
		return c.Key
	}
	return BuiltinKey()
}

func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return errors.New("redirect to a non-https URL refused")
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// get fetches an https URL into w, at most limit bytes.
func (c *Client) get(ctx context.Context, raw string, limit int64, w io.Writer) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("refused URL %q: only https URLs without user names are fetched", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "esm"
	}
	req.Header.Set("User-Agent", ua)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("%s is larger than %d bytes", u.Path, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s is larger than %d bytes", u.Path, limit)
	}
	return nil
}

func (c *Client) getSmall(ctx context.Context, raw string, limit int64) ([]byte, error) {
	var b bytes.Buffer
	if err := c.get(ctx, raw, limit, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Latest looks up the newest release.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, APITimeout)
	defer cancel()
	b, err := c.getSmall(ctx, c.api(), maxAPIBytes)
	if err != nil {
		return Release{}, fmt.Errorf("look up the newest release: %w", err)
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, fmt.Errorf("look up the newest release: the answer is not a release: %w", err)
	}
	if !ValidTag(r.Tag) {
		return Release{}, fmt.Errorf("the newest release has the tag %q, which is not a version like v1.2.3", r.Tag)
	}
	return r, nil
}

// Status is the result of a check for a newer release.
type Status struct {
	// Enabled is false when update_check is off in esm.json.
	Enabled bool `json:"enabled"`
	// Checked is when the newest release was last looked up.
	Checked time.Time `json:"checked,omitzero"`
	// Current is this ESM's version, Latest the newest release's tag ("" when
	// not known).
	Current string `json:"current"`
	Latest  string `json:"latest"`
	// Available says that Latest is newer than Current.
	Available bool `json:"available"`
	// URL is the newest release's page.
	URL string `json:"url"`
	// Note says why Available cannot be known, or what failed.
	Note string `json:"note"`
}

// Check looks up the newest release and compares it with current.
func (c *Client) Check(ctx context.Context, current string) Status {
	st := Status{Enabled: true, Current: current, Checked: time.Now().UTC()}
	r, err := c.Latest(ctx)
	if err != nil {
		st.Note = err.Error()
		return st
	}
	st.Latest, st.URL = r.Tag, r.URL
	newer, ok := Newer(r.Tag, current)
	switch {
	case !ok:
		st.Note = fmt.Sprintf("this ESM is version %q, not a release version; it cannot tell whether %s is newer", current, r.Tag)
	case newer:
		st.Available = true
	}
	return st
}
