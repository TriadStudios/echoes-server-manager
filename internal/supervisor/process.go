package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// pidRecord is the content of instances/<name>/run/pid.json.
type pidRecord struct {
	PID         int       `json:"pid"`
	StartMarker string    `json:"start_marker"`
	Executable  string    `json:"executable"`
	StopFile    string    `json:"stop_file,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	// StopSentAt is when this process was sent its one stop signal. Set,
	// no further signal is ever sent to it.
	StopSentAt time.Time `json:"stop_sent_at,omitzero"`
}

// procState is the result of checking a recorded pid.
type procState int

const (
	procGone    procState = iota // no process with that pid, or it has exited
	procOurs                     // alive, same start marker and executable
	procForeign                  // alive, but a different process now owns the pid
)

type entry struct {
	name     string
	rec      pidRecord
	proc     *os.Process
	child    bool          // started by this supervisor (a goroutine Waits)
	done     chan struct{} // closed once the process has exited
	exitCode *int
	exitedAt time.Time
}

func (e *entry) exited() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Process supervises servers as detached child processes.
type Process struct {
	st   *store.Store
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
	quit    chan struct{}
}

// NewProcess returns the process supervisor.
func NewProcess(st *store.Store, opts Options) (*Process, error) {
	opts.fill()
	if !processSupported {
		return nil, fmt.Errorf("supervisor: process mode is not supported on %s", runtime.GOOS)
	}
	return &Process{st: st, opts: opts, log: opts.Logger, entries: make(map[string]*entry), quit: make(chan struct{})}, nil
}

// Kind returns "process".
func (p *Process) Kind() string { return KindProcess }

// LimitsEnforced is false: process mode cannot apply memory or CPU limits.
func (p *Process) LimitsEnforced() bool { return false }

// Close stops watching adopted processes and stops touching pid files.
// Running servers keep running.
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.quit)
	}
	return nil
}

// Start launches spec as a detached process.
func (p *Process) Start(ctx context.Context, spec Spec) error {
	if err := checkSpec(p.st, spec); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock := p.st.Lock("supervisor:" + spec.Name)
	defer unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	if e, ok := p.entries[spec.Name]; ok && !e.exited() {
		p.mu.Unlock()
		return ErrAlreadyRunning
	}
	p.mu.Unlock()
	// A pid file left by another supervisor instance that still owns a
	// live process: refuse rather than start a second copy.
	var old pidRecord
	if err := p.st.ReadJSON(pidFile(spec.Name), &old); err == nil {
		if checkProc(old) == procOurs {
			return ErrAlreadyRunning
		}
	}

	exe, err := filepath.EvalSymlinks(spec.Executable)
	if err != nil {
		return fmt.Errorf("supervisor: executable: %w", err)
	}
	if spec.StopFile != "" {
		// A stop request left over from a killed run would stop the new
		// server at once.
		rel, _ := p.st.Rel(spec.StopFile)
		if err := p.st.Remove(rel); err != nil {
			return fmt.Errorf("supervisor: remove old stop request: %w", err)
		}
	}
	outRel, _ := p.st.Rel(spec.OutputFile)
	out, err := p.st.OpenAppend(outRel, 0o600)
	if err != nil {
		return fmt.Errorf("supervisor: open output: %w", err)
	}
	var cmd *exec.Cmd
	// A binary still open for writing somewhere (a build being installed,
	// or a descriptor a child forked in that moment holds until it runs
	// its own program) cannot be run yet on Linux; that passes quickly.
	for deadline := time.Now().Add(busyWait); ; {
		cmd = exec.Command(exe, spec.Args...)
		cmd.Dir = spec.Dir
		cmd.Env = mergeEnv(os.Environ(), spec.Env)
		cmd.Stdout = out
		cmd.Stderr = out
		cmd.SysProcAttr = detachedAttr()
		err = cmd.Start()
		if err == nil || !textBusy(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	out.Close()
	if err != nil {
		// The error names the executable, not the arguments.
		return startError(spec, err)
	}
	pid := cmd.Process.Pid
	// While we have not waited for it the pid cannot be reused, so the
	// marker read now belongs to our child.
	marker, err := startMarker(pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("supervisor: read start marker of pid %d: %w", pid, err)
	}
	rec := pidRecord{PID: pid, StartMarker: marker, Executable: exe, StopFile: spec.StopFile, StartedAt: time.Now().UTC()}
	if err := p.st.WriteJSON(pidFile(spec.Name), rec, 0o600); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("supervisor: write pid file: %w", err)
	}
	e := &entry{name: spec.Name, rec: rec, proc: cmd.Process, child: true, done: make(chan struct{})}
	p.mu.Lock()
	p.entries[spec.Name] = e
	p.mu.Unlock()
	p.log.Info("server started", "instance", spec.Name, "pid", pid, "mode", KindProcess)
	go p.waitChild(spec.Name, e, cmd)
	return nil
}

// busyWait is how long a start retries a binary that is busy (see
// textBusy).
const busyWait = 3 * time.Second

// mergeEnv returns base with the KEY=VALUE pairs of set added, each
// replacing what base had for its key.
func mergeEnv(base, set []string) []string {
	if len(set) == 0 {
		return nil // inherit
	}
	keys := map[string]bool{}
	for _, kv := range set {
		k, _, _ := strings.Cut(kv, "=")
		keys[envKey(k)] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !keys[envKey(k)] {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// envKey is a variable name as the OS compares it.
func envKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

func (p *Process) waitChild(name string, e *entry, cmd *exec.Cmd) {
	err := cmd.Wait()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else if err != nil {
		p.log.Warn("wait failed", "instance", name, "err", err)
	}
	p.markExited(name, e, &code)
}

func (p *Process) markExited(name string, e *entry, code *int) {
	p.mu.Lock()
	if e.exited() {
		p.mu.Unlock()
		return
	}
	e.exitCode = code
	e.exitedAt = time.Now().UTC()
	close(e.done)
	closed := p.closed
	p.mu.Unlock()
	if code != nil {
		p.log.Info("server exited", "instance", name, "pid", e.rec.PID, "exit_code", *code)
	} else {
		p.log.Info("server exited", "instance", name, "pid", e.rec.PID)
	}
	if !closed {
		p.removePidFileIf(name, e.rec.PID)
	}
}

// removePidFileIf removes the pid file only if it still names pid.
func (p *Process) removePidFileIf(name string, pid int) {
	unlock := p.st.Lock("pidfile:" + name)
	defer unlock()
	var rec pidRecord
	if err := p.st.ReadJSON(pidFile(name), &rec); err == nil && rec.PID == pid {
		p.st.Remove(pidFile(name))
	}
}

// watchAdopted polls an adopted process until it is gone.
func (p *Process) watchAdopted(name string, e *entry) {
	t := time.NewTicker(p.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-e.done:
			return
		case <-t.C:
			if checkProc(e.rec) != procOurs {
				p.markExited(name, e, nil)
				return
			}
		}
	}
}

func (p *Process) procOf(name string, e *entry) Proc {
	pr := Proc{Name: name, PID: e.rec.PID, StartedAt: e.rec.StartedAt}
	if e.exited() {
		pr.ExitCode = e.exitCode
		pr.ExitedAt = e.exitedAt
		return pr
	}
	pr.Running = true
	return pr
}

// Status reports the process of an instance.
func (p *Process) Status(name string) (Proc, error) {
	if !ValidName(name) {
		return Proc{Name: name}, ErrBadName
	}
	p.mu.Lock()
	e, ok := p.entries[name]
	if ok {
		pr := p.procOf(name, e)
		p.mu.Unlock()
		return pr, nil
	}
	p.mu.Unlock()
	var rec pidRecord
	if err := p.st.ReadJSON(pidFile(name), &rec); err != nil {
		if isNotExist(err) {
			return Proc{Name: name}, ErrNotFound
		}
		return Proc{Name: name}, err
	}
	if checkProc(rec) == procOurs {
		return Proc{Name: name, Running: true, PID: rec.PID, StartedAt: rec.StartedAt}, nil
	}
	return Proc{Name: name, PID: rec.PID, StartedAt: rec.StartedAt}, nil
}

// Adopt watches again every live process recorded under the ESM root and
// removes pid files whose process is gone or no longer ours.
func (p *Process) Adopt() ([]Proc, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	p.mu.Unlock()
	dirs, err := p.st.ReadDir("instances")
	if isNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, d := range dirs {
		name := d.Name()
		if !d.IsDir() || !ValidName(name) {
			continue
		}
		p.mu.Lock()
		if e, ok := p.entries[name]; ok && !e.exited() {
			out = append(out, p.procOf(name, e))
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()
		var rec pidRecord
		if err := p.st.ReadJSON(pidFile(name), &rec); err != nil {
			if !isNotExist(err) {
				p.log.Warn("unreadable pid file", "instance", name, "err", err)
			}
			continue
		}
		e, state := p.adoptOne(name, rec)
		switch state {
		case procOurs:
			p.mu.Lock()
			p.entries[name] = e
			out = append(out, p.procOf(name, e))
			p.mu.Unlock()
			p.log.Info("adopted running server", "instance", name, "pid", rec.PID)
			go p.watchAdopted(name, e)
		case procForeign:
			p.log.Warn("pid file names a process that is not ours; leaving it alone", "instance", name, "pid", rec.PID)
			p.removePidFileIf(name, rec.PID)
		default:
			p.removePidFileIf(name, rec.PID)
		}
	}
	return out, nil
}

// adoptOne opens a handle on rec's pid and then confirms the handle is ours.
// Opening first means the checked process is the one the handle refers to.
func (p *Process) adoptOne(name string, rec pidRecord) (*entry, procState) {
	if rec.PID <= 0 {
		return nil, procGone
	}
	proc, err := os.FindProcess(rec.PID)
	if err != nil {
		return nil, procGone
	}
	state := checkProc(rec)
	if state != procOurs {
		proc.Release()
		return nil, state
	}
	return &entry{name: name, rec: rec, proc: proc, done: make(chan struct{})}, procOurs
}

// Stop asks the process to stop and waits up to timeout (or until ctx
// ends).
//
// Where signals exist (Linux) that is exactly one SIGINT. A process that
// was sent it already (by an earlier stop that ran out of time, even one
// of an earlier ESM) is not sent another: the stop only waits again. A
// process that is still running when the wait ends is left alone, and
// Stop returns ErrStopTimeout: a second signal or a kill could end the
// server before its world is saved.
//
// On Windows (development only) the stop request file is written, and a
// process that is still running after timeout is killed.
func (p *Process) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if !ValidName(name) {
		return ErrBadName
	}
	p.mu.Lock()
	e, ok := p.entries[name]
	p.mu.Unlock()
	if !ok || e.exited() {
		// Not watched by us: only a pid file can say more. Never adopt
		// silently here; check and refuse anything that is not ours.
		var rec pidRecord
		if err := p.st.ReadJSON(pidFile(name), &rec); err != nil {
			if isNotExist(err) {
				return nil
			}
			return err
		}
		if ok && e.rec.PID == rec.PID {
			return nil // the exited entry's own record
		}
		var state procState
		e, state = p.adoptOne(name, rec)
		switch state {
		case procGone:
			return nil
		case procForeign:
			return fmt.Errorf("%w (instance %s, pid %d)", ErrNotOurs, name, rec.PID)
		}
		p.mu.Lock()
		p.entries[name] = e
		p.mu.Unlock()
		go p.watchAdopted(name, e)
	}
	return p.stopEntry(ctx, name, e, timeout)
}

func (p *Process) stopEntry(ctx context.Context, name string, e *entry, timeout time.Duration) error {
	switch checkProc(e.rec) {
	case procGone:
		p.waitExit(e, 2*time.Second)
		return nil
	case procForeign:
		return fmt.Errorf("%w (instance %s, pid %d)", ErrNotOurs, name, e.rec.PID)
	}
	if signalsAvailable {
		return p.interruptOnce(ctx, name, e, timeout)
	}
	if err := p.polite(name, e); err != nil {
		p.log.Warn("polite stop failed; killing", "instance", name, "err", err)
	} else {
		t := time.NewTimer(timeout)
		select {
		case <-e.done:
			t.Stop()
			return nil
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	if e.exited() {
		return nil
	}
	// Re-check right before killing.
	switch checkProc(e.rec) {
	case procGone:
		p.waitExit(e, 2*time.Second)
		return nil
	case procForeign:
		return fmt.Errorf("%w (instance %s, pid %d)", ErrNotOurs, name, e.rec.PID)
	}
	p.log.Warn("server did not stop in time; killing it", "instance", name, "pid", e.rec.PID)
	if err := e.proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("supervisor: kill %s (pid %d): %w", name, e.rec.PID, err)
	}
	if !p.waitExit(e, 10*time.Second) {
		return fmt.Errorf("%w (instance %s, pid %d)", ErrStillRunning, name, e.rec.PID)
	}
	return nil
}

// interruptOnce sends e its one SIGINT, unless it was sent already, and
// waits up to timeout for it to exit. It never sends a second signal and
// never kills.
func (p *Process) interruptOnce(ctx context.Context, name string, e *entry, timeout time.Duration) error {
	gone, err := p.signalOnce(name, e)
	if err != nil || gone {
		return err
	}
	if p.waitExitUntil(ctx, e, timeout) {
		return nil
	}
	p.log.Warn("server did not exit in time after its stop signal; leaving it running", "instance", name, "pid", e.rec.PID,
		"timeout", timeout)
	return fmt.Errorf("%w (instance %s, pid %d, waited %s)", ErrStopTimeout, name, e.rec.PID, timeout)
}

// signalOnce sends e its one SIGINT unless it was sent already. The check,
// the record and the signal happen under one lock per instance, shared by
// every supervisor on this root, so stops that run at the same time send
// one signal between them. gone reports a process that is no longer there.
func (p *Process) signalOnce(name string, e *entry) (gone bool, err error) {
	unlock := p.st.Lock(stopLockKey(name))
	defer unlock()
	p.mu.Lock()
	sent := e.rec.StopSentAt
	p.mu.Unlock()
	// Another supervisor on this root may have sent it: its record is in
	// the pid file.
	var onDisk pidRecord
	if sent.IsZero() && p.st.ReadJSON(pidFile(name), &onDisk) == nil && onDisk.PID == e.rec.PID &&
		onDisk.StartMarker == e.rec.StartMarker {
		sent = onDisk.StopSentAt
	}
	if !sent.IsZero() {
		p.mu.Lock()
		e.rec.StopSentAt = sent
		p.mu.Unlock()
		p.log.Info("the server has had its stop signal already; waiting again, sending none", "instance", name,
			"pid", e.rec.PID, "sent", sent)
		return false, nil
	}
	if testHookBeforeStopSignal != nil {
		testHookBeforeStopSignal(name)
	}
	// Recorded before it is sent: whatever happens next, this process
	// never gets a second one from ESM.
	p.mu.Lock()
	e.rec.StopSentAt = time.Now().UTC()
	rec := e.rec
	p.mu.Unlock()
	if err := p.st.WriteJSON(pidFile(name), rec, 0o600); err != nil {
		return false, fmt.Errorf("supervisor: stop %s: record the stop signal: %w", name, err)
	}
	// Re-check right before signalling.
	switch checkProc(e.rec) {
	case procGone:
		p.waitExit(e, 2*time.Second)
		return true, nil
	case procForeign:
		return false, fmt.Errorf("%w (instance %s, pid %d)", ErrNotOurs, name, e.rec.PID)
	}
	if err := interrupt(e.proc); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return false, fmt.Errorf("supervisor: stop %s (pid %d): %w", name, e.rec.PID, err)
	}
	p.log.Info("stop signal sent", "instance", name, "pid", e.rec.PID, "signal", "SIGINT")
	return false, nil
}

// polite sends the polite stop request where signals do not exist: the
// stop file.
func (p *Process) polite(name string, e *entry) error {
	if e.rec.StopFile == "" {
		return errors.New("no stop file configured")
	}
	rel, err := p.st.Rel(e.rec.StopFile)
	if err != nil {
		return err
	}
	return p.st.WriteFile(rel, []byte("stop\n"), 0o600)
}

// waitExit waits until e is known to have exited; for adopted processes it
// polls directly as well.
func (p *Process) waitExit(e *entry, d time.Duration) bool {
	return p.waitExitUntil(context.Background(), e, d)
}

// waitExitUntil is waitExit that gives up when ctx ends.
func (p *Process) waitExitUntil(ctx context.Context, e *entry, d time.Duration) bool {
	deadline := time.After(d)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-e.done:
			return true
		case <-deadline:
			return e.exited()
		case <-ctx.Done():
			return e.exited()
		case <-t.C:
			if !e.child && checkProc(e.rec) != procOurs {
				p.markExited(e.name, e, nil)
				return true
			}
		}
	}
}

// checkProc decides whether rec's pid is still the process ESM started.
func checkProc(rec pidRecord) procState {
	if rec.PID <= 0 {
		return procGone
	}
	alive, marker, err := inspect(rec.PID)
	if err != nil || !alive {
		return procGone
	}
	if marker != rec.StartMarker {
		return procForeign
	}
	if !sameExecutable(rec.PID, rec.Executable) {
		return procForeign
	}
	return procOurs
}
