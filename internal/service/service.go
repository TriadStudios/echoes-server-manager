// Package service wires ESM together and runs it: it opens the root, loads
// the config, starts the supervisor, the build and instance managers and
// the API, and listens on TCP and on the unix socket. Shutdown stops all of
// that and leaves game servers running.
package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
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
	tcp   net.Listener
	sock  net.Listener
	web   *http.Server
	local *http.Server
	errc  chan error
	once  sync.Once
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
	s := &Service{cfg: cfg, st: st, log: log, errc: make(chan error, 2)}

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
		log.Info("setup code created", "code", code)
	} else if !s.auth.HasAdmin() {
		log.Info("waiting for setup; the setup code was shown when it was made (delete secrets/setup.json and restart for a new one)")
	}
	s.bus = events.NewBus(4096)
	s.audit.SetNotify(func(e audit.Entry) { s.bus.Publish(events.TypeAudit, "", e) })

	if s.sup, err = supervisor.New(st, supervisor.Options{Kind: cfg.Supervisor, StopTimeout: cfg.StopTimeout.D(), Logger: log}); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { s.sup.Close() })
	bm := builds.New(st, builds.Options{Bus: s.bus, Logger: log, Limits: o.BuildLimits, HTTPClient: o.HTTPClient})
	if err := bm.CleanTemp(); err != nil {
		log.Warn("cannot clean the tmp folder", "err", err)
	}
	if s.inst, err = instances.New(instances.Options{
		Store: st, Config: cfg, Supervisor: s.sup, Builds: bm, Bus: s.bus, Logger: log,
		WatchInterval: o.WatchInterval, SchedulerInterval: o.SchedulerInterval,
	}); err != nil {
		return nil, err
	}

	// Listen before adopting servers, so a second ESM on the same root
	// stops here.
	if s.sock, err = listenSocket(cfg.SocketPath()); err != nil {
		return nil, err
	}
	cleanups = append(cleanups, func() { s.sock.Close() })
	if s.tcp, err = net.Listen("tcp", cfg.Listen); err != nil {
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
		Builds: bm, Supervisor: s.sup, Logger: log, TLS: useTLS, Started: time.Now(),
		KeepAlive: o.KeepAlive, PortHost: o.PortHost,
	})
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	s.web = &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: errLog}
	s.local = &http.Server{Handler: srv.SocketHandler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: errLog}
	go s.serve(s.web, s.tcp)
	go s.serve(s.local, s.sock)
	log.Info("ESM started", "version", o.Version, "root", st.Path(), "listen", s.tcp.Addr().String(), "tls", useTLS,
		"socket", cfg.SocketPath(), "supervisor", s.sup.Kind(), "limits_enforced", s.sup.LimitsEnforced())
	if !s.sup.LimitsEnforced() {
		log.Warn("memory and CPU limits are not enforced in process mode")
	}
	return s, nil
}

// listenSocket listens on the unix socket, removing a stale socket file
// first. A socket that still answers means another ESM serves this root.
func listenSocket(path string) (net.Listener, error) {
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
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on the socket %s (set \"socket\" in esm.json to a shorter path if it is too long): %w", path, err)
	}
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

// Shutdown stops accepting requests, ends event streams, stops watching
// and scheduling, flushes the audit log and removes the socket. Game
// servers keep running.
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.once.Do(func() {
		s.bus.Close()
		err = errors.Join(s.web.Shutdown(ctx), s.local.Shutdown(ctx))
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
