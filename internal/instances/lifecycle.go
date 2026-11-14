package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
)

// watchState is what one watcher keeps between checks.
type watchState struct {
	server   tail
	console  tail
	deadline time.Time // ready timeout
	logReady bool      // the ready line was seen in the server log
	bindIP   string    // where this run's query port is probed
	query    int
	adopted  bool // found running at ESM start: never stopped for readiness
	// unsafe marks a server that got its passwords on the command line: its
	// output holds them and stays hidden from the read role, whatever the
	// instance is set to later.
	unsafe bool
}

// launch starts the server of in with its current settings and begins
// watching it. in.op must be held.
//
// The server gets its passwords in a file (runSecretsFile) that is written
// here, right before the start; its command line names the file and holds
// no password. Only an instance set to command-line delivery gets them as
// switches instead; the file is written all the same and then only records
// what the running server was given. ESM never writes the command line
// anywhere: not to its log, not into an error, not into the state.
func (m *Manager) launch(ctx context.Context, in *rt, why string, autoRestart bool) (*run, error) {
	if m.isClosed() {
		return nil, ErrClosed
	}
	in.mu.Lock()
	cfg := in.cfg.clone()
	in.launching = true
	in.mu.Unlock()
	started := false
	defer func() {
		// A start that did not happen leaves no secrets file behind, unless
		// a server of this instance is running after all.
		drop := !started && !m.alive(cfg.Name)
		in.mu.Lock()
		in.launching = false
		if drop {
			m.dropRunSecretsLocked(in)
		}
		in.mu.Unlock()
	}()
	b, err := m.builds.Get(cfg.Build)
	if err != nil {
		if fault.Is(err, fault.NotFound) {
			return nil, fault.New(fault.Conflict, "no_build", "build %s of instance %s is not installed", cfg.Build, cfg.Name)
		}
		return nil, err
	}
	if b.Channel != cfg.Channel {
		return nil, fault.New(fault.Conflict, "channel_mismatch", "build %s is a %s build but instance %s is on %s", b.Version, b.Channel, cfg.Name, cfg.Channel)
	}
	exe, err := m.builds.ExecutablePath(cfg.Build)
	if err != nil {
		return nil, err
	}
	buildAbs, err := m.builds.Path(cfg.Build)
	if err != nil {
		return nil, err
	}
	mapName := cfg.Map
	if mapName == "" {
		mapName = b.DefaultMap
	}
	if mapName == "" {
		return nil, fault.New(fault.Conflict, "no_map", "instance %s names no map and build %s has no default_map; set the instance's map", cfg.Name, cfg.Build)
	}
	if m.alive(cfg.Name) {
		return nil, fault.New(fault.Conflict, "already_running", "a server of instance %s is already running", cfg.Name)
	}
	if err := checkPortsBindable(cfg); err != nil {
		return nil, err
	}
	sec, err := m.readSecrets(cfg.Name)
	if err != nil {
		return nil, err
	}
	given := sec.forServer()
	// The configured bind address is not given to the server (see
	// gameserver.MultihomeSwitch): it only says where ESM checks and
	// probes the ports.
	l, err := m.launchOf(cfg, sec, mapName)
	if err != nil {
		return nil, err
	}
	argv := l.Args()
	// The server's engine stops at once at a command line longer than it
	// keeps. Only settings stored by an earlier version get there: a change
	// is held well below (see checkLine).
	switch n := gameserver.LineLength(argv); {
	case n > gameserver.EngineLineLimit:
		return nil, fault.New(fault.Conflict, "command_line_too_long",
			"the server command line of instance %s has %d characters, and the server's engine does not start with more than %d: shorten or remove rules or extra arguments first",
			cfg.Name, n, gameserver.EngineLineLimit)
	case n > gameserver.MaxLineLength:
		m.log.Warn("the server command line is longer than a change may make it", "instance", cfg.Name, "characters", n,
			"most_after_a_change", gameserver.MaxLineLength, "most_the_engine_starts_with", gameserver.EngineLineLimit)
	}
	if err := m.st.MkdirAll(dataDir(cfg.Name), 0o700); err != nil {
		return nil, err
	}
	if err := m.st.WriteFile(runSecretsFile(cfg.Name), given.File(), 0o600); err != nil {
		return nil, err
	}
	// Every start begins a new server log; the last one is kept beside it.
	if m.st.Exists(serverLog(cfg.Name)) {
		if err := m.st.Rename(serverLog(cfg.Name), previousServerLog(cfg.Name)); err != nil {
			return nil, fmt.Errorf("keep the previous server log: %w", err)
		}
	}
	if err := m.st.MkdirAll(instDir(cfg.Name)+"/logs", 0o700); err != nil {
		return nil, err
	}
	outAbs, _ := m.st.Abs(consoleFile(cfg.Name))
	memMax, _ := ParseMemory(cfg.Limits.MemoryMax)
	// onCommandLine are the passwords that go on the command line: none,
	// unless the instance is set to that.
	var onCommandLine []string
	if cfg.SecretDelivery == gameserver.DeliveryCommandLine {
		onCommandLine = given.Values()
		m.warnUnsafe(cfg.Name)
	}
	// Values stored by an earlier version: an empty one is left out, any
	// other is passed as it is. The log names settings, never values.
	if left := l.LeftOut(); len(left) > 0 {
		m.log.Warn("settings without a value are left off the server command line", "instance", cfg.Name, "left_out", strings.Join(left, ", "))
	}
	if unsafe := cfg.storedUnsafe(sec); len(unsafe) > 0 {
		m.log.Warn(WarningText(WarnUnsafeStoredSettings), "instance", cfg.Name, "settings", strings.Join(unsafe, ", "))
	}
	// A status file from an earlier run must not count as ready.
	if err := m.st.Remove(dataDir(cfg.Name) + "/" + gameserver.StatusFileRel); err != nil {
		return nil, err
	}
	w := &watchState{
		server:  tail{rel: serverLog(cfg.Name)}, // a new file: read from its start
		console: m.tailAt(consoleFile(cfg.Name)),
		bindIP:  cfg.BindIP,
		query:   cfg.Ports.Query,
		unsafe:  cfg.OutputHidden(),
	}
	spec := supervisor.Spec{
		Name: cfg.Name, Executable: exe, Args: argv, Dir: buildAbs, OutputFile: outAbs,
		Env:          m.serverEnv(),
		Limits:       supervisor.Limits{MemoryMaxBytes: memMax, CPUQuotaPercent: cfg.Limits.CPUQuotaPercent},
		StopFile:     gameserver.StopRequestPath(l.UserDir),
		SecretValues: onCommandLine,
	}
	m.log.Info("starting server", "instance", cfg.Name, "build", cfg.Build, "why", why)
	if err := m.sup.Start(context.WithoutCancel(ctx), spec); err != nil {
		if errors.Is(err, supervisor.ErrAlreadyRunning) {
			return nil, fault.New(fault.Conflict, "already_running", "a server of instance %s is already running", cfg.Name)
		}
		// The supervisor does not repeat the command line, but what its
		// tools printed is in this error: mask the passwords that were on
		// the command line, if any, here as well.
		return nil, fault.New(fault.Failed, "start_failed", "cannot start instance %s: %s", cfg.Name,
			audit.Scrub(gameserver.ScrubLine(err.Error(), onCommandLine)))
	}
	started = true
	in.mu.Lock()
	in.started = given.Values()
	in.setFilterLocked()
	in.mu.Unlock()
	if !m.track() {
		// ESM is shutting down: the server keeps running and the next
		// start of ESM adopts it.
		return nil, ErrClosed
	}
	pr, _ := m.sup.Status(cfg.Name)
	now := m.now()
	w.deadline = now.Add(m.cfg.ReadyTimeout.D())
	in.mu.Lock()
	in.gen++
	r := &run{gen: in.gen, autoRestart: autoRestart, done: make(chan struct{})}
	in.cur = r
	in.serverID = ""
	in.status.Build = cfg.Build
	in.status.PID = pr.PID
	in.status.StartedAt = now
	in.status.ReadyAt = time.Time{}
	in.status.ReadySource = ""
	in.status.ExitCode = nil
	in.status.NextRestart = time.Time{}
	in.status.RestartRequired = false
	m.setLocked(in, Starting, why)
	in.mu.Unlock()
	go m.watch(in, r, w)
	return r, nil
}

// launchOf is everything ESM puts on the command line of cfg's server: its
// settings, the folders of the instance and, with command-line delivery,
// the secrets sec. mapName is the map the server loads. Nothing is written
// or looked up for it.
func (m *Manager) launchOf(cfg Instance, sec Secrets, mapName string) (gameserver.Launch, error) {
	var abs [3]string
	for i, rel := range []string{dataDir(cfg.Name), serverLog(cfg.Name), runSecretsFile(cfg.Name)} {
		p, err := m.st.Abs(rel)
		if err != nil {
			return gameserver.Launch{}, err
		}
		abs[i] = p
	}
	name := cfg.DisplayName
	if name == "" {
		name = cfg.Name
	}
	l := gameserver.Launch{
		Map: mapName, GamePort: cfg.Ports.Game, QueryPort: cfg.Ports.Query,
		MaxPlayers: cfg.MaxPlayers, ServerName: name, WorldName: cfg.World, Listed: cfg.Listed, Rules: cfg.Rules,
		UserDir: abs[0], LogFile: abs[1], Delivery: cfg.SecretDelivery, SecretsFile: abs[2],
		ExtraArgs: cfg.ExtraArgs,
	}
	if cfg.SecretDelivery == gameserver.DeliveryCommandLine {
		l.Secrets = sec.forServer()
	}
	return l, nil
}

// maxMapLength is the longest map a build or an instance can name (see
// builds.ValidMap).
const maxMapLength = 128

// lineLength is the length of the command line ESM starts cfg's server
// with, sec being its secrets, as the server's engine counts it (see
// gameserver.LineLength). An instance that names no map loads its build's
// default map; that is looked up only where it can matter, when the line
// without it is within a map's length of gameserver.MaxLineLength.
func (m *Manager) lineLength(cfg Instance, sec Secrets) int {
	l, err := m.launchOf(cfg, sec, cfg.Map)
	if err != nil {
		return 0
	}
	n := gameserver.LineLength(l.Args())
	if cfg.Map == "" && n+maxMapLength > gameserver.MaxLineLength {
		if b, err := m.builds.Get(cfg.Build); err == nil {
			n += gameserver.LineLength([]string{b.DefaultMap}) - 1
		}
	}
	return n
}

// checkLine refuses a change that would make the server command line of an
// instance longer than gameserver.MaxLineLength, which is well below what
// the server's engine starts with. before and beforeSec are the instance
// and its secrets as they are (nil for a new instance): a line that an
// earlier version left too long may stay as long as it is or get shorter,
// and the instance carries WarnCommandLineTooLong. The refusal has the
// lengths and nothing of a value.
func (m *Manager) checkLine(before *Instance, beforeSec Secrets, after Instance, afterSec Secrets) error {
	n := m.lineLength(after, afterSec)
	if n <= gameserver.MaxLineLength || before != nil && n <= m.lineLength(*before, beforeSec) {
		return nil
	}
	return fault.New(fault.Invalid, "command_line_too_long",
		"the server command line of instance %s would have %d characters, more than the %d a change may lead to (the server's engine does not start with more than %d): shorten or remove rules, extra arguments or names, or with secret_delivery %q secrets",
		after.Name, n, gameserver.MaxLineLength, gameserver.EngineLineLimit, gameserver.DeliveryCommandLine)
}

// serverEnv is what ESM adds to a server's environment: the Steam runtime
// folder first on LD_LIBRARY_PATH (gameserver.SteamLibraries). Windows
// servers get nothing added.
func (m *Manager) serverEnv() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{gameserver.LibraryPathVar + "=" + gameserver.LibraryPath(m.cfg.SteamRuntime(), os.Getenv(gameserver.LibraryPathVar))}
}

// findServerID reads the Steam server id from the start of the current
// server log, for a server found running when ESM starts. Every start
// begins a new log, and the server logs its id a few seconds after it
// starts, so the first id line is this run's; a later one is not taken.
func (m *Manager) findServerID(name string) string {
	f, err := m.st.Open(serverLog(name))
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	n, _ := f.ReadAt(buf, 0)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if v, ok := gameserver.ServerID(line); ok {
			return v
		}
	}
	return ""
}

// dropRunSecretsLocked removes the secrets file of a server that has
// stopped. It is best effort: a failure is logged, and the next start
// writes the file afresh anyway. While a start is under way nothing is
// removed: that start has already written the file for its own server.
// in.mu must be held.
func (m *Manager) dropRunSecretsLocked(in *rt) {
	if in.launching || in.deleted {
		return
	}
	if err := m.st.Remove(runSecretsFile(in.name)); err != nil {
		m.log.Warn("cannot remove the secrets file of a stopped server", "instance", in.name, "err", err)
	}
}

// adoptRun watches a server found running at ESM start. A live server
// that ESM started earlier counts as running at once (ready source
// "adopted"). Its ready signals are checked again only to show which one
// confirms; if none does, it keeps running with a note in its reason. It
// is never stopped for that.
func (m *Manager) adoptRun(in *rt, p supervisor.Proc) {
	if !m.track() {
		return
	}
	now := m.now()
	w := &watchState{
		server:   m.tailAt(serverLog(in.name)),
		console:  m.tailAt(consoleFile(in.name)),
		deadline: now.Add(m.cfg.ReadyTimeout.D()),
		adopted:  true,
	}
	id := m.findServerID(in.name)
	in.mu.Lock()
	w.bindIP, w.query = in.cfg.BindIP, in.cfg.Ports.Query
	w.unsafe = in.cfg.OutputHidden()
	in.gen++
	r := &run{gen: in.gen, autoRestart: true, done: make(chan struct{})}
	in.cur = r
	in.serverID = id
	in.status.Build = in.cfg.Build
	in.status.PID = p.PID
	in.status.StartedAt = p.StartedAt
	in.status.ReadyAt, in.status.ReadySource, in.status.ExitCode = now, ReadyAdopted, nil
	m.setLocked(in, Running, adoptedReason)
	in.mu.Unlock()
	r.finish(nil)
	go m.watch(in, r, w)
}

const adoptedReason = "adopted after ESM restart"

func (m *Manager) currentGen(in *rt) uint64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.gen
}

// watch follows one run until it ends: it publishes new log lines, detects
// ready, enforces the ready timeout and notices the process going away.
func (m *Manager) watch(in *rt, r *run, w *watchState) {
	defer m.wg.Done()
	t := time.NewTicker(m.opts.WatchInterval)
	defer t.Stop()
	ready := false
	for first := true; ; first = false {
		// The first check runs at once, so an adopted server that is
		// already ready shows as running without waiting a whole interval.
		if !first {
			select {
			case <-m.quit:
				r.finish(ErrClosed)
				return
			case <-t.C:
			}
		}
		if m.currentGen(in) != r.gen {
			return
		}
		m.followLogs(in, w)
		pr, err := m.sup.Status(in.name)
		if err != nil && !errors.Is(err, supervisor.ErrNotFound) {
			m.log.Warn("cannot read server status", "instance", in.name, "err", err)
			continue
		}
		if err != nil || !pr.Running {
			m.followLogs(in, w)
			m.exited(in, r, pr)
			return
		}
		if ready {
			continue
		}
		if src := m.readySource(in, pr, w); src != "" {
			if w.adopted {
				m.noteAdopted(in, r, "ready signal "+src+" confirmed")
				ready = true
			} else {
				ready = m.markReady(in, r, src)
			}
			continue
		}
		if m.now().After(w.deadline) {
			if w.adopted {
				// Never stop a server only because its ready signal could
				// not be seen again.
				m.noteAdopted(in, r, fmt.Sprintf("no ready signal seen within %s; left running", m.cfg.ReadyTimeout.D()))
				ready = true
				continue
			}
			m.notReady(in, r)
			return
		}
	}
}

// readySource checks the ready sources in order and returns the first one
// that confirms, or "": the server's status file (written by the process
// that is running now), the ready line in the server log, an answer on the
// query port.
func (m *Manager) readySource(in *rt, pr supervisor.Proc, w *watchState) string {
	if b, err := m.st.ReadFile(dataDir(in.name) + "/" + gameserver.StatusFileRel); err == nil {
		var s gameserver.Status
		if json.Unmarshal(b, &s) == nil && s.State == gameserver.StateReady &&
			(s.PID == 0 || pr.PID == 0 || s.PID == pr.PID) {
			return ReadyStatusFile
		}
	}
	if w.logReady {
		return ReadyLogLine
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.opts.ProbeTimeout)
	defer cancel()
	if gameserver.Probe(ctx, w.bindIP, w.query) == nil {
		return ReadyQueryPort
	}
	return ""
}

func (m *Manager) markReady(in *rt, r *run, src string) bool {
	in.mu.Lock()
	if in.gen != r.gen {
		in.mu.Unlock()
		return false
	}
	in.status.ReadyAt = m.now()
	in.status.ReadySource = src
	m.setLocked(in, Running, "ready ("+src+")")
	in.mu.Unlock()
	r.finish(nil)
	return true
}

// noteAdopted adds what the background ready check found to an adopted
// server's reason. The state stays running.
func (m *Manager) noteAdopted(in *rt, r *run, note string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.gen != r.gen || in.status.State != Running {
		return
	}
	in.status.Reason = adoptedReason + "; " + note
	m.emitLocked(in) // the state and its since time stay as they are
}

// notReady stops a server that did not become ready in time.
func (m *Manager) notReady(in *rt, r *run) {
	in.mu.Lock()
	if in.gen != r.gen {
		in.mu.Unlock()
		return
	}
	in.gen++
	gen := in.gen
	in.cur = nil
	in.mu.Unlock()
	limit := m.cfg.ReadyTimeout.D()
	m.log.Warn("server not ready in time; stopping it", "instance", in.name, "ready_timeout", limit)
	stopErr := m.sup.Stop(context.Background(), in.name, m.cfg.StopTimeout.D())
	reason := fmt.Sprintf("not ready within %s", limit)
	switch {
	case errors.Is(stopErr, supervisor.ErrStopTimeout):
		reason += "; " + m.stopTimeoutReason()
	case stopErr != nil:
		reason += "; stopping it failed: " + stopErr.Error()
	}
	in.mu.Lock()
	// Unless something else (a restart, a stop) took the instance over
	// while the server was being stopped.
	if in.gen == gen {
		if stopErr == nil {
			in.status.PID = 0
			in.serverID = ""
			m.dropRunSecretsLocked(in)
		}
		m.setLocked(in, Failed, reason)
	}
	in.mu.Unlock()
	r.finish(fault.New(fault.Failed, "not_ready", "instance %s was %s", in.name, reason))
}

// exited handles a server process that went away without a stop request.
func (m *Manager) exited(in *rt, r *run, pr supervisor.Proc) {
	now := m.now()
	in.mu.Lock()
	if in.gen != r.gen {
		in.mu.Unlock()
		return
	}
	in.gen++
	in.cur = nil
	in.status.PID = 0
	in.status.ExitCode = pr.ExitCode
	in.serverID = ""
	m.dropRunSecretsLocked(in)
	desc := "exited (exit code unknown)"
	if pr.ExitCode != nil {
		desc = "exited with code " + strconv.Itoa(*pr.ExitCode)
	}
	wasStarting := in.status.State == Starting
	var runErr error
	if wasStarting {
		runErr = fault.New(fault.Failed, "start_failed", "the server of instance %s %s before it was ready", in.name, desc)
	} else {
		runErr = fault.New(fault.Failed, "crashed", "the server of instance %s %s", in.name, desc)
	}
	b := m.cfg.Backoff
	switch {
	case wasStarting && !r.autoRestart:
		// An update or restore is checking this start; it decides.
		m.setLocked(in, Crashed, desc+" during start")
	default:
		in.crashes = append(RecentCrashes(in.crashes, now, b.Window.D()), now)
		n := len(in.crashes)
		in.status.Crashes = n
		switch {
		case GiveUp(in.crashes, b.MaxCrashes):
			in.status.GaveUp = true
			m.setLocked(in, Failed, fmt.Sprintf("%s; gave up after %d crashes within %s", desc, n, b.Window.D()))
		case in.cfg.RestartOnCrash:
			delay := BackoffDelay(b.Base.D(), b.Max.D(), n)
			in.status.NextRestart = now.Add(delay)
			gen := in.gen
			in.restartTimer = time.AfterFunc(delay, func() { m.restartAfterCrash(in, gen) })
			m.setLocked(in, Crashed, fmt.Sprintf("%s; restarting in %s", desc, delay))
		default:
			m.setLocked(in, Crashed, desc)
		}
	}
	in.mu.Unlock()
	m.log.Warn("server exited without a stop request", "instance", in.name, "detail", desc)
	r.finish(runErr)
}

func (m *Manager) restartAfterCrash(in *rt, gen uint64) {
	in.op.Lock()
	defer in.op.Unlock()
	in.mu.Lock()
	ok := in.gen == gen && in.status.State == Crashed && !in.deleted
	in.restartTimer = nil
	in.mu.Unlock()
	if !ok || m.isClosed() {
		return
	}
	if _, err := m.launch(context.Background(), in, "restarted after a crash", true); err != nil {
		in.mu.Lock()
		if in.gen == gen {
			m.setLocked(in, Failed, "restart after a crash failed: "+err.Error())
		}
		in.mu.Unlock()
	}
}

// waitRun waits until the run is ready or failed, or ctx ends.
func waitRun(ctx context.Context, r *run) error {
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clearCrashesLocked forgets the crash history: a manual start, restart or
// update starts counting afresh. in.mu must be held.
func (in *rt) clearCrashesLocked() {
	in.crashes = nil
	in.status.Crashes = 0
	in.status.GaveUp = false
	in.status.NextRestart = time.Time{}
	if in.restartTimer != nil {
		in.restartTimer.Stop()
		in.restartTimer = nil
	}
}

// Start starts a stopped instance. With wait it returns once the server is
// ready (or failed); otherwise once it is launched. The first start after
// an automatic update that switched the build of the stopped instance
// always waits: it checks the new build and rolls it back when it does not
// get ready (see startChecked).
func (m *Manager) Start(ctx context.Context, name string, wait bool) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	in.op.Lock()
	in.mu.Lock()
	if in.status.State.active() {
		st := in.status.State
		in.mu.Unlock()
		in.op.Unlock()
		return m.mustStatus(name), fault.New(fault.Conflict, "already_running", "instance %s is %s", name, st)
	}
	in.gen++ // cancels a pending restart after a crash
	in.clearCrashesLocked()
	u, check := m.unverifiedLocked(in)
	in.mu.Unlock()
	if check {
		defer in.op.Unlock()
		return m.startChecked(ctx, in, u)
	}
	r, err := m.launch(ctx, in, "started", true)
	in.op.Unlock()
	if err != nil {
		return m.mustStatus(name), err
	}
	if wait {
		if err := waitRun(ctx, r); err != nil {
			return m.mustStatus(name), err
		}
	}
	return m.mustStatus(name), nil
}

func (m *Manager) mustStatus(name string) Status {
	st, _ := m.Status(name)
	return st
}

// Stop stops an instance: one polite stop request (Linux: one SIGINT),
// then a wait of up to stop_timeout. A server still running then is left
// running, the instance is failed with a reason that says so, and the
// error has the code stop_timeout. A later stop waits again without a
// second signal.
func (m *Manager) Stop(ctx context.Context, name string) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	in.op.Lock()
	defer in.op.Unlock()
	err = m.stopLocked(context.WithoutCancel(ctx), in, "stopped")
	return m.mustStatus(name), err
}

// stopLocked stops the server of in, if any, and leaves the instance
// stopped. in.op must be held.
func (m *Manager) stopLocked(ctx context.Context, in *rt, reason string) error {
	in.mu.Lock()
	in.gen++
	r := in.cur
	in.cur = nil
	if in.restartTimer != nil {
		in.restartTimer.Stop()
		in.restartTimer = nil
	}
	in.status.NextRestart = time.Time{}
	prev := in.status.State
	in.mu.Unlock()
	if r != nil {
		r.finish(fault.New(fault.Conflict, "stopped", "instance %s was stopped before it was ready", in.name))
	}
	pr, err := m.sup.Status(in.name)
	alive := err == nil && pr.Running
	if alive {
		in.mu.Lock()
		m.setLocked(in, Stopping, reason)
		in.mu.Unlock()
		if err := m.sup.Stop(ctx, in.name, m.cfg.StopTimeout.D()); err != nil {
			if errors.Is(err, supervisor.ErrStopTimeout) {
				why := m.stopTimeoutReason()
				in.mu.Lock()
				m.setLocked(in, Failed, why)
				in.mu.Unlock()
				return fault.New(fault.Failed, "stop_timeout", "instance %s: %s", in.name, why)
			}
			in.mu.Lock()
			m.setLocked(in, Failed, "stop failed: "+err.Error())
			in.mu.Unlock()
			return fault.Wrap(fault.Failed, "stop_failed", err, "cannot stop instance %s", in.name)
		}
	}
	in.mu.Lock()
	in.status.PID = 0
	in.serverID = ""
	// No server of this instance runs now: its secrets file goes.
	m.dropRunSecretsLocked(in)
	if alive || prev != Stopped {
		m.setLocked(in, Stopped, reason)
	}
	in.mu.Unlock()
	return nil
}

// stopTimeoutReason says what happened when a server did not exit in time
// after its stop request.
func (m *Manager) stopTimeoutReason() string {
	return fmt.Sprintf("stop timed out: the server did not exit within %s after its stop signal; it was left running "+
		"and gets no second signal (stop again to wait again)", m.cfg.StopTimeout.D())
}

// Restart stops and starts an instance. why is recorded as the reason. A
// start that checks an automatically updated build works as in Start.
func (m *Manager) Restart(ctx context.Context, name, why string, wait bool) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	if why == "" {
		why = "restarted"
	}
	in.op.Lock()
	if err := m.stopLocked(context.WithoutCancel(ctx), in, "stopped for a restart"); err != nil {
		in.op.Unlock()
		return m.mustStatus(name), err
	}
	in.mu.Lock()
	in.clearCrashesLocked()
	u, check := m.unverifiedLocked(in)
	in.mu.Unlock()
	if check {
		defer in.op.Unlock()
		return m.startChecked(ctx, in, u)
	}
	r, err := m.launch(ctx, in, why, true)
	in.op.Unlock()
	if err != nil {
		return m.mustStatus(name), err
	}
	if wait {
		if err := waitRun(ctx, r); err != nil {
			return m.mustStatus(name), err
		}
	}
	return m.mustStatus(name), nil
}

// followLogs publishes new lines of the server log and console output.
// The ready line is looked for in the server log lines as the server wrote
// them, before anything is masked or cut.
//
// The lines of a server that got its passwords on the command line, and
// all lines while the instance is set that way, are published restricted:
// the read role never gets them.
func (m *Manager) followLogs(in *rt, w *watchState) {
	in.mu.Lock()
	filter, hidden := in.filter, w.unsafe || in.cfg.OutputHidden()
	in.mu.Unlock()
	publish := func(source, line string) {
		ll := LogLine{Source: source, Line: showOutput(filter, line)}
		if hidden {
			m.bus.PublishRestricted(events.TypeLog, in.name, ll, nil)
			return
		}
		m.bus.Publish(events.TypeLog, in.name, ll)
	}
	for _, line := range m.follow(&w.server) {
		if !w.logReady && gameserver.IsReadyLine(line) {
			w.logReady = true
		}
		if id, ok := gameserver.ServerID(line); ok {
			// The first id line of a run is the server's; a start clears it.
			in.mu.Lock()
			if in.serverID == "" {
				in.serverID = id
			}
			in.mu.Unlock()
		}
		publish(SourceServer, line)
	}
	for _, line := range m.follow(&w.console) {
		publish(SourceConsole, line)
	}
}
