// Package gameserver is the real-server adapter.
//
// Everything ESM assumes about the Echoes of Durinn dedicated server is in
// this package. Values marked UNCONFIRMED are not yet proven on a real host.
//
// The fake server (cmd/esm-fakeserver) follows exactly these assumptions, so
// tests that run the fake exercise this code. When a real host proves a value
// wrong, it is changed here and in the fake, and nowhere else.
package gameserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultMap is used when an instance does not name a map.
// UNCONFIRMED: placeholder; the real default map name is not known yet.
const DefaultMap = "Default"

// DataDirSwitch is the command-line switch that points the server at its
// data (saved) folder. UNCONFIRMED.
const DataDirSwitch = "-SavedDir"

// PasswordsOnCommandLine says whether the server and admin passwords are
// passed as command-line switches. While this is true they are visible to
// other local users through the process list, so ESM warns about it.
// UNCONFIRMED: yes.
const PasswordsOnCommandLine = true

// Mask replaces secret values in logged command lines.
const Mask = "********"

// Launch is everything needed to build a server command line.
type Launch struct {
	Map            string // empty means DefaultMap
	GamePort       int
	QueryPort      int
	BindIP         string // empty means all interfaces (no -MULTIHOME)
	MaxPlayers     int
	ServerName     string
	WorldName      string
	Rules          map[string]string // each becomes -Key=Value, sorted by key
	DataDir        string            // absolute path of the instance data folder
	ServerPassword string            // optional
	AdminPassword  string            // optional
	ExtraArgs      []string          // appended as-is, one argv entry each
}

// Args builds the argument list (without the executable). It never builds a
// shell string. masked is the same list with password values replaced, for
// logs. Order: map, -Port, -QueryPort, -MULTIHOME (when BindIP is set),
// -MaxPlayers, -ServerName, -WorldName, rules sorted by key, -ServerPassword
// and -AdminPassword (when set), -log, the data folder switch, extra args.
// UNCONFIRMED: every switch name and the positional map argument.
func (l Launch) Args() (argv, masked []string) {
	m := l.Map
	if m == "" {
		m = DefaultMap
	}
	argv = append(argv,
		m,
		"-Port="+strconv.Itoa(l.GamePort),
		"-QueryPort="+strconv.Itoa(l.QueryPort),
	)
	if l.BindIP != "" {
		argv = append(argv, "-MULTIHOME="+l.BindIP)
	}
	argv = append(argv,
		"-MaxPlayers="+strconv.Itoa(l.MaxPlayers),
		"-ServerName="+l.ServerName,
		"-WorldName="+l.WorldName,
	)
	keys := make([]string, 0, len(l.Rules))
	for k := range l.Rules {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		argv = append(argv, "-"+k+"="+l.Rules[k])
	}
	if l.ServerPassword != "" {
		argv = append(argv, "-ServerPassword="+l.ServerPassword)
	}
	if l.AdminPassword != "" {
		argv = append(argv, "-AdminPassword="+l.AdminPassword)
	}
	argv = append(argv, "-log", DataDirSwitch+"="+l.DataDir)
	argv = append(argv, l.ExtraArgs...)
	return argv, MaskArgs(argv)
}

// MaskArgs returns a copy of argv with the value of every -Key=Value whose
// key contains "password" (any case) replaced by Mask.
func MaskArgs(argv []string) []string {
	out := slices.Clone(argv)
	for i, a := range out {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		k, _, ok := strings.Cut(a, "=")
		if ok && strings.Contains(strings.ToLower(k), "password") {
			out[i] = k + "=" + Mask
		}
	}
	return out
}

// reserved are the switches ESM sets itself; rules and extra args must not
// repeat them.
var reserved = []string{"port", "queryport", "multihome", "maxplayers", "servername",
	"worldname", "serverpassword", "adminpassword", "log", strings.ToLower(strings.TrimPrefix(DataDirSwitch, "-"))}

// IsReservedSwitch reports whether key (without the leading "-") names a
// switch ESM sets itself.
func IsReservedSwitch(key string) bool {
	return slices.Contains(reserved, strings.ToLower(strings.TrimLeft(key, "-")))
}

// ReadyLine matches the server log line that says the server accepts
// players. UNCONFIRMED.
var ReadyLine = regexp.MustCompile(`\bServer is ready\b`)

// WorldSavedLine matches the server log line written after each world save.
// UNCONFIRMED.
var WorldSavedLine = regexp.MustCompile(`\bWorld saved\b`)

// ServerLogRel is the server log path inside the data folder. UNCONFIRMED.
const ServerLogRel = "Logs/server.log"

// ServerLogPath returns the server log path for a data folder.
func ServerLogPath(dataDir string) string {
	return filepath.Join(dataDir, filepath.FromSlash(ServerLogRel))
}

// StatusFileRel is the status file inside the data folder.
// UNCONFIRMED: the real server may not write a status file at all; ESM then
// falls back to the ready log line and the query port.
const StatusFileRel = "esm-status.json"

// StatusFilePath returns the status file path for a data folder.
func StatusFilePath(dataDir string) string {
	return filepath.Join(dataDir, filepath.FromSlash(StatusFileRel))
}

// Server states written to the status file.
const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopping = "stopping"
)

// Ports is the port pair in the status file.
type Ports struct {
	Game  int `json:"game"`
	Query int `json:"query"`
}

// Status is the status file schema. UNCONFIRMED.
type Status struct {
	State    string    `json:"state"`
	PID      int       `json:"pid"`
	Ports    Ports     `json:"ports"`
	Players  int       `json:"players"`
	Version  string    `json:"version,omitempty"`
	LastSave time.Time `json:"last_save,omitzero"`
}

// ReadStatus reads the status file of a data folder.
func ReadStatus(dataDir string) (Status, error) {
	var s Status
	b, err := os.ReadFile(StatusFilePath(dataDir))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("gameserver: bad status file: %w", err)
	}
	return s, nil
}

// SaveGlob matches the world save files inside the data folder; backups copy
// exactly these. UNCONFIRMED.
const SaveGlob = "SaveGames/*.sav"

// SaveFileRel is where the save of world lives inside the data folder.
func SaveFileRel(world string) string { return "SaveGames/" + world + ".sav" }

// SaveFiles lists the save files of a data folder as slash paths relative
// to it, sorted.
func SaveFiles(dataDir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dataDir, filepath.FromSlash(SaveGlob)))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		fi, err := os.Lstat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		rel, err := filepath.Rel(dataDir, m)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	slices.Sort(out)
	return out, nil
}

// ControlDirRel is the folder inside the data folder that the server polls
// for request files. UNCONFIRMED.
const ControlDirRel = "control"

// ControlStop is the request file that asks for a polite stop.
const ControlStop = "stop"

// StopMethod is how a polite stop is asked for.
type StopMethod int

// Polite stop methods.
const (
	// StopSignal sends SIGTERM.
	StopSignal StopMethod = iota
	// StopFile writes <data>/control/stop.
	StopFile
)

func (m StopMethod) String() string {
	if m == StopFile {
		return "stop request file"
	}
	return "SIGTERM"
}

// PoliteStop returns the polite stop method for an OS: SIGTERM on Linux,
// the stop request file on Windows (UNCONFIRMED for the real server).
func PoliteStop(goos string) StopMethod {
	if goos == "windows" {
		return StopFile
	}
	return StopSignal
}

// StopRequestPath returns the stop request file of a data folder.
func StopRequestPath(dataDir string) string {
	return filepath.Join(dataDir, ControlDirRel, ControlStop)
}

// RequestStop writes the stop request file.
func RequestStop(dataDir string) error {
	p := StopRequestPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("stop\n"), 0o600)
}

// Probe asks the query port whether the server answers: it sends "PING" over
// UDP and expects "PONG". UNCONFIRMED: the real query protocol is unknown.
func Probe(ctx context.Context, host string, port int) error {
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Second)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := c.Write([]byte("PING")); err != nil {
		return err
	}
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != "PONG" {
		return errors.New("gameserver: unexpected query answer")
	}
	return nil
}
