// Package service wires ESM together and runs it: it opens the root, loads
// the config, starts the supervisor, the build and instance managers, the
// API and the web page, and listens on TCP and on the unix socket. Shutdown stops all of
// that and leaves game servers running.
package service

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/netbind"
	"github.com/TriadStudios/echoes-server-manager/internal/page"
	"github.com/TriadStudios/echoes-server-manager/internal/release"
	"github.com/TriadStudios/echoes-server-manager/internal/steam"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
)

// Options configure Start. Only Root is required.
type Options struct {
	Root    string
	Version string
	// Stderr receives the setup code and, unless Logger is set, the JSON
	// log (default os.Stderr).
	Stderr io.Writer
	Logger *slog.Logger

	// Tuning, mostly for tests. Zero values take the defaults.
	WatchInterval     time.Duration
	SchedulerInterval time.Duration
	KeepAlive         time.Duration
	PortHost          string
	BuildLimits       builds.Limits
	HTTPClient        *http.Client
	AuthOptions       auth.Options
	// BodyTimeout and WriteTimeout override the API's request body and
	// reply deadlines (api.DefaultBodyTimeout, api.DefaultWriteTimeout).
	BodyTimeout  time.Duration
	WriteTimeout time.Duration
	// FailDelay overrides how long a failed login waits for its answer
	// (api.DefaultFailDelay).
	FailDelay time.Duration
	// SteamTick overrides how often Steam sources are looked at
	// (steam.DefaultTick).
	SteamTick time.Duration
	// ReleaseAPI, ReleaseHTTP and ReleaseKey override where releases are
	// looked up, the client that fetches them and the key that checks them
	// (release.DefaultAPI, an https-only client, release.PublicKey). In a
	// test binary nothing is looked up unless ReleaseAPI is set.
	ReleaseAPI  string
	ReleaseHTTP *http.Client
	ReleaseKey  ed25519.PublicKey
	// Executable is the binary a self-update replaces (default: the
	// running one).
	Executable string
	// RestartedBy says what starts ESM again after it ends, "" for nothing
	// (default: restartedBy).
	RestartedBy func() string
}

// Service is a running ESM.
type Service struct {
	cfg   *config.Config
	st    *store.Store
	log   *slog.Logger
	audit *audit.Log
	auth  *auth.Manager
	bus   *events.Bus
	sup   supervisor.Supervisor
	inst  *instances.Manager
	steam *steam.Manager
	rel   *release.Client
	tcp   net.Listener
	sock  net.Listener
	web   *http.Server
	local *http.Server
	errc  chan error
	once  sync.Once
	// exit is closed when the service should end after a self-update;
	// restarts, set just before, says whether its service manager starts
	// it again.
	exit     chan struct{}
	exitOnce sync.Once
	restarts bool
	// updating is held while a self-update runs; updated is set once one
	// put a new binary in place (the service ends a moment later).
	updating sync.Mutex
	updated  bool
	exe      string
	version  string
	// releaseOff says why no release is ever looked up ("" when they are).
	releaseOff string
	// restartedBy says what starts ESM again after it ends ("" nothing).
	restartedBy func() string
}

// Start opens the root and starts serving. It fails when the listen rule
// is broken, when another ESM already serves this root, or when ESM runs
// as the superuser.
func Start(o Options) (_ *Service, err error) {
	if err := config.CheckNotRoot(); err != nil {
		return nil, err
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	st, err := store.Open(o.Root)
	if err != nil {
		return nil, err
	}
	var cleanups []func()
	defer func() {
		if err != nil {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}
		}
	}()
	cleanups = append(cleanups, func() { st.Close() })
	cfg, created, err := config.Load(st)
	if err != nil {
		return nil, err
	}
	if err := cfg.CheckListen(); err != nil {
		return nil, err
	}
	log := o.Logger
	if log == nil {
		var level slog.Level
		level.UnmarshalText([]byte(cfg.LogLevel))
		log = slog.New(slog.NewJSONHandler(o.Stderr, &slog.HandlerOptions{Level: level}))
	}
	if created {
		log.Info("wrote the default config", "path", config.FileName)
	}
	s := &Service{cfg: cfg, st: st, log: log, errc: make(chan error, 2), exit: make(chan struct{}), exe: o.Executable, version: o.Version,
		restartedBy: o.RestartedBy}
	if s.restartedBy == nil {
		s.restartedBy = restartedBy
	}

	if s.audit, err = audit.Open(st); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { s.audit.Close() })
	if s.auth, err = auth.New(st, o.AuthOptions); err != nil {
		return nil, err
	}
	code, err := s.auth.EnsureSetupCode()
	if err != nil {
		return nil, err
	}
	if code != "" {
		fmt.Fprintf(o.Stderr, "ESM setup code: %s (works once, for 24 hours; use it to create the first admin)\n", code)
		// The code goes to stderr as a plain line, once, for whoever starts
		// ESM. The JSON log may be collected and kept elsewhere, so it only
		// says that a code was made.
		log.Info("setup code created; it was printed once to stderr")
	} else if !s.auth.HasAdmin() {
		log.Info("waiting for setup; the setup code was shown when it was made (delete secrets/setup.json and restart for a new one)")
	}
	s.bus = events.NewBus(4096)
	s.audit.SetNotify(func(e audit.Entry) { s.bus.Publish(events.TypeAudit, "", e) })

	if s.sup, err = supervisor.New(st, supervisor.Options{Kind: cfg.Supervisor, Logger: log}); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { s.sup.Close() })
	// Local build sources are read from the import folder only.
	importDir := cfg.ImportDir()
	if err := os.MkdirAll(importDir, 0o700); err != nil {
		log.Warn("cannot create the build import folder", "path", importDir, "err", err)
	}
	bm := builds.New(st, builds.Options{Bus: s.bus, Logger: log, Limits: o.BuildLimits, HTTPClient: o.HTTPClient, ImportDir: importDir,
		SteamAppID: cfg.SteamAppID})
	if err := bm.CleanTemp(); err != nil {
		log.Warn("cannot clean the tmp folder", "err", err)
	}
	s.steam = steam.New(steam.Options{Store: st, Config: cfg, Builds: bm, Audit: s.audit, Logger: log, Tick: o.SteamTick,
		Installed: s.steamInstalled})
	cleanups = append(cleanups, s.steam.Close)
	if s.inst, err = instances.New(instances.Options{
		Store: st, Config: cfg, Supervisor: s.sup, Builds: bm, Bus: s.bus, Logger: log,
		WatchInterval: o.WatchInterval, SchedulerInterval: o.SchedulerInterval,
		SteamTarget: s.steam.Target, Audit: s.audit,
	}); err != nil {
		return nil, err
	}
	s.rel = &release.Client{API: o.ReleaseAPI, HTTP: o.ReleaseHTTP, Key: o.ReleaseKey, UserAgent: "esm/" + o.Version}
	if o.ReleaseAPI == "" && testing.Testing() {
		s.releaseOff = "releases are not looked up in tests"
	}
	checkNote := s.releaseOff
	if !cfg.UpdateCheck {
		checkNote = "update_check is off in " + config.FileName
	}
	checker := release.NewChecker(s.rel, o.Version, 0, checkNote == "", checkNote)

	// Listen before adopting servers, so a second ESM on the same root
	// stops here.
	if s.sock, err = listenSocket(cfg.SocketPath()); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { s.sock.Close() })
	if s.tcp, err = netbind.Listen("tcp", cfg.Listen); err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	cleanups = append(cleanups, func() { s.tcp.Close() })
	useTLS := cfg.TLSCert != ""
	if useTLS {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load tls_cert and tls_key: %w", err)
		}
		s.tcp = tls.NewListener(s.tcp, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}

	if err := s.inst.Resume(); err != nil {
		return nil, err
	}
	srv := api.New(api.Options{
		Version: o.Version, Config: cfg, Auth: s.auth, Audit: s.audit, Bus: s.bus, Instances: s.inst,
		Builds: bm, Supervisor: s.sup, Logger: log, TLS: useTLS, HTTPS: useTLS || cfg.BehindTLSProxy,
		Started: time.Now(), KeepAlive: o.KeepAlive, PortHost: o.PortHost,
		BodyTimeout: o.BodyTimeout, WriteTimeout: o.WriteTimeout, FailDelay: o.FailDelay,
		Listen: s.tcp.Addr().String(), PublicHosts: cfg.PublicHosts,
		Steam: s.steam, Release: checker, SelfUpdate: s.selfUpdate,
		Page: func(apiHandler http.Handler) http.Handler { return page.NewWithLogger(apiHandler, log) },
	})
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	s.web = newHTTPServer(srv.Handler(), errLog)
	s.local = newHTTPServer(srv.SocketHandler(), errLog)
	go s.serve(s.web, s.tcp)
	go s.serve(s.local, s.sock)
	s.steam.Start()
	log.Info("ESM started", "version", o.Version, "root", st.Path(), "listen", s.tcp.Addr().String(), "tls", useTLS,
		"socket", cfg.SocketPath(), "supervisor", s.sup.Kind(), "limits_enforced", s.sup.LimitsEnforced())
	if !s.sup.LimitsEnforced() {
		log.Warn("memory and CPU limits are not enforced in process mode")
	}
	return s, nil
}

// steamInstalled runs after a Steam source installed a new build. The
// source's builds beyond its newest keep_steam_builds (steam.Manager.Surplus)
// that no instance uses are removed; builds of other sources and builds not
// from Steam are left alone. A removal is logged and audited.
func (s *Service) steamInstalled(source string, _ builds.Build) {
	surplus, err := s.steam.Surplus(source)
	var removed []string
	if err == nil && len(surplus) > 0 {
		removed, err = s.inst.RemoveUnusedBuilds(surplus)
	}
	if len(removed) == 0 && err == nil {
		return
	}
	e := audit.Entry{Actor: audit.Actor{Kind: audit.ActorSystem, ID: "steam"}, Action: "build.prune",
		Target: strings.TrimSpace(steam.SourcePrefix + source + " " + strings.Join(removed, " ")), OK: err == nil}
	if len(removed) > 0 {
		s.log.Info("older builds of a Steam source removed", "source", source, "removed", removed, "keep_steam_builds", s.cfg.KeepSteamBuilds)
	}
	if err != nil {
		e.Error = err.Error()
		s.log.Warn("cannot remove older builds of a Steam source", "source", source, "err", err)
	}
	if aerr := s.audit.Append(e); aerr != nil {
		s.log.Error("cannot write the audit log", "err", aerr)
	}
}

// Timeouts of the HTTP servers.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 2 * time.Minute
)

// newHTTPServer makes one of ESM's HTTP servers (TCP or socket). The
// request headers must arrive within readHeaderTimeout and fit in
// api.MaxHeaderBytes, and an idle connection is closed after idleTimeout.
// There is no server-wide ReadTimeout or WriteTimeout: they would also end
// calls that rightly take long (a start that waits for ready, a build
// download, an event stream), and a ReadTimeout even cancels such a call
// while it waits. The API sets those deadlines per request instead: one
// for the request body, and one for the reply, counted from its first byte
// (each event of a stream gets its own).
func newHTTPServer(h http.Handler, errLog *log.Logger) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    api.MaxHeaderBytes,
		ErrorLog:          errLog,
	}
}

// listenSocket listens on the unix socket, removing a stale socket file
// first. A socket that still answers means another ESM serves this root.
func listenSocket(path string) (net.Listener, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	if err := checkSocketDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, 2*time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another ESM is already running on this root (%s answers)", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	l, err := listenUnix(path)
	if err != nil {
		return nil, fmt.Errorf("listen on the socket %s: %w", path, err)
	}
	socketListened(path)
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		l.Close()
		return nil, err
	}
	return l, nil
}

func (s *Service) serve(srv *http.Server, l net.Listener) {
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Error("server stopped", "err", err)
		s.errc <- err
	}
}

// Addr is the TCP address the API listens on.
func (s *Service) Addr() string { return s.tcp.Addr().String() }

// SocketPath is the unix socket path.
func (s *Service) SocketPath() string { return s.cfg.SocketPath() }

// Config is the loaded config.
func (s *Service) Config() *config.Config { return s.cfg }

// Err delivers a listener failure.
func (s *Service) Err() <-chan error { return s.errc }

// Exit is closed when the service should end: after a self-update put a
// new binary in place. Restarts says whether something starts it again.
func (s *Service) Exit() <-chan struct{} { return s.exit }

// Restarts says, once Exit is closed, whether ESM's service manager starts
// the new binary. It is false after a self-update with exit_anyway when
// nothing does: ESM then has to be started by hand.
func (s *Service) Restarts() bool { return s.restarts }

// restartedBy says what starts ESM again after it ends: "systemd" when
// systemd runs ESM as a service (systemd sets INVOCATION_ID for every
// service it starts; the unit of esm service install has Restart=always),
// "" otherwise. ESM knows of nothing on Windows that does.
func restartedBy() string {
	if runtime.GOOS == "linux" && os.Getenv("INVOCATION_ID") != "" {
		return "systemd"
	}
	return ""
}

// noServiceManager is why a self-update is refused when nothing starts ESM
// again after it ends.
func noServiceManager() string {
	why := "ESM does not run under systemd (esm service install writes a unit that starts it again)"
	if runtime.GOOS != "linux" {
		why = "ESM has no service manager on " + runtime.GOOS
	}
	return "nothing would start ESM again after the update ends it: " + why +
		". Update on the box with esm update-self and restart ESM yourself, or set exit_anyway to update and end ESM anyway; it then has to be started by hand"
}

// selfUpdate replaces ESM's binary with the newest release's (see
// release.Client.Update) and, when that worked, asks the service to end a
// moment later so the answer still reaches the caller. Game servers keep
// running; the new ESM adopts them. When nothing starts ESM again after it
// ends, it refuses (409 no_service_manager) and changes nothing, unless
// exitAnyway is set. restarting says whether something starts ESM again.
func (s *Service) selfUpdate(ctx context.Context, force, exitAnyway bool) (res release.Result, restarting bool, err error) {
	if s.releaseOff != "" {
		return release.Result{}, false, errors.New(s.releaseOff)
	}
	by := s.restartedBy()
	if by == "" && !exitAnyway {
		return release.Result{}, false, fault.New(fault.Conflict, "no_service_manager", "%s", noServiceManager())
	}
	// One at a time, and none after one worked: a second update before
	// this ESM ends would keep the new binary as the previous one.
	if !s.updating.TryLock() {
		return release.Result{}, false, release.ErrBusy
	}
	defer s.updating.Unlock()
	if s.updated {
		return release.Result{}, false, fmt.Errorf("%w: ESM was updated already and ends in a moment", release.ErrBusy)
	}
	exe := s.exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return release.Result{}, false, err
		}
		// Like esm update-self, so both use the same lock file.
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return release.Result{}, false, err
		}
	}
	res, err = s.rel.Update(ctx, s.version, exe, runtime.GOOS, runtime.GOARCH, force)
	if err != nil {
		return res, false, err
	}
	s.updated = true
	if by != "" {
		s.log.Info("ESM binary updated; ending so the service manager starts the new one", "from", res.From, "to", res.To,
			"previous", res.Previous, "restarted_by", by)
	} else {
		s.log.Warn("ESM binary updated; ending, and nothing starts ESM again: start it by hand", "from", res.From, "to", res.To,
			"previous", res.Previous)
	}
	restarts := by != ""
	time.AfterFunc(time.Second, func() {
		s.exitOnce.Do(func() {
			s.restarts = restarts
			close(s.exit)
		})
	})
	return res, restarts, nil
}

// Shutdown stops accepting requests, ends event streams, stops watching
// and scheduling, flushes the audit log and removes the socket. Game
// servers keep running.
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.once.Do(func() {
		s.bus.Close()
		err = errors.Join(s.web.Shutdown(ctx), s.local.Shutdown(ctx))
		s.steam.Close()
		n := s.inst.Close()
		s.sup.Close()
		if aerr := s.audit.Close(); aerr != nil {
			err = errors.Join(err, aerr)
		}
		if rerr := os.Remove(s.cfg.SocketPath()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		s.log.Info(fmt.Sprintf("leaving %d game servers running", n), "running", n)
		s.st.Close()
	})
	return err
}
