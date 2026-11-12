// Package supervisor starts, stops and watches game server processes.
//
// Two implementations exist:
//
//   - process (Linux and Windows): ESM starts the server as a detached child
//     and records it in <root>/instances/<name>/run/pid.json. Before ESM
//     signals or kills any pid it checks that the pid still has the recorded
//     start marker and executable; if not, the pid is not ours and is never
//     touched. Resource limits are not enforced in this mode.
//   - systemd (Linux): each server is a transient user unit esm-<name>
//     started with systemd-run --user, which enforces memory and CPU limits.
//
// New picks one according to the configured kind ("auto" prefers systemd
// when `systemctl --user` works).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// Kinds.
const (
	KindAuto    = "auto"
	KindProcess = "process"
	KindSystemd = "systemd"
)

// Errors.
var (
	ErrNotFound        = errors.New("supervisor: no process known for this instance")
	ErrAlreadyRunning  = errors.New("supervisor: instance is already running")
	ErrNotOurs         = errors.New("supervisor: the recorded process is not the one ESM started; not touching it")
	ErrUnknownInstance = errors.New("supervisor: no such instance in the ESM root")
	ErrBadName         = errors.New("supervisor: invalid instance name")
	ErrClosed          = errors.New("supervisor: closed")
	ErrStillRunning    = errors.New("supervisor: process did not exit after being killed")
)

// Limits are resource limits for one server. Zero means no limit.
type Limits struct {
	MemoryMaxBytes  int64 `json:"memory_max_bytes,omitempty"`
	CPUQuotaPercent int   `json:"cpu_quota_percent,omitempty"`
}

// Spec describes a server process to start.
type Spec struct {
	Name       string   // instance name, ^[a-z][a-z0-9-]{0,31}$
	Executable string   // absolute path
	Args       []string // arguments after the executable
	Dir        string   // absolute working directory
	Env        []string // KEY=VALUE; nil means inherit ESM's environment (process mode)
	OutputFile string   // absolute path inside the ESM root; stdout and stderr are appended
	Limits     Limits
	// StopFile is written to ask for a polite stop where signals are not
	// available (Windows). Absolute path inside the ESM root; optional.
	StopFile string
}

// Proc is what the supervisor knows about one server process.
type Proc struct {
	Name      string    `json:"name"`
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	ExitCode  *int      `json:"exit_code,omitempty"` // nil when unknown
	ExitedAt  time.Time `json:"exited_at,omitzero"`
}

// Supervisor runs game server processes.
type Supervisor interface {
	// Start launches the server and returns once it is running.
	Start(ctx context.Context, spec Spec) error
	// Stop asks politely, waits up to timeout, then kills. Stopping
	// something that is not running is not an error.
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Status reports the process of an instance; ErrNotFound when the
	// supervisor knows nothing about it.
	Status(name string) (Proc, error)
	// Adopt finds servers this ESM root started earlier that are still
	// alive, and watches them again. It returns them.
	Adopt() ([]Proc, error)
	// Kind is "process" or "systemd".
	Kind() string
	// LimitsEnforced reports whether Spec.Limits are applied.
	LimitsEnforced() bool
	// Close stops watching. Servers keep running.
	Close() error
}

// Options configure New.
type Options struct {
	// Kind is auto, process or systemd (empty means auto).
	Kind string
	// StopTimeout is the systemd TimeoutStopSec for new units (default 30s).
	StopTimeout time.Duration
	// PollInterval is how often adopted processes are checked (default 250ms).
	PollInterval time.Duration
	// Logger receives diagnostics (default: discard).
	Logger *slog.Logger
}

func (o *Options) fill() {
	if o.Kind == "" {
		o.Kind = KindAuto
	}
	if o.StopTimeout <= 0 {
		o.StopTimeout = 30 * time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 250 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
}

// New returns the supervisor for opts.Kind. "auto" means systemd when the
// OS is Linux and `systemctl --user show-environment` succeeds, otherwise
// process.
func New(st *store.Store, opts Options) (Supervisor, error) {
	opts.fill()
	newProcess := func() (Supervisor, error) {
		p, err := NewProcess(st, opts)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	switch opts.Kind {
	case KindAuto:
		if SystemdAvailable() {
			return NewSystemd(st, opts)
		}
		return newProcess()
	case KindProcess:
		return newProcess()
	case KindSystemd:
		if runtime.GOOS != "linux" {
			return nil, errors.New("supervisor: systemd is only available on Linux")
		}
		if !SystemdAvailable() {
			return nil, errors.New("supervisor: systemd was requested but `systemctl --user` does not work for this user")
		}
		return NewSystemd(st, opts)
	}
	return nil, fmt.Errorf("supervisor: unknown kind %q (want auto, process or systemd)", opts.Kind)
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidName reports whether name is a valid instance name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

func instanceDir(name string) string { return "instances/" + name }

func runDir(name string) string { return instanceDir(name) + "/run" }

func pidFile(name string) string { return runDir(name) + "/pid.json" }

// known reports whether the instance folder exists in the store.
func known(st *store.Store, name string) bool {
	fi, err := st.Stat(instanceDir(name))
	return err == nil && fi.IsDir()
}

func checkName(st *store.Store, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if !known(st, name) {
		return fmt.Errorf("%w: %q", ErrUnknownInstance, name)
	}
	return nil
}

func checkSpec(st *store.Store, s Spec) error {
	if err := checkName(st, s.Name); err != nil {
		return err
	}
	if !filepath.IsAbs(s.Executable) {
		return fmt.Errorf("supervisor: executable must be an absolute path, got %q", s.Executable)
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("supervisor: working directory must be an absolute path, got %q", s.Dir)
	}
	if _, err := st.Rel(s.OutputFile); err != nil {
		return fmt.Errorf("supervisor: output file must be inside the ESM root: %w", err)
	}
	if s.StopFile != "" {
		if _, err := st.Rel(s.StopFile); err != nil {
			return fmt.Errorf("supervisor: stop file must be inside the ESM root: %w", err)
		}
	}
	if s.Limits.MemoryMaxBytes < 0 || s.Limits.CPUQuotaPercent < 0 {
		return errors.New("supervisor: limits must not be negative")
	}
	return nil
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
