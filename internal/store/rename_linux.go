package store

import (
	"syscall"
)

// renameInDir renames oldBase to newBase inside dir (root-relative). The
// directory is opened through the root and the rename is done relative to
// that directory handle, so it cannot land outside the root even if a parent
// directory is swapped for a symlink at the same moment.
func (s *Store) renameInDir(dir, oldBase, newBase string) error {
	d, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	fd := int(d.Fd())
	if err := syscall.Renameat(fd, oldBase, fd, newBase); err != nil {
		return err
	}
	return d.Sync()
}
