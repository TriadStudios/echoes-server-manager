package release

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A self-update that puts a new binary in place on Linux carries it through
// its first starts with a few files next to the binary, so that a new ESM
// that does not come up is replaced by the old one without anyone logging in
// to the box. The service unit of esm service install runs a small shell
// script before every start (package svcunit); the script works from these
// files alone, because the new binary may not run at all.
//
//	<binary>.update.pending     the update is not confirmed yet (a Marker)
//	<binary>.update.starts      one line per start since (made by the script)
//	<binary>.update.rolledback  the old binary was put back (made by the script)
//	<binary>.previous           the old binary, kept by the update
//	<binary>.failed             the new binary that did not come up (moved
//	                            aside by the script)
const (
	PendingSuffix    = ".update.pending"
	StartsSuffix     = ".update.starts"
	RolledBackSuffix = ".update.rolledback"
	PreviousSuffix   = ".previous"
	FailedSuffix     = ".failed"
)

// ConfirmAfter is how long a new ESM has to be up, with its listeners
// serving, before it counts as started: it then removes the pending file
// (and the start lines), and the old binary is not put back any more.
const ConfirmAfter = 30 * time.Second

// RollbackStarts is the start, counted from the first start after the
// update, at which the script puts the old binary back instead of starting
// the new one again: the new ESM gets RollbackStarts-1 tries. It is the
// number in the script (svcunit/rollback.sh); a test keeps the two equal.
const RollbackStarts = 3

// Marker is what the pending file holds: which update is waiting to be
// confirmed.
type Marker struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	Time time.Time `json:"time"`
}

// RolledBack is what the script leaves when it put the old binary back: the
// marker of the update it undid (copied as it was) and when it did that.
type RolledBack struct {
	Marker
	// At is when the old binary was put back (zero when the file does not
	// say; see When).
	At time.Time `json:"rolled_back_at"`
	// modified is the file's modification time, which stands in for At.
	modified time.Time
}

// When is when the old binary was put back: the time the script wrote, or
// the file's modification time when it wrote none.
func (r RolledBack) When() time.Time {
	if !r.At.IsZero() {
		return r.At
	}
	return r.modified
}

// maxMarker bounds how much of a marker file is read.
const maxMarker = 4096

// MarkPending records that the update m was put in place and is not confirmed
// yet: it removes the start lines and the record of an earlier rollback, then
// writes the pending file atomically (a file of its own is written and moved
// over the name, so a reader sees all of it or none). exe is the path of the
// binary.
func MarkPending(exe string, m Marker) error {
	for _, suffix := range []string{StartsSuffix, RolledBackSuffix} {
		if err := os.Remove(exe + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove the old %s: %w", exe+suffix, err)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	var rnd [8]byte
	rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(exe), ".esm-update-pending-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write %s: %w", exe+PendingSuffix, err)
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, exe+PendingSuffix)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", exe+PendingSuffix, err)
	}
	return nil
}

// ReadPending reads the pending file of the binary exe. fs.ErrNotExist
// (testable with errors.Is) when no update waits for its confirmation.
func ReadPending(exe string) (Marker, error) {
	b, _, err := readSmall(exe + PendingSuffix)
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, fmt.Errorf("read %s: %w", exe+PendingSuffix, err)
	}
	return m, nil
}

// ConfirmUpdate says that the ESM at exe, whose version is running, is up: it
// removes the pending file and the start lines, so that the service unit's
// script no longer counts starts towards a rollback. It leaves them alone
// when the pending file belongs to an update that this ESM is the old side
// of: it names another version as the one it updated to, and this ESM is
// the one it updated from (an ESM that was started shortly before a command
// line update, and is not the one the update is waiting for). It returns
// whether it removed the pending file; a missing file is not an error.
func ConfirmUpdate(exe, running string) (bool, error) {
	m, err := ReadPending(exe)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err == nil && !sameVersion(m.To, running) && sameVersion(m.From, running):
		return false, nil
	}
	// A pending file that cannot be read is removed too: nothing could say
	// what it waits for.
	if err := os.Remove(exe + PendingSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.Remove(exe + StartsSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	return true, nil
}

// sameVersion compares two versions, ignoring a leading "v".
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "v") == strings.TrimPrefix(strings.TrimSpace(b), "v")
}

// ReadRolledBack reads the record the script left when it put the old binary
// back. ok is false when there is none. A file that is there but cannot be
// read as a record still counts as a rollback: it comes back with ok true,
// the versions empty, and the error that says why.
func ReadRolledBack(exe string) (r RolledBack, ok bool, err error) {
	b, mod, err := readSmall(exe + RolledBackSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		return RolledBack{}, false, nil
	}
	if err != nil {
		return RolledBack{}, true, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return RolledBack{modified: mod}, true, fmt.Errorf("read %s: %w", exe+RolledBackSuffix, err)
	}
	r.modified = mod
	return r, true, nil
}

// readSmall reads a marker file (at most maxMarker bytes) with its
// modification time.
func readSmall(path string) ([]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMarker))
	return b, fi.ModTime(), err
}
