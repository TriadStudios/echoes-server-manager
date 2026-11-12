// Package config loads and validates <root>/esm.json.
//
// A missing file is written with defaults on first run. Unknown fields are
// refused so typos in a hand-edited file are reported.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// FileName is the config file name inside the ESM root.
const FileName = "esm.json"

// Default values.
const (
	DefaultListen             = "127.0.0.1:8740"
	DefaultGamePorts          = "7777-7786"
	DefaultQueryPorts         = "27015-27024"
	DefaultSupervisor         = "auto"
	DefaultStopTimeout        = 30 * time.Second
	DefaultReadyTimeout       = 120 * time.Second
	DefaultKeepPreviousBuilds = 2
	DefaultLogLevel           = "info"
	DefaultBackoffBase        = 5 * time.Second
	DefaultBackoffMax         = 5 * time.Minute
	DefaultBackoffWindow      = 10 * time.Minute
	DefaultBackoffMaxCrashes  = 5
	// DefaultBackupSaveWait is how long a backup of a running server waits
	// for the next world save before copying the save files anyway.
	DefaultBackupSaveWait = 2 * time.Minute
	// DefaultRootName is the folder under the user's home used when neither
	// --root nor ESM_ROOT is given. A service install would pass
	// --root /opt/durinn instead.
	DefaultRootName = "durinn"
	// RootEnv names the environment variable that sets the root.
	RootEnv = "ESM_ROOT"
)

// Duration is a time.Duration written as a Go duration string ("30s").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes the duration as a string such as "1m30s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string ("30s") or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q", s)
		}
		*d = Duration(v)
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("invalid duration %s", b)
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

// Backoff controls restarts after crashes: the wait before restart n is
// Base*2^n capped at Max; MaxCrashes crashes within Window means give up.
type Backoff struct {
	Base       Duration `json:"base"`
	Max        Duration `json:"max"`
	Window     Duration `json:"window"`
	MaxCrashes int      `json:"max_crashes"`
}

// Config is the content of esm.json.
type Config struct {
	Listen  string `json:"listen"`
	Socket  string `json:"socket,omitempty"` // empty means <root>/esm.sock
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// BehindTLSProxy says that browsers reach ESM only through a reverse
	// proxy that speaks TLS to them (ESM itself listens on loopback without
	// TLS). The session cookie is then marked Secure, as with tls_cert and
	// tls_key. Off by default: a Secure cookie is never sent over plain
	// http, so turning this on without such a proxy breaks browser logins.
	BehindTLSProxy bool `json:"behind_tls_proxy"`
	// BuildImportDir is the folder a local build source (a folder or an
	// archive) must be in. Empty means <root>/imports; a relative path is
	// taken from the root.
	BuildImportDir     string   `json:"build_import_dir,omitempty"`
	GamePorts          string   `json:"game_ports"`
	QueryPorts         string   `json:"query_ports"`
	Supervisor         string   `json:"supervisor"` // auto | systemd | process
	Backoff            Backoff  `json:"backoff"`
	StopTimeout        Duration `json:"stop_timeout"`
	ReadyTimeout       Duration `json:"ready_timeout"`
	KeepPreviousBuilds int      `json:"keep_previous_builds"`
	BackupSaveWait     Duration `json:"backup_save_wait"`
	LogLevel           string   `json:"log_level"`

	// Root is the ESM root this config was loaded from. Not stored.
	Root string `json:"-"`
}

// Defaults returns the default configuration for root.
func Defaults(root string) *Config {
	return &Config{
		Listen:     DefaultListen,
		GamePorts:  DefaultGamePorts,
		QueryPorts: DefaultQueryPorts,
		Supervisor: DefaultSupervisor,
		Backoff: Backoff{
			Base:       Duration(DefaultBackoffBase),
			Max:        Duration(DefaultBackoffMax),
			Window:     Duration(DefaultBackoffWindow),
			MaxCrashes: DefaultBackoffMaxCrashes,
		},
		StopTimeout:        Duration(DefaultStopTimeout),
		ReadyTimeout:       Duration(DefaultReadyTimeout),
		KeepPreviousBuilds: DefaultKeepPreviousBuilds,
		BackupSaveWait:     Duration(DefaultBackupSaveWait),
		LogLevel:           DefaultLogLevel,
		Root:               root,
	}
}

// Load reads esm.json from the store root. Fields missing from the file keep
// their defaults. When the file does not exist the defaults are written and
// created is true.
func Load(st *store.Store) (cfg *Config, created bool, err error) {
	cfg = Defaults(st.Path())
	err = st.ReadJSON(FileName, cfg)
	if errors.Is(err, fs.ErrNotExist) {
		if err := cfg.Validate(); err != nil {
			return nil, false, err
		}
		if err := Save(st, cfg); err != nil {
			return nil, false, err
		}
		return cfg, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("config: %w", err)
	}
	cfg.Root = st.Path()
	if err := cfg.Validate(); err != nil {
		return nil, false, err
	}
	return cfg, false, nil
}

// Peek reads <root>/esm.json without creating or writing anything (for
// read-only callers such as doctor and the command line). A missing file
// gives the defaults and found=false.
func Peek(root string) (cfg *Config, found bool, err error) {
	cfg = Defaults(root)
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, true, fmt.Errorf("config: decode %s: %w", FileName, err)
	}
	cfg.Root = root
	if err := cfg.Validate(); err != nil {
		return nil, true, err
	}
	return cfg, true, nil
}

// Save writes cfg to esm.json.
func Save(st *store.Store, cfg *Config) error {
	return st.WriteJSON(FileName, cfg, 0o600)
}

// SocketPath returns the unix socket path, defaulting to <root>/esm.sock.
func (c *Config) SocketPath() string {
	if c.Socket != "" {
		return c.Socket
	}
	return filepath.Join(c.Root, "esm.sock")
}

// DefaultImportDir is the import folder inside the root.
const DefaultImportDir = "imports"

// ImportDir returns the absolute import folder for local build sources
// (see BuildImportDir).
func (c *Config) ImportDir() string {
	dir := c.BuildImportDir
	if dir == "" {
		dir = DefaultImportDir
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(c.Root, dir)
	}
	return filepath.Clean(dir)
}

// Validate checks every field and returns the first problem found.
func (c *Config) Validate() error {
	if _, _, err := SplitListen(c.Listen); err != nil {
		return fmt.Errorf("config: listen: %w", err)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("config: tls_cert and tls_key must be set together")
	}
	g, err := ParsePortRange(c.GamePorts)
	if err != nil {
		return fmt.Errorf("config: game_ports: %w", err)
	}
	q, err := ParsePortRange(c.QueryPorts)
	if err != nil {
		return fmt.Errorf("config: query_ports: %w", err)
	}
	if g.Overlaps(q) {
		return errors.New("config: game_ports and query_ports overlap")
	}
	switch c.Supervisor {
	case "auto", "process":
	case "systemd":
		if runtime.GOOS != "linux" {
			return errors.New("config: supervisor \"systemd\" is only available on Linux")
		}
	default:
		return fmt.Errorf("config: supervisor must be auto, systemd or process, not %q", c.Supervisor)
	}
	b := c.Backoff
	if b.Base <= 0 || b.Max < b.Base || b.Window <= 0 || b.MaxCrashes < 1 {
		return errors.New("config: backoff needs base > 0, max >= base, window > 0, max_crashes >= 1")
	}
	if c.StopTimeout <= 0 {
		return errors.New("config: stop_timeout must be positive")
	}
	if c.ReadyTimeout <= 0 {
		return errors.New("config: ready_timeout must be positive")
	}
	if c.KeepPreviousBuilds < 0 {
		return errors.New("config: keep_previous_builds must not be negative")
	}
	if c.BackupSaveWait < 0 {
		return errors.New("config: backup_save_wait must not be negative")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log_level must be debug, info, warn or error, not %q", c.LogLevel)
	}
	return nil
}

// CheckListen enforces the exposure rule: a listen address that is not
// loopback needs both tls_cert and tls_key.
func (c *Config) CheckListen() error {
	return CheckListen(c.Listen, c.TLSCert, c.TLSKey)
}

// CheckListen is the exposure rule for an arbitrary address.
func CheckListen(listen, cert, key string) error {
	host, _, err := SplitListen(listen)
	if err != nil {
		return err
	}
	if IsLoopback(host) {
		return nil
	}
	if cert == "" || key == "" {
		return fmt.Errorf("refusing to listen on %s without TLS: set tls_cert and tls_key, or listen on 127.0.0.1 and put a reverse proxy in front", listen)
	}
	return nil
}

// SplitListen splits and checks a host:port listen address.
func SplitListen(listen string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", 0, fmt.Errorf("invalid address %q: %w", listen, err)
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("invalid port in %q", listen)
	}
	return h, n, nil
}

// IsLoopback reports whether host is "localhost" or a loopback IP
// (127.0.0.0/8 or ::1). An empty host means every interface: not loopback.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// PortRange is an inclusive range of ports.
type PortRange struct{ Lo, Hi int }

// ParsePortRange parses "7777-7786" or a single port "7777".
func ParsePortRange(s string) (PortRange, error) {
	lo, hi, found := strings.Cut(strings.TrimSpace(s), "-")
	if !found {
		hi = lo
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil {
		return PortRange{}, fmt.Errorf("invalid port range %q", s)
	}
	if a < 1 || b > 65535 || a > b {
		return PortRange{}, fmt.Errorf("port range %q must be within 1-65535 and low-high", s)
	}
	return PortRange{a, b}, nil
}

// Len returns the number of ports in the range.
func (r PortRange) Len() int { return r.Hi - r.Lo + 1 }

// Contains reports whether p is in the range.
func (r PortRange) Contains(p int) bool { return p >= r.Lo && p <= r.Hi }

// Overlaps reports whether the two ranges share a port.
func (r PortRange) Overlaps(o PortRange) bool { return r.Lo <= o.Hi && o.Lo <= r.Hi }

func (r PortRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// ResolveRoot picks the ESM root: the flag value, else $ESM_ROOT, else
// ~/durinn. The result is absolute.
func ResolveRoot(flagValue string) (string, error) {
	root := flagValue
	if root == "" {
		root = os.Getenv(RootEnv)
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot find the home directory; pass --root: %w", err)
		}
		root = filepath.Join(home, DefaultRootName)
	}
	return filepath.Abs(root)
}

// CheckNotRoot refuses to run as the Linux superuser.
func CheckNotRoot() error {
	return checkUID(os.Geteuid())
}

func checkUID(uid int) error {
	if runtime.GOOS == "linux" && uid == 0 {
		return errors.New("esm refuses to run as root: create an unprivileged user for it (for a service install, own /opt/durinn by that user and pass --root /opt/durinn)")
	}
	return nil
}
