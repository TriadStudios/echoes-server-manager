package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

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
}

// launch starts the server of in with its current settings and begins
// watching it. in.op must be held.
func (m *Manager) launch(ctx context.Context, in *rt, why string, autoRestart bool) (*run, error) {
	if m.isClosed() {
		return nil, ErrClosed
	}
	in.mu.Lock()
	cfg := in.cfg.clone()
	in.mu.Unlock()
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
	if err := checkPortsBindable(cfg); err != nil {
		return nil, err
	}
	sec, err := m.readSecrets(cfg.Name)
	if err != nil {
		return nil, err
	}
	if err := m.st.MkdirAll(dataDir(cfg.Name), 0o700); err != nil {
		return nil, err
	}
	dataAbs, err := m.st.Abs(dataDir(cfg.Name))
	if err != nil {
		return nil, err
	}
	dirAbs, _ := m.st.Abs(instDir(cfg.Name))
	outAbs, _ := m.st.Abs(consoleFile(cfg.Name))
	memMax, _ := ParseMemory(cfg.Limits.MemoryMax)
	name := cfg.DisplayName
	if name == "" {
		name = cfg.Name
	}
	argv, masked := gameserver.Launch{
		Map: cfg.Map, GamePort: cfg.Ports.Game, QueryPort: cfg.Ports.Query, BindIP: cfg.BindIP,
		MaxPlayers: cfg.MaxPlayers, ServerName: name, WorldName: cfg.World, Rules: cfg.Rules,
		DataDir: dataAbs, ServerPassword: sec.ServerPassword, AdminPassword: sec.AdminPassword,
		ExtraArgs: cfg.ExtraArgs,
	}.Args()
	// A status file from an earlier run must not count as ready.
	if err := m.st.Remove(dataDir(cfg.Name) + "/" + gameserver.StatusFileRel); err != nil {
		return nil, err
	}
	w := &watchState{
		server:  tail{rel: serverLog(cfg.Name), off: m.fileSize(serverLog(cfg.Name))},
		console: tail{rel: consoleFile(cfg.Name), off: m.fileSize(consoleFile(cfg.Name))},
		bindIP:  cfg.BindIP,
		query:   cfg.Ports.Query,
	}
	spec := supervisor.Spec{
		Name: cfg.Name, Executable: exe, Args: argv, Dir: dirAbs, OutputFile: outAbs,
		Limits:   supervisor.Limits{MemoryMaxBytes: memMax, CPUQuotaPercent: cfg.Limits.CPUQuotaPercent},
		StopFile: gameserver.StopRequestPath(dataAbs),
	}
	m.log.Info("starting server", "instance", cfg.Name, "build", cfg.Build, "why", why, "argv", masked)
	if err := m.sup.Start(context.WithoutCancel(ctx), spec); err != nil {
		if errors.Is(err, supervisor.ErrAlreadyRunning) {
			return nil, fault.New(fault.Conflict, "already_running", "a server of instance %s is already running", cfg.Name)
		}
		return nil, fault.Wrap(fault.Failed, "start_failed", err, "cannot start instance %s", cfg.Name)
	}
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

// adoptRun watches a server found running at ESM start as a new run that
// must show it is ready again.
func (m *Manager) adoptRun(in *rt, p supervisor.Proc) {
	if !m.track() {
		return
	}
	w := &watchState{
		server:   tail{rel: serverLog(in.name), off: m.fileSize(serverLog(in.name))},
		console:  tail{rel: consoleFile(in.name), off: m.fileSize(consoleFile(in.name))},
		deadline: m.now().Add(m.cfg.ReadyTimeout.D()),
	}
	in.mu.Lock()
	w.bindIP, w.query = in.cfg.BindIP, in.cfg.Ports.Query
	in.gen++
	r := &run{gen: in.gen, autoRestart: true, done: make(chan struct{})}
	in.cur = r
	in.status.Build = in.cfg.Build
	in.status.PID = p.PID
	in.status.StartedAt = p.StartedAt
	in.status.ReadyAt, in.status.ReadySource, in.status.ExitCode = time.Time{}, "", nil
	m.setLocked(in, Starting, "adopted after ESM restart")
	in.mu.Unlock()
	go m.watch(in, r, w)
}

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
	for {
		select {
		case <-m.quit:
			r.finish(ErrClosed)
			return
		case <-t.C:
		}
		if m.currentGen(in) != r.gen {
			return
		}
		m.followLogs(in.name, w)
		pr, err := m.sup.Status(in.name)
		if err != nil && !errors.Is(err, supervisor.ErrNotFound) {
			m.log.Warn("cannot read server status", "instance", in.name, "err", err)
			continue
		}
		if err != nil || !pr.Running {
			m.followLogs(in.name, w)
			m.exited(in, r, pr)
			return
		}
		if ready {
			continue
		}
		if src := m.readySource(in, pr, w); src != "" {
			ready = m.markReady(in, r, src)
			continue
		}
		if m.now().After(w.deadline) {
			m.notReady(in, r)
			return
		}
	}
}

// readySource checks the ready sources in order and returns the first one
// that confirms, or "".
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

// notReady stops a server that did not become ready in time.
func (m *Manager) notReady(in *rt, r *run) {
	in.mu.Lock()
	if in.gen != r.gen {
		in.mu.Unlock()
		return
	}
	in.gen++
	in.cur = nil
	in.mu.Unlock()
	limit := m.cfg.ReadyTimeout.D()
	m.log.Warn("server not ready in time; stopping it", "instance", in.name, "ready_timeout", limit)
	stopErr := m.sup.Stop(context.Background(), in.name, m.cfg.StopTimeout.D())
	reason := fmt.Sprintf("not ready within %s", limit)
	if stopErr != nil {
		reason += "; stopping it failed: " + stopErr.Error()
	}
	in.mu.Lock()
	in.status.PID = 0
	m.setLocked(in, Failed, reason)
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
// ready (or failed); otherwise once it is launched.
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
	in.mu.Unlock()
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

// Stop stops an instance: politely, then by force after stop_timeout.
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
			in.mu.Lock()
			m.setLocked(in, Failed, "stop failed: "+err.Error())
			in.mu.Unlock()
			return fault.Wrap(fault.Failed, "stop_failed", err, "cannot stop instance %s", in.name)
		}
	}
	in.mu.Lock()
	in.status.PID = 0
	if alive || prev != Stopped {
		m.setLocked(in, Stopped, reason)
	}
	in.mu.Unlock()
	return nil
}

// Restart stops and starts an instance. why is recorded as the reason.
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
	in.mu.Unlock()
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
func (m *Manager) followLogs(name string, w *watchState) {
	for _, line := range m.follow(&w.server) {
		if !w.logReady && gameserver.ReadyLine.MatchString(line) {
			w.logReady = true
		}
		m.bus.Publish(events.TypeLog, name, LogLine{Source: "server", Line: line})
	}
	for _, line := range m.follow(&w.console) {
		m.bus.Publish(events.TypeLog, name, LogLine{Source: "console", Line: line})
	}
}
