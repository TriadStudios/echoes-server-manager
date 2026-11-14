// Package steam installs server builds from Steam with SteamCMD.
//
// Each Steam source of esm.json (config.SteamSource) has a persistent cache
// under <root>/steam-cache/<name>/: app/ is SteamCMD's install folder for
// the app (so later downloads are deltas) and home/ is the home folder
// SteamCMD runs with, so its own files stay in the cache. A run writes a
// SteamCMD script into a private folder, runs "steamcmd +runscript <file>"
// (the branch password is in the file, never on a command line) and
// removes the folder again. Then the installed build id is read from
// app/steamapps/appmanifest_<app>.acf. When no build steam-<name>-<buildid>
// is installed yet, the app's files (without steamapps/) are copied into
// the build install area, a manifest is made for them, and they are
// installed read-only like any build. The copy is made only when the file
// system that holds the builds has at least twice the files' size free.
//
// Branch passwords are ESM secrets in <root>/secrets/steam.json (0600).
// They are never returned, logged or audited, and SteamCMD's output is
// masked before ESM keeps any of it.
package steam

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/sysinfo"
)

// Folders and files inside the ESM root.
const (
	CacheDir = "steam-cache"
	// SecretsFile holds the branch passwords.
	SecretsFile = "secrets/steam.json"
	runPrefix   = ".run-"
)

// SourcePrefix marks a build source that is a Steam source: "steam:<name>".
const SourcePrefix = "steam:"

// VersionPrefix starts the version of every build installed from Steam:
// steam-<source>-<buildid>.
const VersionPrefix = "steam-"

// Version is the build version for a source's build id.
func Version(source, buildID string) string { return VersionPrefix + source + "-" + buildID }

// SourceOf returns the source and build id of a build version made by
// Version.
func SourceOf(version string) (source, buildID string, ok bool) {
	rest, ok := strings.CutPrefix(version, VersionPrefix)
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 || !digitsRe.MatchString(rest[i+1:]) || !config.ValidSteamSourceName(rest[:i]) {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// DefaultRunTimeout bounds one SteamCMD run.
const DefaultRunTimeout = 2 * time.Hour

// DefaultTick is how often the check loop looks for sources that are due.
const DefaultTick = 30 * time.Second

// Options configure a Manager.
type Options struct {
	Store  *store.Store
	Config *config.Config
	Builds *builds.Manager
	// Audit records automatic installs and failed automatic checks.
	Audit  *audit.Log
	Logger *slog.Logger
	// Tick is how often the check loop runs (default DefaultTick).
	Tick time.Duration
	// RunTimeout bounds one SteamCMD run (default DefaultRunTimeout).
	RunTimeout time.Duration
	// Installed, when set, is told about every build a source installs.
	Installed func(source string, b builds.Build)
	// DiskSpace reads the size and the free space of the file system that
	// holds a folder (default sysinfo.DiskSpace).
	DiskSpace func(dir string) (total, free uint64, err error)
}

// state is what ESM keeps about a source between runs, in
// steam-cache/<name>.state.json.
type state struct {
	// BuildID is the build id SteamCMD last installed into the cache, and
	// Version the ESM build made of it.
	BuildID string `json:"buildid"`
	Version string `json:"version"`
	// Since is when that build id was first seen.
	Since time.Time `json:"since,omitzero"`
	// Checked is when the last run ended, Error what went wrong in it.
	Checked time.Time `json:"checked,omitzero"`
	Error   string    `json:"error"`
}

func stateFile(name string) string { return CacheDir + "/" + name + ".state.json" }
func logFile(name string) string   { return CacheDir + "/" + name + ".log" }

// Manager runs SteamCMD for the Steam sources.
type Manager struct {
	st     *store.Store
	cfg    *config.Config
	builds *builds.Manager
	opts   Options
	log    *slog.Logger

	// sem lets one SteamCMD run at a time: they share SteamCMD's folder.
	sem chan struct{}

	mu      sync.Mutex
	running map[string]bool

	// noSpaceInfo says once that the free space cannot be read.
	noSpaceInfo sync.Once

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns a Manager. Leftover script folders of an interrupted run are
// removed.
func New(o Options) *Manager {
	if o.Tick <= 0 {
		o.Tick = DefaultTick
	}
	if o.RunTimeout <= 0 {
		o.RunTimeout = DefaultRunTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.DiskSpace == nil {
		o.DiskSpace = sysinfo.DiskSpace
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{st: o.Store, cfg: o.Config, builds: o.Builds, opts: o, log: o.Logger,
		sem: make(chan struct{}, 1), running: map[string]bool{}, ctx: ctx, cancel: cancel}
	if entries, err := m.st.ReadDir(CacheDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), runPrefix) {
				m.st.RemoveAll(CacheDir + "/" + e.Name())
			}
		}
	}
	return m
}

// Start begins checking the sources that have check_every set.
func (m *Manager) Start() {
	if len(m.cfg.SteamSources) == 0 {
		return
	}
	m.wg.Add(1)
	go m.loop()
}

// Close ends the check loop and any SteamCMD run, and waits for them.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) loop() {
	defer m.wg.Done()
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	for {
		for _, src := range m.cfg.SteamSources {
			every := src.Every()
			if every <= 0 {
				continue
			}
			st := m.readState(src.Name)
			if !st.Checked.IsZero() && time.Since(st.Checked) < every {
				continue
			}
			if _, _, err := m.Install(m.ctx, src.Name, false); err != nil && m.ctx.Err() == nil {
				m.log.Warn("Steam check failed", "source", src.Name, "err", err)
			}
		}
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) readState(name string) state {
	var s state
	m.st.ReadJSON(stateFile(name), &s)
	return s
}

func (m *Manager) writeState(name string, s state) {
	if err := m.st.WriteJSON(stateFile(name), s, 0o600); err != nil {
		m.log.Warn("cannot write the Steam source state", "source", name, "err", err)
	}
}

// --- passwords ---

type secrets struct {
	Passwords map[string]string `json:"passwords"`
}

func (m *Manager) readSecrets() (secrets, error) {
	var s secrets
	err := m.st.ReadJSON(SecretsFile, &s)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if s.Passwords == nil {
		s.Passwords = map[string]string{}
	}
	return s, err
}

// SetPassword sets the branch password of a source; "" removes it. The
// value is checked and stored, never repeated.
func (m *Manager) SetPassword(name, pw string) error {
	src, ok := m.cfg.SteamSource(name)
	if !ok {
		return fault.NotFoundf("no Steam source %q in %s", name, config.FileName)
	}
	if src.Branch == "" {
		return fault.Invalidf("Steam source %s has no branch; a password belongs to a beta branch", name)
	}
	if pw != "" {
		if err := CheckPassword(pw); err != nil {
			return fault.New(fault.Invalid, "invalid", "password: %v", err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.readSecrets()
	if err != nil {
		return err
	}
	if pw == "" {
		delete(s.Passwords, name)
	} else {
		s.Passwords[name] = pw
	}
	return m.st.WriteJSON(SecretsFile, s, 0o600)
}

func (m *Manager) password(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.readSecrets()
	return s.Passwords[name], err
}

// --- what the API shows ---

// SourceView is a source as the API shows it. The password is never in
// it, only whether one is set.
type SourceView struct {
	Name        string            `json:"name"`
	AppID       string            `json:"app_id"`
	Branch      string            `json:"branch"`
	Channel     string            `json:"channel"`
	DefaultMap  string            `json:"default_map"`
	Executable  map[string]string `json:"executable"`
	CheckEvery  config.Duration   `json:"check_every"`
	PasswordSet bool              `json:"password_set"`
	// Running says that SteamCMD runs for this source now.
	Running bool `json:"running"`
	// BuildID is the build id last downloaded, Version the ESM build of it
	// (installed unless it was removed since), Since when it was first seen.
	BuildID string    `json:"buildid"`
	Version string    `json:"version"`
	Since   time.Time `json:"since,omitzero"`
	// Checked is when SteamCMD last ran for the source; Error what went
	// wrong then ("" when it worked).
	Checked time.Time `json:"checked,omitzero"`
	Error   string    `json:"error"`
}

// Sources lists the configured sources.
func (m *Manager) Sources() []SourceView {
	out := []SourceView{}
	sec, _ := m.readSecretsLocked()
	for _, s := range m.cfg.SteamSources {
		st := m.readState(s.Name)
		m.mu.Lock()
		running := m.running[s.Name]
		m.mu.Unlock()
		exe := map[string]string{}
		for k, v := range s.Executable {
			exe[k] = v
		}
		out = append(out, SourceView{Name: s.Name, AppID: s.AppID, Branch: s.Branch, Channel: s.Channel, DefaultMap: s.DefaultMap,
			Executable: exe, CheckEvery: config.Duration(s.Every()), PasswordSet: sec.Passwords[s.Name] != "", Running: running,
			BuildID: st.BuildID, Version: st.Version, Since: st.Since, Checked: st.Checked, Error: st.Error})
	}
	return out
}

func (m *Manager) readSecretsLocked() (secrets, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readSecrets()
}

// Target returns the build of a source that SteamCMD saw last, when it is
// installed, and when it was first seen: the build an instance that
// follows the source is updated to.
func (m *Manager) Target(name string) (version string, since time.Time, ok bool) {
	if _, ok := m.cfg.SteamSource(name); !ok {
		return "", time.Time{}, false
	}
	st := m.readState(name)
	if st.Version == "" {
		return "", time.Time{}, false
	}
	if _, err := m.builds.Get(st.Version); err != nil {
		return "", time.Time{}, false
	}
	return st.Version, st.Since, true
}

// --- installs ---

// Install runs SteamCMD for the source and installs its build when no
// build of that build id is installed yet. manual marks a request by a
// person: SteamCMD then also validates every file. It returns the build
// and whether it was installed now (false: it was there already).
func (m *Manager) Install(ctx context.Context, name string, manual bool) (b builds.Build, installed bool, err error) {
	src, ok := m.cfg.SteamSource(name)
	if !ok {
		return builds.Build{}, false, fault.New(fault.Invalid, "no_source", "no Steam source %q in %s", name, config.FileName)
	}
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return builds.Build{}, false, ctx.Err()
	case <-m.ctx.Done():
		return builds.Build{}, false, fault.New(fault.Conflict, "shutting_down", "ESM is shutting down")
	}
	defer func() { <-m.sem }()
	m.mu.Lock()
	m.running[name] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.running, name)
		m.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, m.opts.RunTimeout)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()

	prev := m.readState(name)
	st := prev
	b, installed, buildID, err := m.install(ctx, src, manual)
	st.Checked = time.Now().UTC()
	st.Error = ""
	if err != nil {
		st.Error = err.Error()
	} else {
		if buildID != prev.BuildID || prev.Since.IsZero() {
			st.Since = st.Checked
		}
		st.BuildID, st.Version = buildID, b.Version
	}
	m.writeState(name, st)
	actor := audit.Actor{Kind: audit.ActorSystem, ID: "steam"}
	switch {
	case err != nil && !manual && st.Error != prev.Error:
		// An automatic check that fails is recorded once, not at every
		// interval while it keeps failing the same way.
		m.auditEntry(audit.Entry{Actor: actor, Action: "steam.check", Target: name, Error: st.Error})
	case err == nil && installed && !manual:
		m.auditEntry(audit.Entry{Actor: actor, Action: "steam.install", Target: name + " " + b.Version, OK: true})
	}
	if err == nil && installed {
		m.log.Info("Steam build installed", "source", name, "version", b.Version)
		if m.opts.Installed != nil {
			m.opts.Installed(name, b)
		}
	}
	return b, installed, err
}

func (m *Manager) auditEntry(e audit.Entry) {
	if m.opts.Audit == nil {
		return
	}
	if err := m.opts.Audit.Append(e); err != nil {
		m.log.Error("cannot write the audit log", "err", err)
	}
}

func (m *Manager) install(ctx context.Context, src config.SteamSource, manual bool) (builds.Build, bool, string, error) {
	cache := CacheDir + "/" + src.Name
	for _, d := range []string{cache + "/app", cache + "/home"} {
		if err := m.st.MkdirAll(d, 0o700); err != nil {
			return builds.Build{}, false, "", err
		}
	}
	appAbs, err := m.st.Abs(cache + "/app")
	if err != nil {
		return builds.Build{}, false, "", err
	}
	homeAbs, err := m.st.Abs(cache + "/home")
	if err != nil {
		return builds.Build{}, false, "", err
	}
	pw := ""
	if src.Branch != "" {
		if pw, err = m.password(src.Name); err != nil {
			return builds.Build{}, false, "", err
		}
	}
	script, err := Script(Request{InstallDir: appAbs, AppID: src.AppID, Branch: src.Branch, Password: pw, Validate: manual})
	if err != nil {
		return builds.Build{}, false, "", fault.Invalidf("Steam source %s: %v", src.Name, err)
	}
	if err := m.runSteamCMD(ctx, src, script, homeAbs, pw); err != nil {
		return builds.Build{}, false, "", err
	}
	am, err := m.readAppManifest(cache+"/app", src.AppID)
	if err != nil {
		return builds.Build{}, false, "", fault.Wrap(fault.Failed, "steam_manifest", err, "Steam source %s: SteamCMD finished, but its app manifest", src.Name)
	}
	version := Version(src.Name, am.BuildID)
	if b, err := m.builds.Get(version); err == nil {
		return b, false, am.BuildID, nil
	}
	if err := m.checkSpace(src.Name, version, appAbs); err != nil {
		return builds.Build{}, false, "", err
	}
	b, err := m.builds.InstallFolder(ctx, appAbs, builds.ManifestSpec{
		Version: version, Commit: "steam app " + src.AppID + " build " + am.BuildID, Channel: src.Channel,
		DefaultMap: src.DefaultMap, Executables: src.Executable, Skip: []string{"steamapps"},
	})
	if fault.CodeOf(err) == "build_exists" {
		// Installed meanwhile by another request.
		b, err = m.builds.Get(version)
		return b, false, am.BuildID, err
	}
	if err != nil {
		return builds.Build{}, false, "", err
	}
	return b, true, am.BuildID, nil
}

// spaceFactor is how many times its size a Steam build needs free on the
// file system that holds the builds before it is copied there: once for
// the copy, and as much again left for saves, backups and logs.
const spaceFactor = 2

// checkSpace refuses to install the app's files (in app, without
// steamapps/) as build version when the file system that holds the builds
// has less than spaceFactor times their size free. When the free space
// cannot be read there, the install goes ahead; the log says so once.
func (m *Manager) checkSpace(source, version, app string) error {
	size, err := folderSize(app, "steamapps")
	if err != nil {
		return fmt.Errorf("Steam source %s: size of the app's files: %w", source, err)
	}
	dir := m.st.Path()
	if m.st.Exists(builds.Dir) {
		if dir, err = m.st.Abs(builds.Dir); err != nil {
			return err
		}
	}
	_, free, err := m.opts.DiskSpace(dir)
	if err != nil {
		m.noSpaceInfo.Do(func() {
			m.log.Warn("cannot read the free disk space; Steam builds are installed without checking it", "path", dir, "err", err)
		})
		return nil
	}
	if need := spaceFactor * size; free < need {
		m.log.Warn("Steam build not installed: not enough free disk space", "source", source, "version", version,
			"size_bytes", size, "free_bytes", free, "path", dir)
		return fault.New(fault.Conflict, "not_enough_space",
			"Steam source %s: build %s is %s and needs %s free (twice its size) on the disk that holds %s/, but %s is free; nothing was installed. Free some space (builds no instance uses, old backups) and install again",
			source, version, sizeText(size), sizeText(need), builds.Dir, sizeText(free))
	}
	return nil
}

// folderSize adds up the sizes of the regular files in dir, without the
// top level names in skip.
func folderSize(dir string, skip ...string) (uint64, error) {
	var n uint64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); slices.Contains(skip, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n += uint64(fi.Size())
		return nil
	})
	return n, err
}

// sizeText writes a byte count in binary units, like "6.1 GiB".
func sizeText(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// Surplus lists the installed builds of a source beyond its newest
// keep_steam_builds by build id (compared as numbers), newest first: the
// builds that may be removed after an install from the source, when no
// instance uses them. The build the source installed last is never in it,
// even when its branch went back to an older build id.
func (m *Manager) Surplus(name string) ([]string, error) {
	list, err := m.builds.List()
	if err != nil {
		return nil, err
	}
	type own struct{ id, version string }
	var mine []own
	for _, b := range list {
		if src, id, ok := SourceOf(b.Version); ok && src == name {
			mine = append(mine, own{id, b.Version})
		}
	}
	slices.SortFunc(mine, func(a, b own) int { return compareBuildIDs(b.id, a.id) })
	keep := max(m.cfg.KeepSteamBuilds, 1)
	current := m.readState(name).Version
	out := []string{}
	for i, b := range mine {
		if i >= keep && b.version != current {
			out = append(out, b.version)
		}
	}
	return out, nil
}

// compareBuildIDs compares two build ids (digits) as numbers.
func compareBuildIDs(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if c := cmp.Compare(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// readAppManifest reads and checks the app manifest SteamCMD left.
func (m *Manager) readAppManifest(app, appID string) (AppManifest, error) {
	f, err := m.st.Open(app + "/steamapps/appmanifest_" + appID + ".acf")
	if err != nil {
		return AppManifest{}, fmt.Errorf("cannot be read: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxACFBytes+1))
	if err != nil {
		return AppManifest{}, err
	}
	am, err := ParseAppManifest(b)
	if err != nil {
		return AppManifest{}, err
	}
	if am.AppID != appID {
		return AppManifest{}, fmt.Errorf("names app %s, not %s", am.AppID, appID)
	}
	if !am.FullyInstalled() {
		return AppManifest{}, fmt.Errorf("does not mark the app fully installed (StateFlags %d)", am.StateFlags)
	}
	return am, nil
}

// maxTail is how many lines of SteamCMD's output are kept for an error.
const maxTail = 20

// runSteamCMD writes the script into a private folder, runs SteamCMD with
// it and removes the folder. SteamCMD's output goes, masked, to
// steam-cache/<name>.log.
func (m *Manager) runSteamCMD(ctx context.Context, src config.SteamSource, script []byte, home, pw string) error {
	runDir := CacheDir + "/" + runPrefix + randomHex()
	if err := m.st.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	defer m.st.RemoveAll(runDir)
	if err := m.st.WriteFile(runDir+"/script.txt", script, 0o600); err != nil {
		return err
	}
	scriptAbs, err := m.st.Abs(runDir + "/script.txt")
	if err != nil {
		return err
	}
	exe := m.cfg.SteamCMDPath()
	if fi, err := os.Stat(exe); err != nil || !fi.Mode().IsRegular() {
		return fault.New(fault.Failed, "steamcmd_missing", "SteamCMD is not at %s: install it there (see the README) or set steamcmd in %s", exe, config.FileName)
	}
	cmd := exec.CommandContext(ctx, exe, "+runscript", scriptAbs)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(withoutHome(os.Environ()), "HOME="+home)
	cmd.WaitDelay = 10 * time.Second
	ownGroup(cmd)
	m.st.Remove(logFile(src.Name))
	logf, err := m.st.Create(logFile(src.Name), 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	pr, pwr := io.Pipe()
	cmd.Stdout, cmd.Stderr = pwr, pwr
	var tail []string
	wrongPassword := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			for _, line := range strings.Split(sc.Text(), "\r") {
				line = mask(line, pw)
				if strings.TrimSpace(line) == "" {
					continue
				}
				fmt.Fprintln(logf, line)
				if strings.Contains(strings.ToLower(line), "invalid password") {
					wrongPassword = true
				}
				if tail = append(tail, line); len(tail) > maxTail {
					tail = tail[1:]
				}
			}
		}
		io.Copy(io.Discard, pr)
	}()
	m.log.Info("running SteamCMD", "source", src.Name, "app", src.AppID, "branch", src.Branch)
	runErr := cmd.Run()
	pwr.Close()
	<-done
	if runErr == nil && !hasError(tail) {
		return nil
	}
	if ctx.Err() != nil {
		return fault.New(fault.Failed, "steamcmd_failed", "SteamCMD for %s was stopped (time limit or ESM shutting down)", src.Name)
	}
	if wrongPassword {
		return fault.New(fault.Invalid, "wrong_password", "SteamCMD says the password of branch %s of Steam source %s is wrong; set it again", src.Branch, src.Name)
	}
	why := lastError(tail)
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			why = fmt.Sprintf("exit code %d: %s", ee.ExitCode(), why)
		} else {
			why = runErr.Error()
		}
	}
	return fault.New(fault.Failed, "steamcmd_failed", "SteamCMD failed for Steam source %s (%s); its output is in %s", src.Name, why, logFile(src.Name))
}

// mask hides the password in a line of SteamCMD output.
func mask(line, pw string) string {
	if pw == "" {
		return line
	}
	return strings.ReplaceAll(line, pw, gameserver.Mask)
}

func hasError(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "ERROR!") {
			return true
		}
	}
	return false
}

func lastError(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "ERROR!") {
			return cut(strings.TrimSpace(lines[i]))
		}
	}
	if len(lines) > 0 {
		return cut(strings.TrimSpace(lines[len(lines)-1]))
	}
	return "no output"
}

func cut(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func withoutHome(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "HOME=") {
			out = append(out, e)
		}
	}
	return out
}

func randomHex() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
