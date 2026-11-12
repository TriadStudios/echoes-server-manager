//go:build !linux

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// renameInDir renames oldBase to newBase inside dir (root-relative).
//
// os.Root has no rename in the Go version ESM targets, so outside Linux the
// rename is done by path after checking, through the root, that every
// directory on the way is a real directory (not a symlink or junction).
func (s *Store) renameInDir(dir, oldBase, newBase string) error {
	cur := ""
	if dir != "." {
		for _, part := range strings.Split(dir, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			fi, err := s.root.Lstat(cur)
			if err != nil {
				return err
			}
			if !fi.IsDir() || fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
				return fmt.Errorf("%s is not a plain directory", filepath.ToSlash(cur))
			}
		}
	}
	if fi, err := s.root.Lstat(filepath.Join(dir, newBase)); err == nil && fi.IsDir() {
		return fmt.Errorf("%s is a directory", filepath.ToSlash(filepath.Join(dir, newBase)))
	}
	oldPath := filepath.Join(s.path, dir, oldBase)
	newPath := filepath.Join(s.path, dir, newBase)
	// Two replaces of the same file at once fight each other on Windows;
	// take turns within this process.
	unlock := s.Lock("\x00replace:" + newPath)
	defer unlock()
	var err error
	delay := time.Millisecond
	for deadline := time.Now().Add(5 * time.Second); ; {
		err = os.Rename(oldPath, newPath)
		if err == nil || runtime.GOOS != "windows" || !retryable(err) || time.Now().After(deadline) {
			return err
		}
		// On Windows a reader holding the target open blocks the
		// replacement for a moment; try again shortly.
		time.Sleep(delay)
		delay = min(2*delay, 50*time.Millisecond)
	}
}

// retryable reports access-denied and sharing-violation errors, which on
// Windows usually mean another handle is briefly open on the target.
func retryable(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return true
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 32 // ERROR_SHARING_VIOLATION
}
