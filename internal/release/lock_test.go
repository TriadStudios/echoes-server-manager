package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A self-update holds a lock file next to the binary while it runs. One
// that finds the lock of another update, in this process or another one,
// is refused and changes nothing.
func TestUpdateRefusesWhileAnotherUpdateHoldsTheLock(t *testing.T) {
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	lock := exe + ".update.lock"
	held := []byte("held by another esm\n")
	if err := os.WriteFile(lock, held, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), lock) {
		t.Fatalf("update with the lock held: %v; want ErrBusy naming %s", err, lock)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatalf("binary changed to %q", b)
	}
	if b, _ := os.ReadFile(lock); string(b) != string(held) {
		t.Fatalf("the other update's lock changed to %q", b)
	}
	if len(fr.served) != 0 {
		t.Fatalf("fetched %v although another update runs", fr.served)
	}
}

// A lock older than LockStale was left by an update that did not finish
// (killed, or the machine went down); the next update takes it over and
// removes it when done.
func TestUpdateTakesOverAStaleLock(t *testing.T) {
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	lock := exe + ".update.lock"
	if err := os.WriteFile(lock, []byte("left behind\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-16 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false); err != nil {
		t.Fatalf("update over a stale lock: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary %q", b)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock is still there after the update: %v", err)
	}

	// A lock just under the limit is not taken over.
	exe = oldBinary(t)
	lock = exe + ".update.lock"
	os.WriteFile(lock, []byte("running\n"), 0o600)
	recent := time.Now().Add(-14 * time.Minute)
	os.Chtimes(lock, recent, recent)
	if _, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("update over a 14 minute old lock: %v; want ErrBusy", err)
	}
}

// Two updates of the same binary at once: the second is refused while the
// first runs, the first finishes, and no lock is left behind.
func TestTwoUpdatesAtOnce(t *testing.T) {
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	asked, hold := make(chan struct{}), make(chan struct{})
	name := AssetName(runtime.GOOS, runtime.GOARCH)
	fr.before = func(n string) {
		if n == name {
			close(asked)
			<-hold
		}
	}
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	lock := exe + ".update.lock"
	release := sync.OnceFunc(func() { close(hold) })
	first, firstDone := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(firstDone)
		_, err := c.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
		first <- err
	}()
	// Also when the test ends early: let the first download go on and wait
	// for it, before the server and the folder go away.
	t.Cleanup(func() { release(); <-firstDone })
	select {
	case <-asked:
	case err := <-first:
		t.Fatalf("the first update ended early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first update never fetched the binary")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("no lock file while the first update runs: %v", err)
	}
	other := &Client{API: c.API, HTTP: c.HTTP, Key: pub}
	_, err := other.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
	release()
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second update: %v; want ErrBusy", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Fatalf("binary %q", b)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock is still there: %v", err)
	}
}

// Every way an update ends removes its lock, failures included.
func TestUpdateRemovesTheLockOnFailure(t *testing.T) {
	pub, priv := testKey(t)
	fr := signedRelease("v1.1.0", []byte("new binary"), priv, nil)
	_, c := fr.start(t)
	c.Key = pub
	exe := oldBinary(t)
	for what, run := range map[string]func() error{
		"up to date": func() error {
			_, err := c.Update(context.Background(), "v1.1.0", exe, runtime.GOOS, runtime.GOARCH, false)
			return err
		},
		"no release": func() error {
			bad := &Client{API: c.API + "-missing", HTTP: c.HTTP, Key: pub}
			_, err := bad.Update(context.Background(), "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
			return err
		},
		"canceled": func() error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := c.Update(ctx, "v1.0.0", exe, runtime.GOOS, runtime.GOARCH, false)
			return err
		},
	} {
		if err := run(); err == nil {
			t.Fatalf("%s: no error", what)
		}
		entries, _ := os.ReadDir(filepath.Dir(exe))
		if len(entries) != 1 {
			t.Fatalf("%s: files left next to the binary: %v", what, entries)
		}
	}
}
