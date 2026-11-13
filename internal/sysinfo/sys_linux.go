package sysinfo

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
)

// procRoot and cgroupRoot are where the kernel's files are (changed by
// tests).
var (
	procRoot   = "/proc"
	cgroupRoot = "/sys/fs/cgroup"
)

func memory() (total, avail int64, err error) {
	f, err := os.Open(procRoot + "/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	total, avail = -1, -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		n, err := kibibytes(rest)
		if err != nil {
			return 0, 0, fmt.Errorf("meminfo %s: %w", key, err)
		}
		if key == "MemTotal" {
			total = n
		} else {
			avail = n
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if total < 0 || avail < 0 {
		return 0, 0, errors.New("meminfo has no MemTotal or MemAvailable")
	}
	return total, avail, nil
}

// kibibytes parses " 16314256 kB" into bytes.
func kibibytes(s string) (int64, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, errors.New("no value")
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad value %q", f[0])
	}
	if len(f) > 1 && f[1] != "kB" {
		return 0, fmt.Errorf("unexpected unit %q", f[1])
	}
	return n << 10, nil
}

func load1() (float64, error) {
	b, err := os.ReadFile(procRoot + "/loadavg")
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("loadavg is empty")
	}
	return strconv.ParseFloat(f[0], 64)
}

// DiskSpace returns the size of the file system that holds dir and the
// bytes this user may still write there.
func DiskSpace(dir string) (total, free uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, 0, err
	}
	return s.Blocks * uint64(s.Bsize), s.Bavail * uint64(s.Bsize), nil
}

func processMemory(pid int, unit string) (int64, string, error) {
	if pid <= 0 {
		return 0, "", errors.New("no process id")
	}
	if unit != "" {
		if n, ok := cgroupMemory(pid, unit); ok {
			return n, SourceCgroup, nil
		}
	}
	f, err := os.Open(fmt.Sprintf("%s/%d/status", procRoot, pid))
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			n, err := kibibytes(rest)
			if err != nil {
				return 0, "", fmt.Errorf("VmRSS: %w", err)
			}
			return n, SourceProcess, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, "", err
	}
	return 0, "", errors.New("the process reports no resident memory")
}

// cgroupMemory reads memory.current of the cgroup v2 the process is in,
// when that cgroup is the unit's own.
func cgroupMemory(pid int, unit string) (int64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/cgroup", procRoot, pid))
	if err != nil {
		return 0, false
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		// cgroup v2: "0::/user.slice/.../esm-alpha.service"
		p, ok := bytes.CutPrefix(line, []byte("0::"))
		if !ok {
			continue
		}
		if bytes.Contains(p, []byte("..")) {
			return 0, false
		}
		cg := path.Clean("/" + string(p))
		if path.Base(cg) != unit {
			return 0, false
		}
		v, err := os.ReadFile(cgroupRoot + cg + "/memory.current")
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
