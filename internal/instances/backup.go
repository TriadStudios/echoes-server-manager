package instances

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// Backup reasons.
const (
	ReasonManual     = "manual"
	ReasonScheduled  = "scheduled"
	ReasonPreUpdate  = "pre-update"
	ReasonPreRestore = "pre-restore"
)

// Backup describes one backup: backups/<instance>/<id>/meta.json.
type Backup struct {
	ID        string    `json:"id"`
	Instance  string    `json:"instance"`
	Created   time.Time `json:"created"`
	Reason    string    `json:"reason"`
	Build     string    `json:"build"`
	Files     []string  `json:"files"` // slash paths inside the data folder
	SizeBytes int64     `json:"size_bytes"`
	// SaveConfirmed is true when the server was stopped, or a world save
	// was seen in the server log right before the copy.
	SaveConfirmed bool `json:"save_confirmed"`
}

var backupIDRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z-]{1,32}(-[0-9]{1,4})?$`)

// ValidBackupID reports whether id has the backup id form.
func ValidBackupID(id string) bool { return backupIDRe.MatchString(id) }

func backupDir(name, id string) string { return backupsDir + "/" + name + "/" + id }

// alive reports whether the instance's server process is running.
func (m *Manager) alive(name string) bool {
	pr, err := m.sup.Status(name)
	return err == nil && pr.Running
}

// Backup copies the instance's save files into a new backup. If the server
// is running it first waits (up to backup_save_wait) for its next world
// save.
func (m *Manager) Backup(ctx context.Context, name, reason string) (Backup, error) {
	in, err := m.get(name)
	if err != nil {
		return Backup{}, err
	}
	if reason == "" {
		reason = ReasonManual
	}
	in.op.Lock()
	defer in.op.Unlock()
	return m.backupLocked(context.WithoutCancel(ctx), in, reason, "")
}

// backupLocked makes a backup; protect names a backup that pruning must
// keep. in.op must be held.
func (m *Manager) backupLocked(ctx context.Context, in *rt, reason, protect string) (Backup, error) {
	in.mu.Lock()
	cfg := in.cfg.clone()
	in.mu.Unlock()
	confirmed := true
	if m.alive(in.name) {
		confirmed = false
		if wait := m.cfg.BackupSaveWait.D(); wait > 0 {
			confirmed = m.waitWorldSaved(ctx, in.name, wait)
		}
		if !confirmed {
			m.log.Warn("no world save seen before the backup; copying the save files as they are", "instance", in.name)
		}
	}
	dataAbs, err := m.st.Abs(dataDir(in.name))
	if err != nil {
		return Backup{}, err
	}
	files, err := gameserver.SaveFiles(dataAbs)
	if err != nil {
		return Backup{}, err
	}
	now := m.now()
	id := m.newBackupID(in.name, now, reason)
	tmp := backupsDir + "/" + in.name + "/.tmp-" + id
	defer m.st.RemoveAll(tmp)
	b := Backup{ID: id, Instance: in.name, Created: now, Reason: reason, Build: cfg.Build, Files: []string{}, SaveConfirmed: confirmed}
	for _, f := range files {
		n, err := m.copyFile(dataDir(in.name)+"/"+f, tmp+"/"+f)
		if err != nil {
			return Backup{}, fmt.Errorf("back up %s: %w", f, err)
		}
		b.Files = append(b.Files, f)
		b.SizeBytes += n
	}
	if err := m.st.WriteJSON(tmp+"/meta.json", b, 0o600); err != nil {
		return Backup{}, err
	}
	if err := m.st.Rename(tmp, backupDir(in.name, id)); err != nil {
		return Backup{}, err
	}
	m.log.Info("backup made", "instance", in.name, "id", id, "files", len(b.Files), "save_confirmed", confirmed)
	if err := m.pruneBackups(in.name, cfg.Backup.Keep, id, protect); err != nil {
		m.log.Warn("cannot prune backups", "instance", in.name, "err", err)
	}
	return b, nil
}

// newBackupID returns "<UTC time>-<reason>", with a counter when that id
// is taken.
func (m *Manager) newBackupID(name string, now time.Time, reason string) string {
	base := now.UTC().Format("20060102T150405Z") + "-" + reason
	id := base
	for i := 2; m.st.Exists(backupDir(name, id)) || m.st.Exists(backupsDir+"/"+name+"/.tmp-"+id); i++ {
		id = base + "-" + strconv.Itoa(i)
	}
	return id
}

// copyFile copies src to dst (both root-relative) through a temporary file,
// so dst is never seen half written.
func (m *Manager) copyFile(src, dst string) (int64, error) {
	in, err := m.st.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	tmp := dst + ".tmp-copy"
	m.st.Remove(tmp)
	out, err := m.st.Create(tmp, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = m.st.Rename(tmp, dst)
	}
	if err != nil {
		m.st.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// ListBackups returns the backups of an instance, newest first. Backups of
// deleted instances can still be listed by name.
func (m *Manager) ListBackups(name string) ([]Backup, error) {
	if !ValidName(name) {
		return nil, fault.NotFoundf("instance %q not found", name)
	}
	if m.lookup(name) == nil && !m.st.Exists(backupsDir+"/"+name) {
		return nil, fault.NotFoundf("instance %q not found", name)
	}
	entries, err := m.st.ReadDir(backupsDir + "/" + name)
	if errors.Is(err, fs.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Backup{}
	for _, e := range entries {
		if !e.IsDir() || !ValidBackupID(e.Name()) {
			continue
		}
		var b Backup
		if err := m.st.ReadJSON(backupDir(name, e.Name())+"/meta.json", &b); err != nil {
			m.log.Warn("skipping unreadable backup", "instance", name, "id", e.Name(), "err", err)
			continue
		}
		b.ID = e.Name()
		out = append(out, b)
	}
	slices.SortStableFunc(out, func(a, b Backup) int {
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	return out, nil
}

// pruneBackups removes the oldest backups beyond keep. The ids in protect
// are never removed; they are kept on top of the newest keep backups.
func (m *Manager) pruneBackups(name string, keep int, protect ...string) error {
	if keep < 1 {
		keep = DefaultBackupKeep
	}
	list, err := m.ListBackups(name)
	if err != nil {
		return err
	}
	kept := 0
	for _, b := range list {
		if kept < keep || slices.Contains(protect, b.ID) {
			kept++
			continue
		}
		if err := m.st.RemoveAll(backupDir(name, b.ID)); err != nil {
			return err
		}
		m.log.Info("backup pruned", "instance", name, "id", b.ID)
	}
	return nil
}

// Restore replaces the instance's save files with those of a backup. A
// running server is stopped first and started again afterwards; the
// current save files are backed up first (reason pre-restore).
func (m *Manager) Restore(ctx context.Context, name, id string) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	if !ValidBackupID(id) {
		return Status{}, fault.NotFoundf("backup %q of instance %s not found", id, name)
	}
	var b Backup
	if err := m.st.ReadJSON(backupDir(name, id)+"/meta.json", &b); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Status{}, fault.NotFoundf("backup %q of instance %s not found", id, name)
		}
		return Status{}, err
	}
	if err := m.checkBackupFiles(name, id, b.Files); err != nil {
		return Status{}, err
	}
	ctx = context.WithoutCancel(ctx)
	in.op.Lock()
	defer in.op.Unlock()
	wasRunning := m.alive(name)
	if wasRunning {
		if err := m.stopLocked(ctx, in, "stopped for a restore"); err != nil {
			return m.mustStatus(name), err
		}
	}
	restart := func() error {
		if !wasRunning {
			return nil
		}
		r, err := m.launch(ctx, in, "started after a restore", false)
		if err != nil {
			return err
		}
		return waitRun(ctx, r)
	}
	if _, err := m.backupLocked(ctx, in, ReasonPreRestore, id); err != nil {
		restart()
		return m.mustStatus(name), fmt.Errorf("back up the current save files: %w", err)
	}
	dataAbs, err := m.st.Abs(dataDir(name))
	if err != nil {
		return Status{}, err
	}
	current, err := gameserver.SaveFiles(dataAbs)
	if err != nil {
		return Status{}, err
	}
	for _, f := range current {
		if !slices.Contains(b.Files, f) {
			if err := m.st.Remove(dataDir(name) + "/" + f); err != nil {
				return m.mustStatus(name), err
			}
		}
	}
	for _, f := range b.Files {
		if _, err := m.copyFile(backupDir(name, id)+"/"+f, dataDir(name)+"/"+f); err != nil {
			return m.mustStatus(name), fmt.Errorf("restore %s: %w", f, err)
		}
	}
	m.log.Info("backup restored", "instance", name, "id", id)
	if err := restart(); err != nil {
		return m.mustStatus(name), err
	}
	return m.mustStatus(name), nil
}

// checkBackupFiles makes sure every file a backup's meta.json names is a
// clean relative path that matches the save file pattern and is a regular
// file inside that backup, before anything is stopped or replaced.
func (m *Manager) checkBackupFiles(name, id string, files []string) error {
	damaged := func(format string, args ...any) error {
		return fault.New(fault.Invalid, "backup_damaged", "backup %s of instance %s cannot be restored: "+format, append([]any{id, name}, args...)...)
	}
	for _, f := range files {
		if f == "" || strings.ContainsAny(f, `\:`) || path.Clean(f) != f || !filepath.IsLocal(filepath.FromSlash(f)) {
			return damaged("%q is not a clean relative path", f)
		}
		if ok, err := path.Match(gameserver.SaveGlob, f); err != nil || !ok {
			return damaged("%q is not a save file (%s)", f, gameserver.SaveGlob)
		}
		fi, err := m.st.Lstat(backupDir(name, id) + "/" + f)
		if err != nil || !fi.Mode().IsRegular() {
			return damaged("%q is missing from the backup", f)
		}
	}
	return nil
}

// Update switches an instance to another build of its channel. The save
// files are backed up first; a running server is stopped. The new build is
// started and must become ready; if it does not, the old build is put back
// and started, and the error says so. An instance that was stopped is
// stopped again after the new build proved ready.
func (m *Manager) Update(ctx context.Context, name, to string) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	nb, err := m.builds.Get(to)
	if err != nil {
		if fault.Is(err, fault.NotFound) {
			return Status{}, fault.New(fault.Invalid, "no_build", "build %s is not installed", to)
		}
		return Status{}, err
	}
	ctx = context.WithoutCancel(ctx)
	in.op.Lock()
	defer in.op.Unlock()
	in.mu.Lock()
	cfg := in.cfg.clone()
	prevState := in.status.State
	in.mu.Unlock()
	if cfg.Build == to {
		return m.mustStatus(name), fault.New(fault.Conflict, "same_build", "instance %s already runs build %s", name, to)
	}
	if nb.Channel != cfg.Channel {
		return m.mustStatus(name), fault.New(fault.Invalid, "channel_mismatch", "build %s is a %s build but instance %s is on %s", to, nb.Channel, name, cfg.Channel)
	}
	if prevState == Starting || prevState == Stopping {
		return m.mustStatus(name), fault.New(fault.Conflict, "busy", "instance %s is %s; try again when it is settled", name, prevState)
	}
	in.mu.Lock()
	in.clearCrashesLocked()
	m.setLocked(in, Updating, "updating to "+to)
	in.mu.Unlock()
	wasRunning := m.alive(name)
	if _, err := m.backupLocked(ctx, in, ReasonPreUpdate, ""); err != nil {
		in.mu.Lock()
		m.setLocked(in, prevState, "update to "+to+" not started: backup failed")
		in.mu.Unlock()
		return m.mustStatus(name), fmt.Errorf("pre-update backup: %w", err)
	}
	if wasRunning {
		if err := m.stopLocked(ctx, in, "stopped for an update to "+to); err != nil {
			return m.mustStatus(name), err
		}
	}
	old, oldPrev := cfg.Build, slices.Clone(cfg.PreviousBuilds)
	if err := m.setBuild(in, to, append([]string{old}, oldPrev...)); err != nil {
		return m.mustStatus(name), err
	}
	r, err := m.launch(ctx, in, "updated from "+old+" to "+to, false)
	if err == nil {
		err = waitRun(ctx, r)
	}
	if err == nil {
		if !wasRunning {
			if err := m.stopLocked(ctx, in, "stopped again after the update check"); err != nil {
				return m.mustStatus(name), err
			}
		}
		m.log.Info("instance updated", "instance", name, "from", old, "to", to)
		return m.mustStatus(name), nil
	}
	cause := err
	m.log.Warn("update failed; rolling back", "instance", name, "from", old, "to", to, "err", cause)
	if err := m.stopLocked(ctx, in, "stopped after a failed update"); err != nil {
		return m.mustStatus(name), fault.Wrap(fault.Failed, "update_failed", err, "update to %s failed (%v) and the new build could not be stopped", to, cause)
	}
	if err := m.setBuild(in, old, oldPrev); err != nil {
		return m.mustStatus(name), err
	}
	if wasRunning {
		r, err := m.launch(ctx, in, "rolled back to "+old, false)
		if err == nil {
			err = waitRun(ctx, r)
		}
		if err != nil {
			return m.mustStatus(name), fault.Wrap(fault.Failed, "update_failed", err,
				"update to %s failed (%v) and build %s did not start again", to, cause, old)
		}
	} else {
		in.mu.Lock()
		m.setLocked(in, Stopped, "update to "+to+" failed; still on "+old)
		in.mu.Unlock()
	}
	return m.mustStatus(name), fault.New(fault.Failed, "update_failed", "update failed, rolled back to %s: %v", old, cause)
}
