//go:build unix

package service

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// The socket file is never open to other accounts, not even for the moment
// between its creation and the change of its mode: it is made under a
// umask that leaves it to its owner.
func TestSocketIsOwnersOnlyFromTheStart(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	var mu sync.Mutex
	var mode os.FileMode
	socketListened = func(path string) {
		fi, err := os.Lstat(path)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		mode = fi.Mode().Perm()
		mu.Unlock()
	}
	defer func() { socketListened = func(string) {} }()
	root := shortRoot(t)
	writeConfig(t, root, nil)
	s, err := Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}})
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, s)
	mu.Lock()
	defer mu.Unlock()
	if mode&0o077 != 0 {
		t.Fatalf("the socket was made with mode %04o", mode)
	}
	fi, err := os.Lstat(filepath.Join(root, "esm.sock"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v (%v)", fi.Mode(), err)
	}
}

// ESM refuses to put its socket into a folder that other accounts may
// write to.
func TestSocketFolderMustBeOwnersOnly(t *testing.T) {
	root := shortRoot(t)
	writeConfig(t, root, nil)
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Start(Options{Root: root, Version: "test", Stderr: &syncBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "other accounts can write") {
		t.Fatalf("got %v", err)
	}
}
