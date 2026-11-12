package instances

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
)

// Folders inside the ESM root.
const (
	instancesDir = "instances"
	backupsDir   = "backups"
)

func instDir(name string) string     { return instancesDir + "/" + name }
func instFile(name string) string    { return instDir(name) + "/instance.json" }
func secretsFile(name string) string { return instDir(name) + "/secrets.json" }
func dataDir(name string) string     { return instDir(name) + "/data" }
func consoleFile(name string) string { return instDir(name) + "/console.log" }
func stateFile(name string) string   { return instDir(name) + "/run/state.json" }
func serverLog(name string) string   { return dataDir(name) + "/" + gameserver.ServerLogRel }

// ErrClosed is returned once the manager is closed.
var ErrClosed = fault.New(fault.Conflict, "shutting_down", "ESM is shutting down")

// Options configure a Manager.
type Options struct {
	Store      *store.Store
	Config     *config.Config
	Supervisor supervisor.Supervisor
	Builds     *builds.Manager
	Bus        *events.Bus
	Logger     *slog.Logger

	// WatchInterval is how often running servers are checked (default 1s).
	WatchInterval time.Duration
	// SchedulerInterval is how often schedules are checked (default 15s).
	SchedulerInterval time.Duration
	// ProbeTimeout bounds one query-port probe (default 200ms).
	ProbeTimeout time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// View is an instance as the API shows it: settings, which secrets are set,
// and the live status.
type View struct {
	Instance
	Secrets SecretFlags `json:"secrets"`
	Status  Status      `json:"status"`
}

// LogLine is the data of a "log" event.
type LogLine struct {
	Source string `json:"source"` // server | console
	Line   string `json:"line"`
}

// Manager runs the instances of one ESM root.
type Manager struct {
	st     *store.Store
	cfg    *config.Config
	sup    supervisor.Supervisor
	builds *builds.Manager
	bus    *events.Bus
	log    *slog.Logger
	opts   Options
	games  config.PortRange
	query  config.PortRange

	mu     sync.Mutex // guards insts and port and build bookkeeping
	insts  map[string]*rt
	closed bool
	quit   chan struct{}
	wg     sync.WaitGroup
}

// rt is the runtime record of one instance.
type rt struct {
	name string
	op   sync.Mutex // serializes life-cycle operations

	mu           sync.Mutex // guards everything below
	cfg          Instance
	status       Status
	crashes      []time.Time
	gen          uint64 // bumped whenever the current run ends or is replaced
	cur          *run
	restartTimer *time.Timer
	nextRestart  time.Time
	nextBackup   time.Time
	deleted      bool
}

// run is one start of a server process.
type run struct {
	gen         uint64
	autoRestart bool // a crash before ready leads to a restart
	done        chan struct{}
	once        sync.Once
	err         error
}

func (r *run) finish(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.done)
	})
}

// New loads every instance under the root. Call Resume to adopt running
// servers and begin watching.
func New(opts Options) (*Manager, error) {
	if opts.WatchInterval <= 0 {
		opts.WatchInterval = time.Second
	}
	if opts.SchedulerInterval <= 0 {
		opts.SchedulerInterval = 15 * time.Second
	}
	if opts.ProbeTimeout <= 0 {
		opts.ProbeTimeout = 200 * time.Millisecond
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Bus == nil {
		opts.Bus = events.NewBus(1)
	}
	g, err := config.ParsePortRange(opts.Config.GamePorts)
	if err != nil {
		return nil, err
	}
	q, err := config.ParsePortRange(opts.Config.QueryPorts)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		st: opts.Store, cfg: opts.Config, sup: opts.Supervisor, builds: opts.Builds, bus: opts.Bus,
		log: opts.Logger, opts: opts, games: g, query: q,
		insts: make(map[string]*rt), quit: make(chan struct{}),
	}
	if err := m.st.MkdirAll(instancesDir, 0o700); err != nil {
		return nil, err
	}
	entries, err := m.st.ReadDir(instancesDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		var in Instance
		if err := m.st.ReadJSON(instFile(e.Name()), &in); err != nil {
			m.log.Warn("skipping unreadable instance", "instance", e.Name(), "err", err)
			continue
		}
		in.normalize()
		if in.Name != e.Name() {
			m.log.Warn("skipping instance whose name does not match its folder", "folder", e.Name(), "name", in.Name)
			continue
		}
		r := &rt{name: in.Name, cfg: in}
		r.status = Status{Name: in.Name, State: Stopped, Since: m.now(), Build: in.Build}
		var last Status
		if err := m.st.ReadJSON(stateFile(in.Name), &last); err == nil {
			if last.GaveUp || last.State == Failed {
				r.status.State, r.status.Reason, r.status.GaveUp = Failed, last.Reason, last.GaveUp
			} else if last.State.active() {
				r.status.Reason = "found not running when ESM started"
			}
		}
		m.insts[in.Name] = r
	}
	return m, nil
}

func (m *Manager) now() time.Time { return m.opts.Now().UTC() }

// Resume adopts servers that are still running from an earlier ESM run,
// starts the autostart instances that are not running, and starts the
// scheduler. Call it once after New.
func (m *Manager) Resume() error {
	procs, err := m.sup.Adopt()
	if err != nil {
		m.log.Warn("cannot look for running servers", "err", err)
	}
	adopted := map[string]bool{}
	for _, p := range procs {
		in := m.lookup(p.Name)
		if in == nil {
			continue
		}
		adopted[p.Name] = true
		m.adoptRun(in, p)
	}
	for _, in := range m.all() {
		in.mu.Lock()
		auto := in.cfg.Autostart && !in.status.GaveUp
		in.mu.Unlock()
		if auto && !adopted[in.name] {
			go func(name string) {
				if _, err := m.Start(context.Background(), name, false); err != nil {
					m.log.Warn("autostart failed", "instance", name, "err", err)
				}
			}(in.name)
		}
	}
	if m.track() {
		go m.schedLoop()
	}
	return nil
}

// Close stops watching and scheduling. Game servers keep running; Close
// returns how many are still running.
func (m *Manager) Close() int {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0
	}
	m.closed = true
	close(m.quit)
	all := m.allLocked()
	m.mu.Unlock()
	for _, in := range all {
		in.mu.Lock()
		if in.restartTimer != nil {
			in.restartTimer.Stop()
			in.restartTimer = nil
		}
		in.mu.Unlock()
	}
	m.wg.Wait()
	n := 0
	for _, in := range all {
		if pr, err := m.sup.Status(in.name); err == nil && pr.Running {
			n++
		}
	}
	return n
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// track registers a goroutine that Close waits for. It returns false once
// the manager is closed; checking and adding under one lock keeps Add from
// racing with Close's Wait.
func (m *Manager) track() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

func (m *Manager) lookup(name string) *rt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.insts[name]
}

func (m *Manager) get(name string) (*rt, error) {
	if in := m.lookup(name); in != nil {
		return in, nil
	}
	return nil, fault.NotFoundf("instance %q not found", name)
}

func (m *Manager) all() []*rt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.allLocked()
}

func (m *Manager) allLocked() []*rt {
	out := make([]*rt, 0, len(m.insts))
	for _, in := range m.insts {
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b *rt) int {
		if a.name < b.name {
			return -1
		}
		if a.name > b.name {
			return 1
		}
		return 0
	})
	return out
}

// setLocked changes the state, then mirrors and publishes it. in.mu must
// be held.
func (m *Manager) setLocked(in *rt, s State, reason string) {
	in.status.State = s
	in.status.Reason = reason
	in.status.Since = m.now()
	m.emitLocked(in)
}

// emitLocked mirrors the status to state.json and publishes it. in.mu must
// be held so events and the file follow the order of changes.
func (m *Manager) emitLocked(in *rt) {
	st := in.status
	if in.deleted {
		return
	}
	if err := m.st.WriteJSON(stateFile(in.name), st, 0o600); err != nil {
		m.log.Warn("cannot write state file", "instance", in.name, "err", err)
	}
	m.bus.Publish(events.TypeState, in.name, st)
	m.log.Info("instance state", "instance", in.name, "state", st.State, "reason", st.Reason)
}

func (m *Manager) view(in *rt) View {
	in.mu.Lock()
	v := View{Instance: in.cfg.clone(), Status: in.status}
	in.mu.Unlock()
	v.Secrets = m.secretFlags(in.name)
	return v
}

// List returns every instance, sorted by name.
func (m *Manager) List() []View {
	out := []View{}
	for _, in := range m.all() {
		out = append(out, m.view(in))
	}
	return out
}

// Get returns one instance.
func (m *Manager) Get(name string) (View, error) {
	in, err := m.get(name)
	if err != nil {
		return View{}, err
	}
	return m.view(in), nil
}

// Status returns the live status of one instance.
func (m *Manager) Status(name string) (Status, error) {
	in, err := m.get(name)
	if err != nil {
		return Status{}, err
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.status, nil
}

// Create makes a new instance. It is stopped until started.
func (m *Manager) Create(req CreateRequest) (View, error) {
	if !ValidName(req.Name) {
		return View{}, invalid("name", "must match %s", nameRe)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return View{}, ErrClosed
	}
	if _, ok := m.insts[req.Name]; ok || m.st.Exists(instDir(req.Name)) {
		return View{}, fault.New(fault.Conflict, "instance_exists", "instance %s already exists", req.Name)
	}
	channel := builds.ChannelLive
	if req.Channel != nil {
		channel = *req.Channel
	}
	var b builds.Build
	if req.Build == "" {
		list, err := m.builds.List()
		if err != nil {
			return View{}, err
		}
		i := slices.IndexFunc(list, func(b builds.Build) bool { return b.Channel == channel })
		if i < 0 {
			return View{}, fault.New(fault.Invalid, "no_build", "no %s build is installed; install one first", channel)
		}
		b = list[i]
	} else {
		var err error
		if b, err = m.builds.Get(req.Build); err != nil {
			if fault.Is(err, fault.NotFound) {
				return View{}, fault.New(fault.Invalid, "no_build", "build %s is not installed", req.Build)
			}
			return View{}, err
		}
		if req.Channel == nil {
			channel = b.Channel
		}
	}
	if b.Channel != channel {
		return View{}, invalid("channel", "build %s is a %s build; the instance channel must match", b.Version, b.Channel)
	}
	now := m.now()
	in := Instance{
		Name: req.Name, DisplayName: req.Name, Channel: channel, Build: b.Version, World: req.Name,
		MaxPlayers: DefaultMaxPlayers, Listed: channel == builds.ChannelLive, RestartOnCrash: true,
		Backup: BackupPolicy{Keep: DefaultBackupKeep}, Created: now, Updated: now,
	}
	in.normalize()
	req.Settings.apply(&in)
	if err := in.Validate(); err != nil {
		return View{}, err
	}
	ports, err := m.allocPortsLocked(in.BindIP, in.Ports, "", nil)
	if err != nil {
		return View{}, err
	}
	in.Ports = ports
	for _, d := range []string{dataDir(in.Name), instDir(in.Name) + "/run"} {
		if err := m.st.MkdirAll(d, 0o700); err != nil {
			return View{}, err
		}
	}
	if err := m.st.WriteJSON(instFile(in.Name), in, 0o600); err != nil {
		m.st.RemoveAll(instDir(in.Name))
		return View{}, err
	}
	r := &rt{name: in.Name, cfg: in}
	r.status = Status{Name: in.Name, Build: in.Build}
	m.insts[in.Name] = r
	r.mu.Lock()
	m.setLocked(r, Stopped, "created")
	r.mu.Unlock()
	m.log.Info("instance created", "instance", in.Name, "build", in.Build, "game_port", ports.Game, "query_port", ports.Query)
	return m.view(r), nil
}

// Patch changes settings. Changes to a running instance take effect at the
// next start; restartRequired says so.
func (m *Manager) Patch(name string, req PatchRequest) (v View, restartRequired bool, err error) {
	in, err := m.get(name)
	if err != nil {
		return View{}, false, err
	}
	if req.Build != nil {
		return View{}, false, invalid("build", "is changed with an update, not here")
	}
	in.op.Lock()
	defer in.op.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	in.mu.Lock()
	old := in.cfg.clone()
	active := in.status.State.active()
	in.mu.Unlock()
	cfg := old.clone()
	portsGiven := req.Settings.apply(&cfg)
	if err := cfg.Validate(); err != nil {
		return View{}, false, err
	}
	if cfg.Channel != old.Channel {
		b, err := m.builds.Get(cfg.Build)
		if err == nil && b.Channel != cfg.Channel {
			return View{}, false, invalid("channel", "build %s is a %s build; update to a %s build instead", b.Version, b.Channel, cfg.Channel)
		}
	}
	if portsGiven {
		// The running server of this instance holds its current ports, so
		// those are not checked for binding.
		own := map[int]bool{}
		if active && cfg.BindIP == old.BindIP {
			own[old.Ports.Game], own[old.Ports.Query] = true, true
		}
		ports, err := m.allocPortsLocked(cfg.BindIP, cfg.Ports, name, own)
		if err != nil {
			return View{}, false, err
		}
		cfg.Ports = ports
	}
	cfg.Updated = m.now()
	if err := m.st.WriteJSON(instFile(name), cfg, 0o600); err != nil {
		return View{}, false, err
	}
	in.mu.Lock()
	in.cfg = cfg
	in.nextRestart, in.nextBackup = time.Time{}, time.Time{}
	if active {
		in.status.RestartRequired = true
		m.emitLocked(in)
	}
	in.mu.Unlock()
	return m.view(in), active, nil
}

// Delete removes a stopped instance. Its backups are kept unless
// purgeBackups is set.
func (m *Manager) Delete(ctx context.Context, name string, purgeBackups bool) error {
	in, err := m.get(name)
	if err != nil {
		return err
	}
	in.op.Lock()
	defer in.op.Unlock()
	in.mu.Lock()
	state := in.status.State
	in.mu.Unlock()
	if pr, err := m.sup.Status(name); state.active() || (err == nil && pr.Running) {
		return fault.New(fault.Conflict, "instance_running", "instance %s is running; stop it first", name)
	}
	m.mu.Lock()
	delete(m.insts, name)
	m.mu.Unlock()
	in.mu.Lock()
	in.gen++
	if in.restartTimer != nil {
		in.restartTimer.Stop()
		in.restartTimer = nil
	}
	in.deleted = true
	st := in.status
	in.mu.Unlock()
	if err := m.st.RemoveAll(instDir(name)); err != nil {
		return err
	}
	if purgeBackups {
		if err := m.st.RemoveAll(backupsDir + "/" + name); err != nil {
			return err
		}
	}
	st.State, st.Reason, st.Since, st.PID = Deleted, "deleted", m.now(), 0
	m.bus.Publish(events.TypeState, name, st)
	m.log.Info("instance deleted", "instance", name, "purge_backups", purgeBackups)
	return nil
}

// usedPortsLocked maps the ports of every other instance to its name.
func (m *Manager) usedPortsLocked(except string) map[int]string {
	used := map[int]string{}
	for name, in := range m.insts {
		if name == except {
			continue
		}
		in.mu.Lock()
		used[in.cfg.Ports.Game] = name
		used[in.cfg.Ports.Query] = name
		in.mu.Unlock()
	}
	return used
}

// allocPortsLocked fills in zero ports from the ranges and checks every
// port: not used by another instance and, unless it is in own, bindable now
// (UDP and TCP) on bindIP. m.mu must be held.
func (m *Manager) allocPortsLocked(bindIP string, want Ports, except string, own map[int]bool) (Ports, error) {
	used := m.usedPortsLocked(except)
	check := func(p int) error {
		if p < 1 || p > 65535 {
			return invalid("ports", "%d is not a port number", p)
		}
		if other, ok := used[p]; ok {
			return fault.New(fault.Conflict, "port_in_use", "port %d is used by instance %s", p, other)
		}
		if own[p] {
			return nil
		}
		if err := PortFree(bindIP, p); err != nil {
			return fault.New(fault.Conflict, "port_in_use", "port %d is busy on this box: %v", p, err)
		}
		return nil
	}
	first := func(r config.PortRange, not int) (int, error) {
		for p := r.Lo; p <= r.Hi; p++ {
			if p != not && check(p) == nil {
				return p, nil
			}
		}
		return 0, fault.New(fault.Conflict, "no_free_ports", "no free port in %s", r)
	}
	switch {
	case want.Game != 0 && want.Query != 0:
		if want.Game == want.Query {
			return Ports{}, invalid("ports", "game and query port must differ")
		}
		if err := check(want.Game); err != nil {
			return Ports{}, err
		}
		if err := check(want.Query); err != nil {
			return Ports{}, err
		}
		return want, nil
	case want.Game != 0:
		if err := check(want.Game); err != nil {
			return Ports{}, err
		}
		q, err := first(m.query, want.Game)
		return Ports{Game: want.Game, Query: q}, err
	case want.Query != 0:
		if err := check(want.Query); err != nil {
			return Ports{}, err
		}
		g, err := first(m.games, want.Query)
		return Ports{Game: g, Query: want.Query}, err
	}
	for i := 0; ; i++ {
		g, q := m.games.Lo+i, m.query.Lo+i
		if !m.games.Contains(g) || !m.query.Contains(q) {
			break
		}
		if check(g) == nil && check(q) == nil {
			return Ports{Game: g, Query: q}, nil
		}
	}
	return Ports{}, fault.New(fault.Conflict, "no_free_ports",
		"no free game/query port pair in %s and %s", m.games, m.query)
}

// PortFree reports whether port can be bound now for UDP and TCP on ip
// (all interfaces when ip is empty).
func PortFree(ip string, port int) error {
	host := ip
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	pc.Close()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	l.Close()
	return nil
}

// checkPortsBindable re-checks an instance's ports right before a start.
func checkPortsBindable(in Instance) error {
	for _, p := range []int{in.Ports.Game, in.Ports.Query} {
		if err := PortFree(in.BindIP, p); err != nil {
			return fault.New(fault.Conflict, "port_in_use", "port %d is busy on this box: %v", p, err)
		}
	}
	return nil
}

// readSecrets loads secrets.json (empty when missing).
func (m *Manager) readSecrets(name string) (Secrets, error) {
	var s Secrets
	err := m.st.ReadJSON(secretsFile(name), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return Secrets{}, nil
	}
	return s, err
}

func (m *Manager) secretFlags(name string) SecretFlags {
	s, _ := m.readSecrets(name)
	return SecretFlags{ServerPasswordSet: s.ServerPassword != "", AdminPasswordSet: s.AdminPassword != ""}
}

// SetSecrets changes the server and admin passwords. They are used from the
// next start; restartRequired says the instance is running.
func (m *Manager) SetSecrets(name string, req SecretsRequest) (flags SecretFlags, restartRequired bool, err error) {
	in, err := m.get(name)
	if err != nil {
		return SecretFlags{}, false, err
	}
	if req.ServerPassword != nil {
		if err := checkSecret("server_password", *req.ServerPassword); err != nil {
			return SecretFlags{}, false, err
		}
	}
	if req.AdminPassword != nil {
		if err := checkSecret("admin_password", *req.AdminPassword); err != nil {
			return SecretFlags{}, false, err
		}
	}
	in.op.Lock()
	defer in.op.Unlock()
	s, err := m.readSecrets(name)
	if err != nil {
		return SecretFlags{}, false, err
	}
	if req.ServerPassword != nil {
		s.ServerPassword = *req.ServerPassword
	}
	if req.AdminPassword != nil {
		s.AdminPassword = *req.AdminPassword
	}
	if err := m.st.WriteJSON(secretsFile(name), s, 0o600); err != nil {
		return SecretFlags{}, false, err
	}
	in.mu.Lock()
	active := in.status.State.active()
	if active {
		in.status.RestartRequired = true
		m.emitLocked(in)
	}
	in.mu.Unlock()
	return m.secretFlags(name), active, nil
}

// Warnings lists things an operator should know about an instance's
// settings on this box.
func (m *Manager) Warnings(v View) []string {
	w := []string{}
	if !m.sup.LimitsEnforced() && (v.Limits.MemoryMax != "" || v.Limits.CPUQuotaPercent > 0) {
		w = append(w, fmt.Sprintf("limits are not enforced by the %s supervisor", m.sup.Kind()))
	}
	if gameserver.PasswordsOnCommandLine && (v.Secrets.ServerPasswordSet || v.Secrets.AdminPasswordSet) && runtime.GOOS != "windows" {
		w = append(w, "passwords are passed on the server command line, where other local users can see them")
	}
	return w
}

// Cluster groups instance names by their cluster field.
type Cluster struct {
	Name      string   `json:"name"`
	Instances []string `json:"instances"`
}

// Clusters lists the clusters named by instances, sorted by name.
func (m *Manager) Clusters() []Cluster {
	byName := map[string][]string{}
	for _, in := range m.all() {
		in.mu.Lock()
		c := in.cfg.Cluster
		in.mu.Unlock()
		if c != "" {
			byName[c] = append(byName[c], in.name)
		}
	}
	out := []Cluster{}
	for name, list := range byName {
		out = append(out, Cluster{Name: name, Instances: list})
	}
	slices.SortFunc(out, func(a, b Cluster) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return out
}

// BuildUsers maps each build version to the instances that use it, as
// their current build or one of their kept previous builds.
func (m *Manager) BuildUsers() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildUsersLocked()
}

func (m *Manager) buildUsersLocked() map[string][]string {
	out := map[string][]string{}
	keep := m.cfg.KeepPreviousBuilds
	for name, in := range m.insts {
		in.mu.Lock()
		refs := append([]string{in.cfg.Build}, in.cfg.PreviousBuilds[:min(keep, len(in.cfg.PreviousBuilds))]...)
		in.mu.Unlock()
		for _, v := range refs {
			if !slices.Contains(out[v], name) {
				out[v] = append(out[v], name)
			}
		}
	}
	for v := range out {
		slices.Sort(out[v])
	}
	return out
}

// DeleteBuild removes a build no instance uses.
func (m *Manager) DeleteBuild(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if users := m.buildUsersLocked()[version]; len(users) > 0 {
		return fault.New(fault.Conflict, "build_in_use", "build %s is used by %v", version, users)
	}
	return m.builds.Remove(version)
}

// PruneBuilds removes every build that no instance uses as its current
// build or one of its kept previous builds.
func (m *Manager) PruneBuilds() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	users := m.buildUsersLocked()
	return m.builds.Prune(func(v string) bool { return len(users[v]) > 0 })
}

// setBuild switches the instance to build b with the given previous
// builds (trimmed to keep_previous_builds) and saves it.
func (m *Manager) setBuild(in *rt, b string, prev []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in.mu.Lock()
	defer in.mu.Unlock()
	cfg := in.cfg.clone()
	cfg.Build = b
	cfg.PreviousBuilds = slices.DeleteFunc(slices.Clone(prev), func(v string) bool { return v == b })
	cfg.PreviousBuilds = slices.Compact(cfg.PreviousBuilds)
	if len(cfg.PreviousBuilds) > m.cfg.KeepPreviousBuilds {
		cfg.PreviousBuilds = cfg.PreviousBuilds[:m.cfg.KeepPreviousBuilds]
	}
	cfg.Updated = m.now()
	cfg.normalize()
	if err := m.st.WriteJSON(instFile(in.name), cfg, 0o600); err != nil {
		return err
	}
	in.cfg = cfg
	return nil
}
