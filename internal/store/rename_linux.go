package store

import (
	"syscall"
)

// renameInDir renames oldBase to newBase inside dir (root-relative).
func (s *Store) renameInDir(dir, oldBase, newBase string) error {
	return s.renameAt(dir, oldBase, dir, newBase)
}

// renameAt renames oldDir/oldBase to newDir/newBase (directories
// root-relative). Both directories are opened through the root and the
// rename is done relative to those handles, so it cannot land outside the
// root even if a parent directory is swapped for a symlink at the same
// moment.
func (s *Store) renameAt(oldDir, oldBase, newDir, newBase string) error {
	od, err := s.root.Open(oldDir)
	if err != nil {
		return err
	}
	defer od.Close()
	nd := od
	if newDir != oldDir {
		if nd, err = s.root.Open(newDir); err != nil {
			return err
		}
		defer nd.Close()
	}
	if err := syscall.Renameat(int(od.Fd()), oldBase, int(nd.Fd()), newBase); err != nil {
		return err
	}
	if nd != od {
		od.Sync()
	}
	return nd.Sync()
}
