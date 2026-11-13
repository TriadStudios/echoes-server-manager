package release

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Result says what an update did.
type Result struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Binary is the path of the replaced binary, Previous where the old one
	// was kept (to roll back: move it back over Binary).
	Binary   string `json:"binary"`
	Previous string `json:"previous"`
}

// Errors of an update that are the caller's to explain.
var (
	// ErrUpToDate: the newest release is not newer than this ESM.
	ErrUpToDate = errors.New("this ESM is the newest release already")
	// ErrUnknownVersion: this ESM is not a release version, so newer cannot
	// be told.
	ErrUnknownVersion = errors.New("this ESM is not a release version (a development build); update with --force to take the newest release anyway")
	// ErrBusy: another update runs.
	ErrBusy = errors.New("an update is already running")
)

var updating sync.Mutex

// Update replaces the binary at exe with the newest release's binary for
// goos/goarch. The release's SHA256SUMS must carry a valid signature by the
// built-in key and list that binary with the hash it has. The new binary is
// written next to exe and moved in place; the old one is kept as
// exe+".previous". Unless force is set, only a newer release is taken.
func (c *Client) Update(ctx context.Context, current, exe, goos, goarch string, force bool) (Result, error) {
	if !updating.TryLock() {
		return Result{}, ErrBusy
	}
	defer updating.Unlock()
	r, err := c.Latest(ctx)
	if err != nil {
		return Result{}, err
	}
	newer, ok := Newer(r.Tag, current)
	switch {
	case force:
	case !ok:
		return Result{}, ErrUnknownVersion
	case !newer:
		return Result{}, ErrUpToDate
	}
	name := AssetName(goos, goarch)
	bin, ok := r.asset(name)
	if !ok {
		return Result{}, fmt.Errorf("release %s has no %s (no binary for %s/%s)", r.Tag, name, goos, goarch)
	}
	sumsAsset, ok1 := r.asset(SumsName)
	sigAsset, ok2 := r.asset(SigName)
	if !ok1 || !ok2 {
		return Result{}, fmt.Errorf("release %s has no %s or %s; it cannot be checked, so it is not taken", r.Tag, SumsName, SigName)
	}
	small, cancel := context.WithTimeout(ctx, APITimeout)
	defer cancel()
	sums, err := c.getSmall(small, sumsAsset.URL, maxSumsBytes)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", SumsName, err)
	}
	sig, err := c.getSmall(small, sigAsset.URL, maxSigBytes)
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", SigName, err)
	}
	if err := Verify(c.key(), sums, sig); err != nil {
		return Result{}, fmt.Errorf("release %s: %w", r.Tag, err)
	}
	list, err := ParseSums(sums)
	if err != nil {
		return Result{}, fmt.Errorf("release %s: %w", r.Tag, err)
	}
	// The tag comes from the release list, which is not signed; the
	// version in SHA256SUMS is. They must agree, and the signed one decides
	// whether the release is newer.
	if list.Version != r.Tag {
		return Result{}, fmt.Errorf("release %s: the signed %s belongs to %s; it was not taken", r.Tag, SumsName, list.Version)
	}
	if newer, ok := Newer(list.Version, current); !force && (!ok || !newer) {
		if !ok {
			return Result{}, ErrUnknownVersion
		}
		return Result{}, ErrUpToDate
	}
	want, ok := list.Files[name]
	if !ok {
		return Result{}, fmt.Errorf("release %s: the signed %s does not list %s", r.Tag, SumsName, name)
	}
	tmp, err := c.download(ctx, bin.URL, exe, want)
	if err != nil {
		return Result{}, fmt.Errorf("release %s: %w", r.Tag, err)
	}
	prev := exe + ".previous"
	if err := swap(exe, tmp, prev); err != nil {
		os.Remove(tmp)
		return Result{}, err
	}
	return Result{From: current, To: list.Version, Binary: exe, Previous: prev}, nil
}

// download fetches the binary into a new file next to exe and checks its
// hash. It returns the file's path.
func (c *Client) download(ctx context.Context, raw, exe, want string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, DownloadTimeout)
	defer cancel()
	var b [8]byte
	rand.Read(b[:])
	tmp := filepath.Join(filepath.Dir(exe), ".esm-update-"+hex.EncodeToString(b[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return "", fmt.Errorf("cannot write next to %s: %w", exe, err)
	}
	h := sha256.New()
	err = c.get(ctx, raw, MaxBinaryBytes, io.MultiWriter(f, h))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("download the binary: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		os.Remove(tmp)
		return "", fmt.Errorf("the downloaded binary has sha256 %s, but the signed %s says %s; it was not used", got, SumsName, want)
	}
	if fi, err := os.Stat(exe); err == nil && runtime.GOOS != "windows" {
		os.Chmod(tmp, fi.Mode().Perm()|0o500)
	}
	return tmp, nil
}

// swap moves the new binary to exe and keeps the old one as prev. On Linux
// the old binary is first linked to prev and the new one then renamed over
// exe, so exe never goes missing. Windows cannot replace a running
// program's file but can rename it, so there the old one is renamed to
// prev first (and back again if the second step fails).
func swap(exe, tmp, prev string) error {
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the old %s: %w", prev, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Link(exe, prev); err != nil {
			if err := copyFile(exe, prev); err != nil {
				return fmt.Errorf("keep the old binary as %s: %w", prev, err)
			}
		}
		if err := os.Rename(tmp, exe); err != nil {
			return fmt.Errorf("move the new binary in place: %w", err)
		}
		return nil
	}
	if err := os.Rename(exe, prev); err != nil {
		return fmt.Errorf("keep the old binary as %s: %w", prev, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		if rerr := os.Rename(prev, exe); rerr != nil {
			return fmt.Errorf("move the new binary in place: %w (and putting the old one back failed: %v; it is at %s)", err, rerr, prev)
		}
		return fmt.Errorf("move the new binary in place: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// Checker keeps the result of the last check for a newer release and
// checks again, in the background, when it is older than Every.
type Checker struct {
	client  *Client
	current string
	every   time.Duration
	enabled bool
	note    string

	mu      sync.Mutex
	status  Status
	running bool
}

// DefaultCheckEvery is how often the service looks for a newer release at
// most.
const DefaultCheckEvery = 6 * time.Hour

// NewChecker returns a checker. With enabled false it never looks anything
// up, and its status carries note.
func NewChecker(c *Client, current string, every time.Duration, enabled bool, note string) *Checker {
	if every <= 0 {
		every = DefaultCheckEvery
	}
	return &Checker{client: c, current: current, every: every, enabled: enabled, note: note}
}

// Status returns the last result. When there is none, or it is older than
// the check interval, a check starts in the background (the result shows
// in a later call).
func (k *Checker) Status() Status {
	if !k.enabled {
		return Status{Current: k.current, Note: k.note}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.status
	if !k.running && (st.Checked.IsZero() || time.Since(st.Checked) >= k.every) {
		k.running = true
		go func() {
			res := k.client.Check(context.Background(), k.current)
			k.mu.Lock()
			k.status, k.running = res, false
			k.mu.Unlock()
		}()
	}
	if st.Checked.IsZero() {
		st = Status{Enabled: true, Current: k.current, Note: "not checked yet"}
	}
	return st
}

// Client is the checker's client.
func (k *Checker) Client() *Client { return k.client }
