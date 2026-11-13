// Package gameserver is the real-server adapter.
//
// Everything ESM assumes about the Echoes of Durinn dedicated server is in
// this package. Values marked UNCONFIRMED are not yet proven on a real host.
// Values marked CONFIRMED were seen on a real Linux host running one
// server.
//
// The fake server (cmd/esm-fakeserver) follows exactly these assumptions, so
// tests that run the fake exercise this code. When a real host proves a value
// wrong, it is changed here and in the fake, and nowhere else.
//
// Nothing game-specific is written into ESM: the path of the server binary
// inside a build and the build's default map come from the build manifest,
// and the map of an instance from its settings.
package gameserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mask stands in for a secret value in text that ESM shows.
const Mask = "********"

// The switches ESM puts on every server command line. CONFIRMED: a real
// server started with exactly these (and no others) ran, was listed and
// was joined.
const (
	UnattendedSwitch    = "-unattended"
	PortSwitch          = "-Port"
	QueryPortSwitch     = "-QueryPort"
	ServerNameSwitch    = "-ServerName"
	WorldNameSwitch     = "-WorldName"
	MaxPlayersSwitch    = "-MaxPlayers"
	ListInBrowserSwitch = "-ListInBrowser"
	// UserDirSwitch points the server at its own folder: it keeps its saves
	// (and its other written files) below it, so a new build never touches
	// them. CONFIRMED.
	UserDirSwitch = "-UserDir"
	// LogFileSwitch names the file the server writes its log to (an
	// absolute path). CONFIRMED.
	LogFileSwitch = "-abslog"
)

// MultihomeSwitch is the engine's bind-address switch. ESM never passes it
// and refuses it as a rule or an extra argument. CONFIRMED on a real host:
// with it the server advertises the bind address where its Steam identity
// belongs, and players who pick it from the server list cannot join.
// Without it the query socket listens on every address of the box; the
// box's firewall decides where the query port is reachable from.
const MultihomeSwitch = "-MULTIHOME"

// Launch is everything needed to build a server command line.
type Launch struct {
	// Map is the map the server loads: the instance's map, or the build's
	// default map. The command line needs one; ESM refuses to start an
	// instance without either.
	Map        string
	GamePort   int
	QueryPort  int
	MaxPlayers int
	ServerName string
	WorldName  string
	// Listed says whether the server shows in the in-game server list.
	Listed bool
	// UserDir is the absolute path of the instance's data folder.
	UserDir string
	// LogFile is the absolute path of the server log file.
	LogFile string
	// Rules are game settings; each becomes -Key=Value, sorted by key.
	// CONFIRMED: every key of the game's server settings works as a switch.
	// UNCONFIRMED: whether a loose settings file next to the build would be
	// read instead; ESM does not rely on one.
	Rules map[string]string
	// Delivery is how the passwords reach the server: DeliveryFile or
	// DeliveryCommandLine. Empty means SecretDelivery.
	Delivery string
	// SecretsFile is the absolute path of the secrets file. With
	// DeliveryFile the server is pointed at it and gets no password on its
	// command line.
	SecretsFile string
	// Secrets are the passwords. They are put on the command line with
	// DeliveryCommandLine, and never otherwise.
	Secrets   Secrets
	ExtraArgs []string // appended as-is, one argv entry each
}

// Args builds the argument list (without the executable). It never builds a
// shell string. Order: the map, -unattended, -Port, -QueryPort,
// -ServerName, -WorldName, -MaxPlayers, -ListInBrowser, -UserDir, -abslog,
// the rules sorted by key, the passwords (the secrets file switch; or, with
// DeliveryCommandLine, -ServerPassword and -AdminPassword when set), then
// the extra args. -MULTIHOME is never part of it: a bind address configured
// for an instance is not given to the server.
func (l Launch) Args() []string {
	argv := []string{
		l.Map,
		UnattendedSwitch,
		PortSwitch + "=" + strconv.Itoa(l.GamePort),
		QueryPortSwitch + "=" + strconv.Itoa(l.QueryPort),
		ServerNameSwitch + "=" + l.ServerName,
		WorldNameSwitch + "=" + l.WorldName,
		MaxPlayersSwitch + "=" + strconv.Itoa(l.MaxPlayers),
		ListInBrowserSwitch + "=" + boolWord(l.Listed),
		UserDirSwitch + "=" + l.UserDir,
		LogFileSwitch + "=" + l.LogFile,
	}
	keys := make([]string, 0, len(l.Rules))
	for k := range l.Rules {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		argv = append(argv, "-"+k+"="+l.Rules[k])
	}
	if l.Delivery == DeliveryCommandLine {
		if l.Secrets.ServerPassword != "" {
			argv = append(argv, ServerPasswordSwitch+"="+l.Secrets.ServerPassword)
		}
		if l.Secrets.AdminPassword != "" {
			argv = append(argv, AdminPasswordSwitch+"="+l.Secrets.AdminPassword)
		}
	} else if l.SecretsFile != "" {
		argv = append(argv, SecretsFileSwitch+"="+l.SecretsFile)
	}
	return append(argv, l.ExtraArgs...)
}

// boolWord writes a switch value the way the engine's own switches are
// written: True or False.
func boolWord(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// reserved are the switches ESM sets itself; rules and extra args must not
// repeat them.
var reserved = []string{
	switchName(UnattendedSwitch), switchName(PortSwitch), switchName(QueryPortSwitch),
	switchName(ServerNameSwitch), switchName(WorldNameSwitch), switchName(MaxPlayersSwitch),
	switchName(ListInBrowserSwitch), switchName(UserDirSwitch), switchName(LogFileSwitch),
	switchName(ServerPasswordSwitch), switchName(AdminPasswordSwitch), switchName(SecretsFileSwitch),
}

// switchName is a switch name as it is compared: lower case, no dashes in
// front.
func switchName(name string) string { return strings.ToLower(strings.TrimLeft(name, "-")) }

// IsReservedSwitch reports whether key (with or without the leading "-")
// names a switch ESM sets itself.
func IsReservedSwitch(key string) bool {
	return slices.Contains(reserved, switchName(key))
}

// RefusedSwitch reports whether key (with or without the leading "-") names
// a switch that must never reach the server, and says why.
func RefusedSwitch(key string) (why string, refused bool) {
	if switchName(key) == switchName(MultihomeSwitch) {
		return MultihomeSwitch + " is never passed: with it the server advertises the bind address instead of its Steam identity and players cannot join; the box's firewall decides where the query port is reachable", true
	}
	return "", false
}

// The engine puts this in front of every message in its log file (the
// -abslog file ESM reads): the time and frame number
// ("[2026.01.02-03.04.05:678][  0]"), then exactly one log category, then
// optionally one verbosity word, each followed by ": "
// ("LogExample: Display: "). All of it is required but the verbosity, so
// nothing a player types can stand where the words of a line ESM reads
// start: a chat line has the player's name, or a tag, between the
// category and the text. UNCONFIRMED: the categories the real lines carry;
// any name of letters, digits and "_" is accepted.
const logPrefix = `^\[[0-9]{4}\.[0-9]{2}\.[0-9]{2}-[0-9]{2}\.[0-9]{2}\.[0-9]{2}:[0-9]{3}\]\[ *[0-9]+\]` +
	`[A-Za-z][A-Za-z0-9_]*: (?:(?:Fatal|Error|Warning|Display|Log|Verbose|VeryVerbose): )?`

// The shapes of the server log lines ESM reads. Only the fixed words are
// matched; names and numbers in them are the server's own. Each must start
// its message: the same words later in a line (a repeated command line, a
// server name, a chat message) never count.
var (
	// readyLine: the server registered its dedicated session, so it is in
	// the server list and accepts players. CONFIRMED (every real run).
	// Chosen over the Steam logon line, which comes a moment earlier: a
	// server that is logged on but has no session yet cannot be joined.
	readyLine = regexp.MustCompile(logPrefix + `\[SESSION\] Dedicated session '.*' registered \(map `)
	// worldSavedLine: the server wrote its world save (at start, at every
	// autosave and at a stop), or confirmed at a stop, tagged [SHUTDOWN]
	// or [EXIT], that its last write went through. CONFIRMED for the
	// words; UNCONFIRMED whether "Wrote save" starts its message.
	worldSavedLine = regexp.MustCompile(logPrefix + `(?:Wrote save |\[(?:SHUTDOWN|EXIT)\] Synchronous write after the flush succeeded)`)
	// serverIDLine: the server logged on to Steam anonymously and names
	// the Steam id it got. CONFIRMED: the id is new at every start.
	// UNCONFIRMED: whether the words start the message (a subsystem tag in
	// front of them would hide the id; it is only shown, never relied on).
	serverIDLine = regexp.MustCompile(logPrefix + `Steam game server logged on with UID: ([0-9]{1,20})(?:[^0-9A-Za-z_]|$)`)
	// resumedLine: the server found a save of its world and carries on
	// with it (instead of a new world). CONFIRMED for the words.
	resumedLine = regexp.MustCompile(logPrefix + `\[SERVER\] Resuming world `)
)

// IsReadyLine reports whether line, one whole line of the server log, is
// the server saying it accepts players.
func IsReadyLine(line string) bool { return readyLine.MatchString(line) }

// IsWorldSavedLine reports whether line, one whole line of the server log,
// is the server saying it saved the world.
func IsWorldSavedLine(line string) bool { return worldSavedLine.MatchString(line) }

// IsWorldResumedLine reports whether line, one whole line of the server
// log, is the server saying it carries on with a saved world.
func IsWorldResumedLine(line string) bool { return resumedLine.MatchString(line) }

// ServerID returns the Steam server id that line, one whole line of the
// server log, names, if it is the server's logon line. The id changes every
// time the server starts, so ESM shows it for the running server only and
// never keeps it.
func ServerID(line string) (id string, ok bool) {
	m := serverIDLine.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Lines of the shapes above, as the fake server writes them. Tests and the
// fake use these; ESM itself only matches lines.

// ReadyLine is a ready line for a session named name on map m with slots
// player slots.
func ReadyLine(name, m string, slots int) string {
	return fmt.Sprintf("[SESSION] Dedicated session '%s' registered (map %s, %d slots)", name, m, slots)
}

// WorldSavedLine is a save-written line for a save file of size bytes.
func WorldSavedLine(file string, size int) string {
	return fmt.Sprintf("Wrote save %s (%d bytes)", file, size)
}

// FlushedLine is the line that confirms the last write at a stop.
const FlushedLine = "[SHUTDOWN] Synchronous write after the flush succeeded."

// ServerIDLine is the logon line naming the Steam id id.
func ServerIDLine(id string) string { return "Steam game server logged on with UID: " + id }

// ResumedLine is the line of a server that carries on with a saved world.
func ResumedLine(world string) string { return "[SERVER] Resuming world '" + world + "'" }

// NewWorldLine is the line of a server that starts a new world.
func NewWorldLine(world string) string { return "[SERVER] New world '" + world + "'" }

// ServerLogRel is where ESM points the server log (LogFileSwitch), as a
// slash path inside the instance folder. The log is outside the data
// folder, so a backup never copies it.
const ServerLogRel = "logs/server.log"

// ServerLogPath returns the server log path of an instance folder.
func ServerLogPath(instanceDir string) string {
	return filepath.Join(instanceDir, filepath.FromSlash(ServerLogRel))
}

// DataDirRel is the instance's data folder (UserDirSwitch) as a slash path
// inside the instance folder.
const DataDirRel = "data"

// DataDirPath returns the data folder of an instance folder.
func DataDirPath(instanceDir string) string {
	return filepath.Join(instanceDir, DataDirRel)
}

// StatusFileRel is the status file inside the data folder.
// UNCONFIRMED: the real server writes no status file today; ESM then
// relies on the ready log line and the query port.
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
	// ServerPasswordSet and AdminPasswordSet say which passwords the server
	// took, never what they are.
	ServerPasswordSet bool `json:"server_password_set"`
	AdminPasswordSet  bool `json:"admin_password_set"`
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

// SaveDirRel is the folder, as a slash path inside the data folder, that
// holds the world saves. Every file below it belongs to the saves (the
// game keeps each world's save, its metadata and older copies there), so
// backups copy the whole folder and restores put back the whole folder.
// CONFIRMED: a real server honours UserDirSwitch and saves below this
// folder; a new build never touches it.
const SaveDirRel = "Saved/SaveGames"

// IsSaveFile reports whether rel, a slash path inside the data folder, is
// a clean path of a file below SaveDirRel.
func IsSaveFile(rel string) bool {
	return strings.HasPrefix(rel, SaveDirRel+"/") && path.Clean(rel) == rel && !strings.ContainsAny(rel, `\:`) &&
		filepath.IsLocal(filepath.FromSlash(rel))
}

// SaveFiles lists the save files of a data folder (every regular file
// below SaveDirRel; links are skipped) as slash paths relative to the data
// folder, sorted.
func SaveFiles(dataDir string) ([]string, error) {
	top := filepath.Join(dataDir, filepath.FromSlash(SaveDirRel))
	out := []string{}
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == top && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// ControlDirRel is the folder inside the data folder that the fake server
// polls for request files; the stop request file is how ESM stops a server
// on Windows. UNCONFIRMED for a real server on Windows.
const ControlDirRel = "control"

// ControlStop is the request file that asks for a polite stop.
const ControlStop = "stop"

// StopMethod is how a polite stop is asked for.
type StopMethod int

// Polite stop methods.
const (
	// StopInterrupt sends exactly one SIGINT.
	StopInterrupt StopMethod = iota
	// StopFile writes <data>/control/stop.
	StopFile
)

func (m StopMethod) String() string {
	if m == StopFile {
		return "stop request file"
	}
	return "SIGINT"
}

// PoliteStop returns the polite stop method for an OS: one SIGINT on Linux,
// the stop request file on Windows.
//
// CONFIRMED on Linux: one SIGINT makes the server save and exit cleanly
// within a few seconds (StopExitCode). A second signal while it shuts down
// may end it before the save is written, and a kill certainly does, so ESM
// sends one signal and waits; it never sends a second one and never kills
// a server on Linux. UNCONFIRMED: what SIGTERM does (never sent by ESM).
func PoliteStop(goos string) StopMethod {
	if goos == "windows" {
		return StopFile
	}
	return StopInterrupt
}

// StopExitCode is the exit code of a server that stopped cleanly after
// SIGINT. CONFIRMED.
const StopExitCode = 130

// SteamAppIDFile is the file that names the game's Steam app id. CONFIRMED:
// a real (non-Shipping) server writes it next to its binary when it starts,
// so it needs to write into the build. ESM places it there when it installs
// a build (from the configured app id), before the build is made read-only.
// UNCONFIRMED: whether the server accepts a file that is already there and
// read-only, instead of writing its own.
const SteamAppIDFile = "steam_appid.txt"

// SteamLibraries are the Steam client libraries the server loads at start.
// CONFIRMED: they are not part of a server build; the 64-bit copies from
// the Steamworks SDK redistributable sit in one folder, which the server
// finds through LD_LIBRARY_PATH.
var SteamLibraries = []string{"steamclient.so", "libsteamwebrtc.so"}

// LibraryPathVar is the variable the server finds SteamLibraries through.
const LibraryPathVar = "LD_LIBRARY_PATH"

// LibraryPath returns the LD_LIBRARY_PATH value for a server: the Steam
// runtime folder first, then what was set before (if anything).
func LibraryPath(runtimeDir, inherited string) string {
	if inherited == "" {
		return runtimeDir
	}
	return strings.Join([]string{runtimeDir, inherited}, ":")
}

// LoopbackLocalAddr returns the local UDP address to send from when talking
// to host: the same loopback address when host is a loopback address, so
// the sending socket is bound to loopback only and never to all
// interfaces; nil (the system's choice) otherwise.
func LoopbackLocalAddr(host string) *net.UDPAddr {
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil
	}
	return &net.UDPAddr{IP: ip}
}

// StopRequestPath returns the stop request file of a data folder.
func StopRequestPath(dataDir string) string {
	return filepath.Join(dataDir, ControlDirRel, ControlStop)
}

// PortProtocols are the protocols the query port is reached with.
// CONFIRMED: UDP.
var PortProtocols = []string{"udp"}

// InboundPorts says which of an instance's ports must be reachable from
// outside the box. CONFIRMED: only the query port. The game port has no
// socket at all: players join through Steam's relay, over outbound UDP.
// ESM still gives every instance its own game port, which the server is
// told and puts in its advert. UNCONFIRMED: two or more servers on one box,
// told apart by their ports only, and a query port other than the default.
const InboundPorts = "query"
