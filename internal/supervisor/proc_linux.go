package supervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	processSupported = true
	// signalsAvailable: the polite stop is SIGTERM.
	signalsAvailable = true
)

// detachedAttr puts the child in its own session so it survives ESM and
// never gets the terminal's signals.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

// procStat returns the state letter and start time (clock ticks after boot,
// field 22 of /proc/<pid>/stat) of pid.
func procStat(pid int) (state, start string, err error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the command name may contain spaces and ')'
	if i < 0 {
		return "", "", errors.New("malformed /proc stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", "", errors.New("short /proc stat")
	}
	return f[0], f[19], nil
}

// startMarker returns the start marker of pid (it may already be a zombie).
func startMarker(pid int) (string, error) {
	_, start, err := procStat(pid)
	if err != nil {
		return "", err
	}
	return "linux:" + start, nil
}

// inspect reports whether pid is alive (not a zombie) and its start marker.
func inspect(pid int) (alive bool, marker string, err error) {
	state, start, err := procStat(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if state == "Z" || state == "X" || state == "x" {
		return false, "", nil
	}
	return true, "linux:" + start, nil
}

// sameExecutable reports whether pid runs the file at path.
func sameExecutable(pid int, path string) bool {
	link := fmt.Sprintf("/proc/%d/exe", pid)
	a, errA := os.Stat(link) // follows the link even when the file was replaced
	b, errB := os.Stat(path)
	if errA == nil && errB == nil {
		return os.SameFile(a, b)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return false
	}
	return filepath.Clean(strings.TrimSuffix(target, " (deleted)")) == filepath.Clean(path)
}
