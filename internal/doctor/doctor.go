// Package doctor runs read-only checks of an ESM box: the root folder, the
// config, the exposure rule, free ports, disk space, the supervisor, the
// socket and the secrets folder. It changes nothing.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
)

// Check results.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
)

// LowDiskBytes is the free space below which the disk check warns.
const LowDiskBytes = 10 << 30

// Check is the result of one check.
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Message string `json:"message"`
}

// Report is the result of a doctor run.
type Report struct {
	Checks   []Check `json:"checks"`
	Problems int     `json:"problems"` // checks that failed
	Warnings int     `json:"warnings"`
}

// Options configure a run.
type Options struct {
	Root string
	// PortHost is the address free ports are test-bound on (default
	// 0.0.0.0, all interfaces).
	PortHost string
	// UsedPorts are ports that instances own: counted as taken, not probed.
	UsedPorts map[int]bool
	// Supervisor and LimitsEnforced describe the running service's
	// supervisor. Empty means: work out what "auto" would pick.
	Supervisor     string
	LimitsEnforced bool
	// Service is true when the running service asks; the socket is then
	// known to be live and is not checked.
	Service bool
}

func (r *Report) add(name, status, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Message: fmt.Sprintf(format, args...)})
	switch status {
	case Fail:
		r.Problems++
	case Warn:
		r.Warnings++
	}
}

// Run performs the checks.
func Run(o Options) Report {
	r := Report{Checks: []Check{}}
	if err := config.CheckNotRoot(); err != nil {
		r.add("user", Fail, "%v", err)
	} else {
		r.add("user", OK, "not running as the superuser")
	}
	rootOK := checkRoot(&r, o.Root)
	cfg, found, err := config.Peek(o.Root)
	switch {
	case err != nil:
		r.add("config", Fail, "%v", err)
		cfg = config.Defaults(o.Root)
	case !found:
		r.add("config", OK, "%s not written yet; defaults apply", config.FileName)
	default:
		r.add("config", OK, "%s is valid", config.FileName)
	}
	if err := cfg.CheckListen(); err != nil {
		r.add("listen", Fail, "%v", err)
	} else if cfg.TLSCert != "" {
		r.add("listen", OK, "%s with TLS", cfg.Listen)
	} else {
		r.add("listen", OK, "%s (loopback only, no TLS needed)", cfg.Listen)
	}
	checkPorts(&r, "game_ports", cfg.GamePorts, o)
	checkPorts(&r, "query_ports", cfg.QueryPorts, o)
	checkDisk(&r, o.Root)
	checkSupervisor(&r, cfg, o)
	if !o.Service {
		checkSocket(&r, cfg.SocketPath())
	}
	if rootOK {
		checkSecrets(&r, o.Root)
	}
	return r
}

func checkRoot(r *Report, root string) bool {
	fi, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.add("root", Warn, "%s does not exist yet; esm serve creates it", root)
		return false
	case err != nil:
		r.add("root", Fail, "%s: %v", root, err)
		return false
	case !fi.IsDir():
		r.add("root", Fail, "%s is not a folder", root)
		return false
	}
	if ok, known := writable(root); known && !ok {
		r.add("root", Fail, "%s is not writable by this user", root)
		return true
	}
	r.add("root", OK, "%s exists", root)
	return true
}

func checkPorts(r *Report, name, spec string, o Options) {
	pr, err := config.ParsePortRange(spec)
	if err != nil {
		r.add(name, Fail, "%v", err)
		return
	}
	free, used := 0, 0
	for p := pr.Lo; p <= pr.Hi; p++ {
		if o.UsedPorts[p] {
			used++
			continue
		}
		if portFree(o.PortHost, p) {
			free++
		}
	}
	busy := pr.Len() - free - used
	status := OK
	if free == 0 {
		status = Warn
	}
	r.add(name, status, "%s: %d free, %d used by instances, %d busy with other programs", pr, free, used, busy)
}

func portFree(host string, port int) bool {
	if host == "" {
		host = "0.0.0.0"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return false
	}
	pc.Close()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func checkDisk(r *Report, root string) {
	dir := root
	for {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			r.add("disk", Warn, "cannot find an existing folder above %s", root)
			return
		}
		dir = parent
	}
	free, err := freeBytes(dir)
	if err != nil {
		r.add("disk", Warn, "cannot read free space: %v", err)
		return
	}
	gib := float64(free) / (1 << 30)
	if free < LowDiskBytes {
		r.add("disk", Warn, "%.1f GiB free on the disk holding %s (builds and backups need room)", gib, dir)
		return
	}
	r.add("disk", OK, "%.1f GiB free on the disk holding %s", gib, dir)
}

func checkSupervisor(r *Report, cfg *config.Config, o Options) {
	kind := o.Supervisor
	limits := o.LimitsEnforced
	if kind == "" {
		kind = cfg.Supervisor
		if kind == supervisor.KindAuto {
			kind = supervisor.KindProcess
			if supervisor.SystemdAvailable() {
				kind = supervisor.KindSystemd
			}
		}
		limits = kind == supervisor.KindSystemd
	}
	switch {
	case kind == supervisor.KindSystemd && !o.Service && !supervisor.SystemdAvailable():
		r.add("supervisor", Fail, "systemd is configured but `systemctl --user` does not work for this user")
		return
	case limits:
		r.add("supervisor", OK, "%s (memory and CPU limits enforced)", kind)
	default:
		r.add("supervisor", Warn, "%s (limits not enforced)", kind)
	}
	if kind == supervisor.KindSystemd {
		checkLinger(r)
	}
}

func checkLinger(r *Report) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger", "--value").Output()
	if err != nil {
		r.add("linger", Warn, "cannot ask loginctl about lingering: %v", err)
		return
	}
	if strings.TrimSpace(string(out)) == "yes" {
		r.add("linger", OK, "lingering is on: servers keep running after you log out")
		return
	}
	r.add("linger", Warn, "lingering is off: user units stop when you log out; the box owner runs `loginctl enable-linger <user>` once")
}

func checkSocket(r *Report, path string) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		r.add("socket", OK, "no service is running (no socket at %s)", path)
		return
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		r.add("socket", Warn, "stale socket file %s (no service answers); esm serve removes it", path)
		return
	}
	c.Close()
	r.add("socket", OK, "a service is running on %s", path)
}

func checkSecrets(r *Report, root string) {
	dir := filepath.Join(root, "secrets")
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		r.add("secrets", OK, "no secrets folder yet")
		return
	}
	if err != nil {
		r.add("secrets", Fail, "%v", err)
		return
	}
	if runtime.GOOS == "windows" {
		r.add("secrets", Warn, "ESM does not set Windows file permissions (ACLs); keep the root inside a folder only this account can read")
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		r.add("secrets", Warn, "%s has mode %o; it should be 0700", dir, fi.Mode().Perm())
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().Perm()&0o077 != 0 {
			r.add("secrets", Warn, "%s has mode %o; it should be 0600", filepath.Join(dir, e.Name()), info.Mode().Perm())
			return
		}
	}
	r.add("secrets", OK, "secrets folder and files are private to this user")
}
