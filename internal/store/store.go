// Package store keeps ESM's state as plain files under the ESM root.
//
// Every read and write goes through an os.Root opened on the ESM root, so a
// path can never resolve to a place outside it (".." and symlinks that leave
// the root are refused by the operating system layer of os.Root). Writes are
// atomic: the data goes to a temporary file in the same directory, is synced,
// and is then renamed over the target.
//
// Paths given to a Store are relative to the root and use forward slashes,
// for example "instances/alpha/instance.json".
package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Store is a handle on the ESM root directory.
type Store struct {
	path string
	root *os.Root

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Open creates the root directory if needed (mode 0700) and opens it.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: resolve root %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("store: create root: %w", err)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("store: open root: %w", err)
	}
	return &Store{path: abs, root: r, locks: make(map[string]*sync.Mutex)}, nil
}

// Path returns the absolute path of the root directory.
func (s *Store) Path() string { return s.path }

// Close releases the root handle.
func (s *Store) Close() error { return s.root.Close() }

// Lock takes the lock for key and returns the function that releases it.
// Use it around read-modify-write sequences; plain writes are already atomic.
// Locks are not reentrant.
func (s *Store) Lock(key string) (unlock func()) {
	s.mu.Lock()
	m, ok := s.locks[key]
	if !ok {
		m = new(sync.Mutex)
		s.locks[key] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// clean validates a root-relative slash path and returns it in OS form.
func clean(rel string) (string, error) {
	if rel == "" || rel == "." {
		return ".", nil
	}
	if strings.ContainsRune(rel, 0) || strings.Contains(rel, `\`) {
		return "", fmt.Errorf("store: invalid path %q", rel)
	}
	p := filepath.FromSlash(path.Clean(rel))
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("store: path %q is not inside the root", rel)
	}
	return p, nil
}

// Abs returns the absolute OS path of rel after validating it. Use it only to
// hand a path to something outside ESM (a child process, systemd).
func (s *Store) Abs(rel string) (string, error) {
	p, err := clean(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.path, p), nil
}

// Rel converts an absolute path inside the root to a root-relative slash path.
func (s *Store) Rel(abs string) (string, error) {
	r, err := filepath.Rel(s.path, abs)
	if err != nil || !filepath.IsLocal(r) {
		return "", fmt.Errorf("store: %q is not inside the root %q", abs, s.path)
	}
	return filepath.ToSlash(r), nil
}

// ReadFile reads a whole file.
func (s *Store) ReadFile(rel string) ([]byte, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	f, err := s.root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// ReadJSON decodes a JSON file into v. Unknown fields are refused so that a
// typo in a hand-edited file is reported instead of silently ignored.
// A missing file returns an error matching fs.ErrNotExist.
func (s *Store) ReadJSON(rel string, v any) error {
	b, err := s.ReadFile(rel)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("store: decode %s: %w", rel, err)
	}
	return nil
}

// WriteJSON atomically writes v as indented JSON.
func (s *Store) WriteJSON(rel string, v any, perm fs.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encode %s: %w", rel, err)
	}
	b = append(b, '\n')
	return s.WriteFile(rel, b, perm)
}

// WriteFile atomically replaces rel with data. Parent directories are created
// with mode 0700 when missing. perm applies to the new file.
func (s *Store) WriteFile(rel string, data []byte, perm fs.FileMode) error {
	p, err := clean(rel)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("store: cannot write the root itself")
	}
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	if err := s.mkdirAll(dir, 0o700); err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmpBase := "." + base + ".tmp-" + hex.EncodeToString(rnd[:])
	tmp := filepath.Join(dir, tmpBase)
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("store: create temp for %s: %w", rel, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("store: write %s: %w", rel, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: sync %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", rel, err)
	}
	if err := s.renameInDir(dir, tmpBase, base); err != nil {
		return fmt.Errorf("store: replace %s: %w", rel, err)
	}
	ok = true
	return nil
}

// OpenAppend opens rel for appending, creating it with perm when missing.
func (s *Store) OpenAppend(rel string, perm fs.FileMode) (*os.File, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	if err := s.mkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return s.root.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perm)
}

// Create creates a new file for writing (it must not exist yet), creating
// missing parent directories with mode 0700. A perm without write bits
// still gives a writable handle: the file is read-only once closed.
func (s *Store) Create(rel string, perm fs.FileMode) (*os.File, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	if p == "." {
		return nil, fmt.Errorf("store: cannot create the root itself")
	}
	if err := s.mkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return s.root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// Rename moves oldRel to newRel inside the root. A file at newRel is
// replaced; a directory at newRel is refused. The parent of newRel is
// created when missing.
func (s *Store) Rename(oldRel, newRel string) error {
	op, err := clean(oldRel)
	if err != nil {
		return err
	}
	np, err := clean(newRel)
	if err != nil {
		return err
	}
	if op == "." || np == "." {
		return fmt.Errorf("store: cannot rename the root")
	}
	if _, err := s.root.Lstat(op); err != nil {
		return err
	}
	if fi, err := s.root.Lstat(np); err == nil && fi.IsDir() {
		return fmt.Errorf("store: rename %s: %s is a directory: %w", oldRel, newRel, fs.ErrExist)
	}
	if err := s.mkdirAll(filepath.Dir(np), 0o700); err != nil {
		return err
	}
	oldDir, oldBase := filepath.Split(op)
	newDir, newBase := filepath.Split(np)
	if err := s.renameAt(filepath.Clean(oldDir), oldBase, filepath.Clean(newDir), newBase); err != nil {
		return fmt.Errorf("store: rename %s to %s: %w", oldRel, newRel, err)
	}
	return nil
}

// Open opens rel for reading.
func (s *Store) Open(rel string) (*os.File, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	return s.root.Open(p)
}

// Stat returns file information for rel, following symlinks inside the root.
func (s *Store) Stat(rel string) (fs.FileInfo, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	return s.root.Stat(p)
}

// Lstat returns file information for rel without following a final symlink.
func (s *Store) Lstat(rel string) (fs.FileInfo, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	return s.root.Lstat(p)
}

// Exists reports whether rel exists.
func (s *Store) Exists(rel string) bool {
	_, err := s.Lstat(rel)
	return err == nil
}

// ReadDir lists a directory, sorted by name.
func (s *Store) ReadDir(rel string) ([]fs.DirEntry, error) {
	p, err := clean(rel)
	if err != nil {
		return nil, err
	}
	return fs.ReadDir(s.root.FS(), filepath.ToSlash(p))
}

// MkdirAll creates rel and any missing parents with perm.
func (s *Store) MkdirAll(rel string, perm fs.FileMode) error {
	p, err := clean(rel)
	if err != nil {
		return err
	}
	return s.mkdirAll(p, perm)
}

func (s *Store) mkdirAll(p string, perm fs.FileMode) error {
	if p == "." || p == "" {
		return nil
	}
	parts := strings.Split(p, string(filepath.Separator))
	cur := ""
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		err := s.root.Mkdir(cur, perm)
		if err == nil || errors.Is(err, fs.ErrExist) {
			continue
		}
		return fmt.Errorf("store: mkdir %s: %w", filepath.ToSlash(cur), err)
	}
	fi, err := s.root.Stat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("store: %s exists and is not a directory", filepath.ToSlash(p))
	}
	return nil
}

// Remove removes a file or an empty directory. A missing path is not an error.
func (s *Store) Remove(rel string) error {
	p, err := clean(rel)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("store: cannot remove the root")
	}
	if err := s.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveAll removes rel and everything below it. Symlinks are removed, never
// followed. A missing path is not an error. The root itself cannot be removed.
func (s *Store) RemoveAll(rel string) error {
	p, err := clean(rel)
	if err != nil {
		return err
	}
	if p == "." {
		return fmt.Errorf("store: cannot remove the root")
	}
	return s.removeAll(p)
}

func (s *Store) removeAll(p string) error {
	fi, err := s.root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() && fi.Mode()&fs.ModeSymlink == 0 {
		entries, err := fs.ReadDir(s.root.FS(), filepath.ToSlash(p))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := s.removeAll(filepath.Join(p, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := s.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if fi.Mode().Perm()&0o200 == 0 {
			// Read-only files (builds) cannot be deleted on Windows until
			// they are writable again.
			if f, oerr := s.root.OpenFile(p, os.O_RDONLY, 0); oerr == nil {
				cerr := f.Chmod(fi.Mode().Perm() | 0o200)
				f.Close()
				if cerr == nil {
					if err2 := s.root.Remove(p); err2 == nil || errors.Is(err2, fs.ErrNotExist) {
						return nil
					}
				}
			}
		}
		return err
	}
	return nil
}
