package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// SystemdAvailable reports whether `systemctl --user show-environment`
// works and systemd-run is installed.
func SystemdAvailable() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Run() == nil
}

// Systemd supervises servers as transient systemd user units named
// esm-<name>.service. It only ever touches units whose instance exists in
// the ESM root and whose Description is the one this root gave them.
type Systemd struct {
	st   *store.Store
	opts Options
	log  *slog.Logger

	expandFlag bool // systemd-run understands --expand-environment=no

	mu   sync.Mutex
	seen map[string]time.Time // instances started or adopted, for Status
}

// NewSystemd returns the systemd supervisor.
func NewSystemd(st *store.Store, opts Options) (Supervisor, error) {
	opts.fill()
	s := &Systemd{st: st, opts: opts, log: opts.Logger, seen: make(map[string]time.Time)}
	s.expandFlag = systemdVersion() >= 254
	return s, nil
}

var versionRe = regexp.MustCompile(`^systemd (\d+)`)

func systemdVersion() int {
	out, err := exec.Command("systemctl", "--version").Output()
	if err != nil {
		return 0
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

// Kind returns "systemd".
func (s *Systemd) Kind() string { return KindSystemd }

// LimitsEnforced is true: MemoryMax and CPUQuota are set on the unit.
func (s *Systemd) LimitsEnforced() bool { return true }

// Close does nothing: units keep running.
func (s *Systemd) Close() error { return nil }

func unitName(name string) string { return "esm-" + name + ".service" }

func (s *Systemd) description(name string) string {
	return "ESM instance " + name + " (root " + s.st.Path() + ")"
}

// systemctl runs a systemd command with a fixed locale and UTC timestamps.
func (s *Systemd) systemctl(ctx context.Context, bin string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", commandLabel(bin, args), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// commandLabel names a command for error messages. It never includes the
// server's own command line (everything after "--", which can hold
// passwords) or environment values.
func commandLabel(bin string, args []string) string {
	keep := []string{bin}
	for _, a := range args {
		if a == "--" {
			keep = append(keep, "-- <server command line>")
			break
		}
		if strings.HasPrefix(a, "--setenv=") {
			a = "--setenv=<hidden>"
		}
		keep = append(keep, a)
	}
	return strings.Join(gameserver.MaskArgs(keep), " ")
}

type unitInfo struct {
	loaded      bool
	active      string // ActiveState
	sub         string
	pid         int
	description string
	exitStatus  int
	exitKnown   bool
	started     time.Time
	exited      time.Time
}

func (u unitInfo) running() bool {
	switch u.active {
	case "active", "activating", "deactivating", "reloading":
		return u.pid > 0
	}
	return false
}

func (s *Systemd) show(name string) (unitInfo, error) {
	out, err := s.systemctl(context.Background(), "systemctl", "--user", "show", unitName(name), "--no-pager",
		"-p", "LoadState,ActiveState,SubState,MainPID,ExecMainStatus,ExecMainCode,Description,ExecMainStartTimestamp,ExecMainExitTimestamp")
	if err != nil {
		return unitInfo{}, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			kv[k] = v
		}
	}
	u := unitInfo{
		loaded:      kv["LoadState"] == "loaded",
		active:      kv["ActiveState"],
		sub:         kv["SubState"],
		description: kv["Description"],
		started:     parseSystemdTime(kv["ExecMainStartTimestamp"]),
		exited:      parseSystemdTime(kv["ExecMainExitTimestamp"]),
	}
	u.pid, _ = strconv.Atoi(kv["MainPID"])
	// ExecMainCode 1 is CLD_EXITED: ExecMainStatus is then an exit code.
	if kv["ExecMainCode"] == "1" && !u.running() {
		if n, err := strconv.Atoi(kv["ExecMainStatus"]); err == nil {
			u.exitStatus, u.exitKnown = n, true
		}
	}
	return u, nil
}

// parseSystemdTime parses "Wed 2026-11-11 22:05:01 UTC" (TZ=UTC is set).
func parseSystemdTime(v string) time.Time {
	if v == "" || v == "n/a" {
		return time.Time{}
	}
	t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func (s *Systemd) ours(name string, u unitInfo) bool {
	return u.loaded && u.description == s.description(name)
}

// escapeArg protects "$" from the service manager's variable expansion on
// systemd versions without --expand-environment.
func (s *Systemd) escapeArg(a string) string {
	if s.expandFlag {
		return a
	}
	return strings.ReplaceAll(a, "$", "$$")
}

// Start runs spec as the transient unit esm-<name>.service.
func (s *Systemd) Start(ctx context.Context, spec Spec) error {
	if err := checkSpec(s.st, spec); err != nil {
		return err
	}
	unlock := s.st.Lock("supervisor:" + spec.Name)
	defer unlock()
	u, err := s.show(spec.Name)
	if err != nil {
		return err
	}
	if u.loaded && !s.ours(spec.Name, u) {
		return fmt.Errorf("%w (unit %s has description %q)", ErrNotOurs, unitName(spec.Name), u.description)
	}
	if u.running() {
		return ErrAlreadyRunning
	}
	if u.loaded {
		// A failed unit of ours that was not collected yet.
		s.systemctl(ctx, "systemctl", "--user", "reset-failed", unitName(spec.Name))
	}
	if spec.StopFile != "" {
		// A stop request left over from a killed run would stop the new
		// server at once.
		rel, _ := s.st.Rel(spec.StopFile)
		if err := s.st.Remove(rel); err != nil {
			return fmt.Errorf("supervisor: remove old stop request: %w", err)
		}
	}
	outRel, _ := s.st.Rel(spec.OutputFile)
	if f, err := s.st.OpenAppend(outRel, 0o600); err == nil {
		f.Close() // make sure it exists with our permissions
	} else {
		return fmt.Errorf("supervisor: open output: %w", err)
	}
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + "esm-" + spec.Name,
		"-p", "Description=" + s.description(spec.Name),
		"-p", "KillSignal=SIGTERM",
		"-p", "TimeoutStopSec=" + strconv.Itoa(int(s.opts.StopTimeout.Round(time.Second)/time.Second)),
		"-p", "StandardOutput=append:" + spec.OutputFile,
		"-p", "StandardError=append:" + spec.OutputFile,
		"--working-directory=" + spec.Dir,
	}
	if spec.Limits.MemoryMaxBytes > 0 {
		args = append(args, "-p", "MemoryMax="+strconv.FormatInt(spec.Limits.MemoryMaxBytes, 10))
	}
	if spec.Limits.CPUQuotaPercent > 0 {
		args = append(args, "-p", "CPUQuota="+strconv.Itoa(spec.Limits.CPUQuotaPercent)+"%")
	}
	for _, kv := range spec.Env {
		args = append(args, "--setenv="+kv)
	}
	if s.expandFlag {
		args = append(args, "--expand-environment=no")
	}
	args = append(args, "--", s.escapeArg(spec.Executable))
	for _, a := range spec.Args {
		args = append(args, s.escapeArg(a))
	}
	if _, err := s.systemctl(ctx, "systemd-run", args...); err != nil {
		// systemd-run may repeat parts of the command line in its own
		// messages; hide the password values.
		return fmt.Errorf("supervisor: start %s: %s", spec.Name, gameserver.HideValues(err.Error(), gameserver.SecretValues(spec.Args)))
	}
	s.mu.Lock()
	s.seen[spec.Name] = time.Now()
	s.mu.Unlock()
	s.log.Info("server started", "instance", spec.Name, "unit", unitName(spec.Name), "mode", KindSystemd)
	return nil
}

// Status reports the unit of an instance.
func (s *Systemd) Status(name string) (Proc, error) {
	if err := checkName(s.st, name); err != nil {
		return Proc{Name: name}, err
	}
	u, err := s.show(name)
	if err != nil {
		return Proc{Name: name}, err
	}
	if !u.loaded || !s.ours(name, u) {
		s.mu.Lock()
		_, seen := s.seen[name]
		s.mu.Unlock()
		if !seen {
			return Proc{Name: name}, ErrNotFound
		}
		// Collected after it ended: no exit code is left to read.
		return Proc{Name: name}, nil
	}
	pr := Proc{Name: name, Running: u.running(), StartedAt: u.started}
	if pr.Running {
		pr.PID = u.pid
	} else {
		pr.ExitedAt = u.exited
		if u.exitKnown {
			code := u.exitStatus
			pr.ExitCode = &code
		}
	}
	return pr, nil
}

// Adopt lists esm-* units of instances in this root that are running.
func (s *Systemd) Adopt() ([]Proc, error) {
	out, err := s.systemctl(context.Background(), "systemctl", "--user", "list-units", "--all", "--plain",
		"--no-legend", "--no-pager", "--type=service", "esm-*.service")
	if err != nil {
		return nil, err
	}
	var procs []Proc
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(f[0], "esm-"), ".service")
		if !ValidName(name) || !known(s.st, name) {
			continue
		}
		u, err := s.show(name)
		if err != nil || !s.ours(name, u) || !u.running() {
			continue
		}
		s.mu.Lock()
		s.seen[name] = time.Now()
		s.mu.Unlock()
		procs = append(procs, Proc{Name: name, Running: true, PID: u.pid, StartedAt: u.started})
		s.log.Info("adopted running server", "instance", name, "unit", unitName(name), "pid", u.pid)
	}
	return procs, nil
}

// Stop stops the unit: systemctl --user stop, waiting up to timeout, then
// SIGKILL through systemd.
func (s *Systemd) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := checkName(s.st, name); err != nil {
		return err
	}
	u, err := s.show(name)
	if err != nil {
		return err
	}
	if !u.loaded {
		return nil
	}
	if !s.ours(name, u) {
		return fmt.Errorf("%w (unit %s has description %q)", ErrNotOurs, unitName(name), u.description)
	}
	if !u.running() {
		return nil
	}
	if _, err := s.systemctl(ctx, "systemctl", "--user", "stop", "--no-block", unitName(name)); err != nil {
		return err
	}
	if s.waitStopped(ctx, name, timeout) {
		return nil
	}
	s.log.Warn("server did not stop in time; killing it", "instance", name, "unit", unitName(name))
	if _, err := s.systemctl(context.Background(), "systemctl", "--user", "kill", "--signal=SIGKILL", unitName(name)); err != nil {
		if u, serr := s.show(name); serr == nil && !u.running() {
			return nil
		}
		return err
	}
	if s.waitStopped(context.Background(), name, 10*time.Second) {
		return nil
	}
	return fmt.Errorf("%w (unit %s)", ErrStillRunning, unitName(name))
}

func (s *Systemd) waitStopped(ctx context.Context, name string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		u, err := s.show(name)
		if err == nil && (!u.loaded || !u.running() || !s.ours(name, u)) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}
