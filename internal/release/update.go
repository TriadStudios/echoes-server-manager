package release

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// ErrBusy: another update of the same binary runs, in this process or
	// another one (its lock file is there).
	ErrBusy = errors.New("an update is already running")
)

// LockSuffix names the lock file of a self-update: exe+LockSuffix, next to
// the binary.
const LockSuffix = ".update.lock"

// LockStale is the age at which a lock file counts as left behind by an
// update that never finished (killed, or the machine went down); the next
// update takes it over. An update that runs is done well before that: its
// downloads are bounded by APITimeout and DownloadTimeout.
const LockStale = 15 * time.Minute

// Update replaces the binary at exe with the newest release's binary for
// goos/goarch. The release's SHA256SUMS must carry a valid signature by the
// built-in key and list that binary with the hash it has. The new binary is
// written next to exe and moved in place; the old one is kept as
// exe+".previous". Unless force is set, only a newer release is taken.
//
// While it runs, the update holds the lock file exe+LockSuffix, so a second
// update of the same binary, from this process or another (the command line
// and the service), gets ErrBusy instead of replacing the binary at the same
// time. The lock is removed however the update ends.
func (c *Client) Update(ctx context.Context, current, exe, goos, goarch string, force bool) (Result, error) {
	unlock, err := lockUpdate(exe + LockSuffix)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
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

// --- the update lock ---

// lockUpdate makes the lock file path (O_EXCL, so of two updates only one
// can) and returns the function that removes it again. A lock file that is
// there already belongs to another update: ErrBusy, unless it is LockStale
// old, when it is taken over.
func lockUpdate(path string) (unlock func(), err error) {
	var tok [16]byte
	rand.Read(tok[:])
	mine := fmt.Appendf(nil, "esm self-update\npid %d\nstarted %s\ntoken %s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), hex.EncodeToString(tok[:]))
	for try := 0; ; try++ {
		err := createLock(path, mine)
		if err == nil {
			return func() { removeLock(path, mine) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("cannot make the update lock %s: %w", path, err)
		}
		held, made, err := readLock(path)
		switch {
		case errors.Is(err, fs.ErrNotExist) && try == 0:
			continue // the other update just ended
		case err != nil:
			return nil, fmt.Errorf("%w: cannot read its lock %s: %v", ErrBusy, path, err)
		case time.Since(made) < LockStale || try > 0 || !takeOver(path, held):
			return nil, fmt.Errorf("%w: its lock %s is from %s (an update takes over a lock older than %d minutes)",
				ErrBusy, path, made.UTC().Format(time.RFC3339), int(LockStale.Minutes()))
		}
	}
}

func createLock(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// readLock returns a lock file's content and when it was made, both read
// from the same open file.
func readLock(path string) ([]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	return b, fi.ModTime(), err
}

// takeOver removes the stale lock at path, whose content is stale. It is
// first moved to a name of its own, so that of two updates that find the
// same stale lock only one removes it. When what was moved is not that
// lock (another update took it over and made its own meanwhile), it is put
// back.
func takeOver(path string, stale []byte) bool {
	var b [8]byte
	rand.Read(b[:])
	moved := path + ".stale-" + hex.EncodeToString(b[:])
	if err := os.Rename(path, moved); err != nil {
		return false
	}
	if got, err := os.ReadFile(moved); err != nil || !bytes.Equal(got, stale) {
		os.Rename(moved, path)
		return false
	}
	return os.Remove(moved) == nil
}

// removeLock removes the lock file when it is still this update's.
func removeLock(path string, mine []byte) {
	if b, err := os.ReadFile(path); err == nil && bytes.Equal(b, mine) {
		os.Remove(path)
	}
}

// Checker keeps the result of the last check for a newer release. It
// checks only while the box is in use, that is, while Status was called
// within the last check interval: Status checks again, in the background,
// when the result is that old, and a watcher does the same between calls
// of Status for as long as the box stays in use, then ends. So a call of
// Status within an interval of the one before gets a result at most an
// interval plus one check old. CheckNow checks at once and waits for the
// result. One check runs at a time, and each is bounded by CheckTimeout.
type Checker struct {
	client  *Client
	current string
	every   time.Duration
	enabled bool
	note    string
	// timeout bounds a check, reuse is how long CheckNow gives the last
	// result again instead of asking anew (CheckTimeout and ReuseWithin;
	// the tests make them shorter).
	timeout, reuse time.Duration

	mu       sync.Mutex
	status   Status
	finished time.Time     // when the last check ended
	running  chan struct{} // closed when the check that runs ends; nil when none runs
	looked   time.Time     // when Status was last called
	watching bool          // the watcher runs
}

// DefaultCheckEvery is how often the service looks for a newer release at
// most, unless update_check_every says otherwise.
const DefaultCheckEvery = 5 * time.Minute

// CheckTimeout bounds one look for the newest release (a check in the
// background, and a check by hand that waits for its result).
const CheckTimeout = 15 * time.Second

// ReuseWithin is how long the result of a check is given again to a check
// by hand without asking the release server anew: a burst of clicks makes
// one request.
const ReuseWithin = 30 * time.Second

// NewChecker returns a checker that looks at most every every (default
// DefaultCheckEvery). With enabled false it never looks anything up, and
// its status carries note.
func NewChecker(c *Client, current string, every time.Duration, enabled bool, note string) *Checker {
	if every <= 0 {
		every = DefaultCheckEvery
	}
	return &Checker{client: c, current: current, every: every, enabled: enabled, note: note,
		timeout: CheckTimeout, reuse: ReuseWithin}
}

// off is the status of a checker that never looks anything up.
func (k *Checker) off() Status { return Status{Current: k.current, Note: k.note} }

// Status returns the last result. When there is none, or it is older than
// the check interval, a check starts in the background (the result shows
// in a later call). Until no call of Status has come for an interval, the
// watcher checks again whenever the result turns an interval old.
func (k *Checker) Status() Status {
	if !k.enabled {
		return k.off()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.looked = time.Now()
	st := k.status
	if k.running == nil && k.dueLocked() {
		k.startLocked()
	}
	if !k.watching {
		k.watching = true
		go k.watch()
	}
	if st.Checked.IsZero() {
		st = Status{Enabled: true, Current: k.current, Note: "not checked yet"}
	}
	return st
}

// dueLocked reports whether there is no result yet or it is an interval
// old. k.mu is held.
func (k *Checker) dueLocked() bool {
	return k.status.Checked.IsZero() || time.Since(k.status.Checked) >= k.every
}

// watch keeps the result fresh while the box is in use: it waits for the
// check that runs, or until the result turns an interval old, and then
// checks again. It ends when, at that point, Status has not been called for
// an interval.
func (k *Checker) watch() {
	for {
		k.mu.Lock()
		if time.Since(k.looked) > k.every {
			k.watching = false
			k.mu.Unlock()
			return
		}
		done := k.running
		if done == nil && k.dueLocked() {
			done = k.startLocked()
		}
		wait := time.Until(k.status.Checked.Add(k.every))
		k.mu.Unlock()
		if done != nil {
			<-done
			continue
		}
		time.Sleep(wait)
	}
}

// CheckNow looks for a newer release now and returns the result. When a
// check runs already, it waits for that one instead of starting another;
// when the last check ended less than ReuseWithin ago, it returns that
// result without asking the release server, with Reused set. It returns
// within CheckTimeout, or when ctx ends (then with the last result). With
// the checker off it looks nothing up and returns the off status.
func (k *Checker) CheckNow(ctx context.Context) Status {
	if !k.enabled {
		return k.off()
	}
	k.mu.Lock()
	if k.running == nil && !k.finished.IsZero() && time.Since(k.finished) < k.reuse {
		st := k.status
		age := time.Since(k.finished).Round(time.Second)
		k.mu.Unlock()
		st.Reused = true
		reused := fmt.Sprintf("the last look ended %s ago (less than %s), so its result is given again without asking the release server", age, k.reuse)
		if st.Note == "" {
			st.Note = reused
		} else {
			st.Note += "; " + reused
		}
		return st
	}
	done := k.running
	if done == nil {
		done = k.startLocked()
	}
	k.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.status.Checked.IsZero() {
		return Status{Enabled: true, Current: k.current, Note: "not checked yet"}
	}
	return k.status
}

// startLocked starts a check in the background, bounded by k.timeout, and
// returns the channel that is closed when it ends. k.mu is held.
func (k *Checker) startLocked() chan struct{} {
	done := make(chan struct{})
	k.running = done
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
		res := k.client.Check(ctx, k.current)
		cancel()
		k.mu.Lock()
		k.status, k.finished, k.running = res, time.Now(), nil
		k.mu.Unlock()
		close(done)
	}()
	return done
}

// Client is the checker's client.
func (k *Checker) Client() *Client { return k.client }
