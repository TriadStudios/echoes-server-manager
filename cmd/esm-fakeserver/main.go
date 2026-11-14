// Command esm-fakeserver stands in for the Echoes of Durinn dedicated server
// in tests. It follows the assumptions in internal/gameserver exactly: it
// takes the same command line, writes log lines of the same shape, keeps
// its saves in the same folder, answers the query port, and stops the same
// way.
//
// Like the real server it takes the map first, then -unattended, -Port=,
// -QueryPort=, -ServerName=, -WorldName=, -MaxPlayers=, -ListInBrowser=,
// -UserDir=<data folder> and -abslog=<log file>, plus any -Key=Value. It
// refuses -MULTIHOME (exit code 2): ESM must never pass it. Like the real
// server it joins its arguments into one line and looks every switch up
// there (testkit/cmdline), so it reads what the real server would read: a
// switch inside the value of another one, the first of two, and the next
// argument for an empty value.
//
// Extra switches, only for tests:
//
//	-FakeSaveInterval=1s   time between world saves
//	-FakeReadyDelay=300ms  time before the server reports ready
//	-FakeStopDelay=200ms   time between the stop signal and the exit
//	-FakeCrashAfter=<dur>  exit with the crash code after this long
//	-FakeIgnoreStop        ignore stop requests (only a kill ends it)
//	-FakeNoReady           never report ready
//	-FakeNoReadyLine       report ready without writing the ready log line
//	-FakeNoStatusFile      write no status file
//	-FakeNoQuery           never answer the query port
//	-FakeIgnoreSecretsFile behave like a server build that does not know the
//	                       secrets file: ignore its switch
//	-FakeFailStart         exit with code 3 at once
//	-FakeExitCode=N        exit code used for crashes (default 1)
//
// Unlike a real server, the fake binds loopback addresses only (the query
// port on 127.0.0.1), so no test ever opens a listener a desktop firewall
// would ask about. Like a real server it opens no socket on the game port.
//
// The same switches may be set in fake.json next to the executable (for
// example {"FakeFailStart": true}), so a whole build can be made broken.
// Command-line values win over fake.json.
//
// Stopping: the first SIGINT (or SIGTERM) makes it save, write the flush
// line and exit with gameserver.StopExitCode after -FakeStopDelay. A
// SECOND signal is a mistake ESM must never make: the fake then records it
// in the request file "second-signal" (in <data>/control/, never taken by
// the fake itself) and exits at once with SecondSignalExitCode, so tests
// fail loudly. The stop request file (Windows) saves and exits with 0.
//
// Request files in <data>/control/: "stop" (polite stop), "crash" (exit with
// the crash code now), "hang" (ignore stop requests from now on),
// "ready" (report ready now), "unready" (keep running but stop reporting
// ready), "cmdline" (print the command line again), "players" (report the
// player count the file holds, on the query port and in the status file).
//
// Passwords and tokens come from the secrets file named by the adapter's
// switch, or, when that switch is absent, from the adapter's password and
// token switches. Both at once is an error: with a secrets file no password
// or token belongs on the command line. The status file says which
// passwords are set, never what they are.
//
// Like a real server it prints its whole command line, with any passwords
// on it, to its log and its output when it starts, and a new Steam server
// id at every start. Only for tests it also prints its working folder and
// its LD_LIBRARY_PATH.
//
// The query port answers A2S_INFO, the Source server query, only once the
// server is ready: a request without the server's challenge gets the
// challenge reply, one with it the info reply (name, map, players, max
// players). Only for tests it also says whether a password is the right
// one: "JOIN <server password>" and "ADMIN <admin password>" are answered
// with OK or DENIED.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit/cmdline"
)

// Test-only request file names in <data>/control/.
const (
	controlCrash = "crash"
	controlHang  = "hang"
	// controlReady makes the server report ready now, whatever its ready
	// delay.
	controlReady = "ready"
	// controlUnready makes the server stop reporting ready (status file
	// back to starting, no query answers) while it keeps running.
	controlUnready = "unready"
	// controlCommandLine prints the command line again.
	controlCommandLine = "cmdline"
	// controlPlayers sets the player count the server reports (the file
	// holds the number).
	controlPlayers = "players"
	// controlSecondSignal is written (never read) when a second stop
	// signal arrives.
	controlSecondSignal = "second-signal"
)

// SecondSignalExitCode is the exit code after a second stop signal.
const SecondSignalExitCode = 98

// saveFolder is the folder below gameserver.SaveDirRel the fake keeps its
// save in, like a game keeps its saves in a folder of its own.
const saveFolder = "Fake"

// Test-only questions on the query port: each is followed by a password and
// answered with answerOK or answerDenied.
const (
	askJoin      = "JOIN "
	askAdmin     = "ADMIN "
	answerOK     = "OK"
	answerDenied = "DENIED"
)

type options struct {
	mapName      string
	port         int
	queryPort    int
	serverName   string
	worldName    string
	maxPlayers   int
	listed       bool
	userDir      string
	logFile      string
	secrets      gameserver.Secrets
	saveInterval time.Duration
	readyDelay   time.Duration
	stopDelay    time.Duration
	crashAfter   time.Duration
	ignoreStop   bool
	noReady      bool
	noReadyLine  bool
	noStatusFile bool
	noQuery      bool
	failStart    bool
	exitCode     int
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// loadFakeJSON reads fake.json next to the executable into switch values.
func loadFakeJSON() (map[string]string, error) {
	kv := map[string]string{}
	exe, err := os.Executable()
	if err != nil {
		return kv, nil
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "fake.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return kv, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("fake.json: %w", err)
	}
	for k, v := range raw {
		switch x := v.(type) {
		case bool:
			kv[k] = strconv.FormatBool(x)
		case float64:
			kv[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case string:
			kv[k] = x
		default:
			return nil, fmt.Errorf("fake.json: unsupported value for %s", k)
		}
	}
	return kv, nil
}

// switches are the fake's settings: the line its arguments are joined into,
// where it looks every switch up the way the game server does (see
// testkit/cmdline), and fake.json, for what the line does not have.
type switches struct {
	line string
	file map[string]string
}

// value returns the value of the switch name.
func (s switches) value(name string) (string, bool) {
	if v, ok := cmdline.Value(s.line, name); ok {
		return v, true
	}
	v, ok := s.file[name]
	return v, ok
}

// flag reads the switch name as on or off: the line's "name=" or bare
// switch, else fake.json.
func (s switches) flag(name string) (on, set bool, err error) {
	if on, set := cmdline.Bool(s.line, name); set {
		return on, true, nil
	}
	v, ok := s.file[name]
	if !ok {
		return false, false, nil
	}
	on, err = strconv.ParseBool(v)
	return on, true, err
}

// parseArgs reads the adapter's command line: an optional positional map
// first, then switches, which are read from the line they are joined into,
// as the server reads them.
func parseArgs(args []string, file map[string]string) (mapName string, sw switches, err error) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			if i == 0 {
				mapName = a
				continue
			}
			return "", switches{}, fmt.Errorf("unexpected argument %q", a)
		}
	}
	sw = switches{line: cmdline.Join(args), file: file}
	multihome := key(gameserver.MultihomeSwitch)
	if _, found := cmdline.Value(sw.line, multihome); found || cmdline.Param(sw.line, multihome) {
		return "", switches{}, fmt.Errorf("%s is refused: ESM must never pass it", gameserver.MultihomeSwitch)
	}
	return mapName, sw, nil
}

func key(name string) string { return strings.TrimLeft(name, "-") }

func buildOptions(mapName string, sw switches) (options, error) {
	str := func(name string) string {
		v, _ := sw.value(name)
		return v
	}
	o := options{
		mapName:      mapName,
		serverName:   str(key(gameserver.ServerNameSwitch)),
		worldName:    str(key(gameserver.WorldNameSwitch)),
		userDir:      str(key(gameserver.UserDirSwitch)),
		logFile:      str(key(gameserver.LogFileSwitch)),
		saveInterval: time.Second,
		readyDelay:   300 * time.Millisecond,
		stopDelay:    200 * time.Millisecond,
		exitCode:     1,
	}
	var err error
	num := func(key string, dst *int) {
		if v, ok := sw.value(key); ok && err == nil {
			*dst, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("-%s: %w", key, err)
			}
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := sw.value(key); ok && err == nil {
			*dst, err = time.ParseDuration(v)
			if err != nil {
				err = fmt.Errorf("-%s: %w", key, err)
			}
		}
	}
	flag := func(key string, dst *bool) {
		if on, set, ferr := sw.flag(key); set && err == nil {
			*dst = on
			if ferr != nil {
				err = fmt.Errorf("-%s: %w", key, ferr)
			}
		}
	}
	num(key(gameserver.PortSwitch), &o.port)
	num(key(gameserver.QueryPortSwitch), &o.queryPort)
	num(key(gameserver.MaxPlayersSwitch), &o.maxPlayers)
	flag(key(gameserver.ListInBrowserSwitch), &o.listed)
	num("FakeExitCode", &o.exitCode)
	dur("FakeSaveInterval", &o.saveInterval)
	dur("FakeReadyDelay", &o.readyDelay)
	dur("FakeStopDelay", &o.stopDelay)
	dur("FakeCrashAfter", &o.crashAfter)
	flag("FakeIgnoreStop", &o.ignoreStop)
	flag("FakeNoReady", &o.noReady)
	flag("FakeNoReadyLine", &o.noReadyLine)
	flag("FakeNoStatusFile", &o.noStatusFile)
	flag("FakeNoQuery", &o.noQuery)
	flag("FakeFailStart", &o.failStart)
	if err != nil {
		return o, err
	}
	if o.secrets, err = readSecrets(sw); err != nil {
		return o, err
	}
	if o.mapName == "" {
		o.mapName = "TestMap"
	}
	if o.worldName == "" {
		o.worldName = "world"
	}
	if o.userDir == "" {
		o.userDir = "fake-user"
	}
	if o.saveInterval <= 0 {
		o.saveInterval = time.Second
	}
	if o.userDir, err = filepath.Abs(o.userDir); err != nil {
		return o, err
	}
	if o.logFile == "" {
		o.logFile = filepath.Join(o.userDir, "Saved", "Logs", "server.log")
	}
	o.logFile, err = filepath.Abs(o.logFile)
	return o, err
}

// readSecrets takes the passwords and tokens from the secrets file when its
// switch is given, and from the password and token switches otherwise. A
// password or token switch next to the secrets file switch is refused.
// Errors never repeat a value.
func readSecrets(sw switches) (gameserver.Secrets, error) {
	path, fromFile := cmdline.Value(sw.line, key(gameserver.SecretsFileSwitch))
	if ignore, _, _ := sw.flag("FakeIgnoreSecretsFile"); ignore {
		fromFile = false
	}
	if !fromFile {
		v := func(name string) string {
			s, _ := cmdline.Value(sw.line, key(name))
			return s
		}
		return gameserver.Secrets{
			ServerPassword:     v(gameserver.ServerPasswordSwitch),
			AdminPassword:      v(gameserver.AdminPasswordSwitch),
			AnnouncementsToken: v(gameserver.AnnouncementsTokenSwitch),
			ProblemReportToken: v(gameserver.ProblemReportTokenSwitch),
		}, nil
	}
	for _, s := range cmdline.Switches(sw.line) {
		if low := strings.ToLower(s.Name); strings.Contains(low, "password") || strings.HasSuffix(low, "token") {
			return gameserver.Secrets{}, fmt.Errorf("-%s is not accepted together with %s: with a secrets file no password or token belongs on the command line",
				s.Name, gameserver.SecretsFileSwitch)
		}
	}
	s, err := gameserver.ReadSecretsFile(path)
	if err != nil {
		return gameserver.Secrets{}, fmt.Errorf("%s: %w", gameserver.SecretsFileSwitch, err)
	}
	return s, nil
}

type server struct {
	o       options
	logMu   sync.Mutex
	logFile *os.File
	out     io.Writer
	queryLn net.PacketConn
	counter int
	status  gameserver.Status
	ready   atomic.Bool // the query port answers only once ready
	players atomic.Int32
}

// logf writes one log line the way the engine does: time and frame in
// square brackets, then the category.
func (s *server) logf(format string, args ...any) { s.logIn("LogFake", format, args...) }

// logIn writes one log line under category.
func (s *server) logIn(category, format string, args ...any) {
	now := time.Now().UTC()
	line := fmt.Sprintf("[%s:%03d][%3d]%s: %s\n", now.Format("2006.01.02-15.04.05"), now.Nanosecond()/1e6, 0,
		category, fmt.Sprintf(format, args...))
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		s.logFile.WriteString(line)
	}
	io.WriteString(s.out, line)
}

func run(args []string) int {
	file, err := loadFakeJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	mapName, sw, err := parseArgs(args, file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	o, err := buildOptions(mapName, sw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	s := &server{o: o, out: os.Stdout}
	for _, d := range []string{filepath.Join(o.userDir, gameserver.ControlDirRel), filepath.Dir(s.savePath()), filepath.Dir(o.logFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
			return 2
		}
	}
	s.logFile, err = os.OpenFile(o.logFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	defer s.logFile.Close()

	s.logf("Fake server starting: pid %d", os.Getpid())
	s.logCommandLine(args)
	wd, _ := os.Getwd()
	s.logf("Working folder: %s", wd)
	s.logf("Library path: %s", os.Getenv(gameserver.LibraryPathVar))
	if o.failStart {
		s.logf("Failing start on purpose")
		return 3
	}
	s.status = gameserver.Status{State: gameserver.StateStarting, PID: os.Getpid(), Version: findVersion(),
		ServerPasswordSet: o.secrets.ServerPassword != "", AdminPasswordSet: o.secrets.AdminPassword != "",
		AnnouncementsTokenSet: o.secrets.AnnouncementsToken != "", ProblemReportTokenSet: o.secrets.ProblemReportToken != ""}
	// Loopback only, unlike a real server, which listens on every address.
	// No socket on the game port, like a real server.
	s.queryLn, err = net.ListenPacket("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.queryPort)))
	if err != nil {
		s.logf("Cannot bind query port: %v", err)
		return 4
	}
	defer s.queryLn.Close()
	s.status.Ports = gameserver.Ports{Game: o.port, Query: s.queryLn.LocalAddr().(*net.UDPAddr).Port}
	s.writeStatus()
	go s.answerQueries()
	s.counter = s.loadCounter()
	if s.counter > 0 {
		s.logf("%s (slot %s)", gameserver.ResumedLine(o.worldName), s.slot())
	} else {
		s.logf("%s (slot %s)", gameserver.NewWorldLine(o.worldName), s.slot())
	}
	s.save()
	s.logIn(gameserver.ServerIDCategory, "%s", gameserver.ServerIDLine(newServerID()))

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	var ready <-chan time.Time
	if !o.noReady {
		ready = time.After(o.readyDelay)
	}
	var crash <-chan time.Time
	if o.crashAfter > 0 {
		crash = time.After(o.crashAfter)
	}
	saveTick := time.NewTicker(o.saveInterval)
	defer saveTick.Stop()
	poll := time.NewTicker(100 * time.Millisecond)
	defer poll.Stop()
	ignoreStop := o.ignoreStop
	signals := 0
	var exitAt <-chan time.Time
	exitCode := 0

	// stop saves the world for a stop request; the exit follows after the
	// stop delay with code.
	stop := func(why string, code int) {
		if ignoreStop {
			s.logf("Ignoring stop request (%s)", why)
			return
		}
		if exitAt != nil {
			return
		}
		s.logf("Stop requested (%s)", why)
		s.status.State = gameserver.StateStopping
		s.writeStatus()
		s.ready.Store(false)
		s.save()
		s.logf("%s", gameserver.FlushedLine)
		exitAt, exitCode = time.After(o.stopDelay), code
	}

	becomeReady := func() {
		ready = nil
		s.status.State = gameserver.StateReady
		s.writeStatus()
		s.ready.Store(true)
		if o.noReadyLine {
			s.logf("Ready (no ready line on purpose)")
			return
		}
		s.logf("%s", gameserver.ReadyLine(o.serverName, shortMap(o.mapName), o.maxPlayers))
	}

	for {
		select {
		case <-exitAt:
			s.logf("Exiting with code %d", exitCode)
			return exitCode
		case <-ready:
			becomeReady()
		case <-saveTick.C:
			if exitAt == nil {
				s.save()
			}
		case <-crash:
			s.logf("Crashing on purpose (exit %d)", o.exitCode)
			return o.exitCode
		case <-poll.C:
			if s.takeControl(controlCrash) {
				s.logf("Crash requested (exit %d)", o.exitCode)
				return o.exitCode
			}
			if s.takeControl(controlReady) && s.status.State != gameserver.StateReady {
				becomeReady()
			}
			if s.takeControl(controlUnready) {
				ready = nil
				s.ready.Store(false)
				s.status.State = gameserver.StateStarting
				s.writeStatus()
				s.logf("Unready requested: no longer reporting ready")
			}
			if s.takeControl(controlCommandLine) {
				s.logCommandLine(args)
			}
			if v, ok := s.takeControlValue(controlPlayers); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
					s.players.Store(int32(n))
					s.status.Players = n
					s.writeStatus()
					s.logf("Players requested: %d", n)
				}
			}
			if s.takeControl(controlHang) {
				ignoreStop = true
				s.logf("Hang requested: stop requests are ignored from now on")
			}
			if s.takeControl(gameserver.ControlStop) {
				stop("stop request file", 0)
			}
		case got := <-sig:
			signals++
			if signals > 1 {
				s.logf("SECOND stop signal (%s): a stop must be one signal only", got)
				s.writeControl(controlSecondSignal, fmt.Sprintf("signal %d: %s\n", signals, got))
				return SecondSignalExitCode
			}
			code := gameserver.StopExitCode
			if got == syscall.SIGTERM {
				code = 143
			}
			stop(got.String(), code)
		}
	}
}

// newServerID returns a made-up Steam server id: 17 digits, new each time.
func newServerID() string {
	var b [8]byte
	rand.Read(b[:])
	return strconv.FormatUint(90000000000000000+binary.BigEndian.Uint64(b[:])%9999999999999999, 10)
}

// shortMap is the map's own name: the last part of a path such as
// "/Some/Folder/Name.Name".
func shortMap(m string) string {
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "."); i >= 0 {
		m = m[:i]
	}
	return m
}

// slot names the world's save: map and world.
func (s *server) slot() string { return shortMap(s.o.mapName) + "__" + s.o.worldName }

// logCommandLine prints the arguments as they came, the way real servers
// print their command line.
func (s *server) logCommandLine(args []string) {
	s.logf("Command line: %s", strings.Join(args, " "))
}

// takeControl reports whether the request file name is there, and takes
// it. A request counts only once its file is gone: on Windows the file
// cannot be removed while its writer still has it open, and a request
// left in place would be acted on again by the next server.
func (s *server) takeControl(name string) bool {
	p := filepath.Join(s.o.userDir, gameserver.ControlDirRel, name)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return os.Remove(p) == nil
}

// takeControlValue is takeControl for a request file whose content matters.
func (s *server) takeControlValue(name string) (string, bool) {
	p := filepath.Join(s.o.userDir, gameserver.ControlDirRel, name)
	b, err := os.ReadFile(p)
	if err != nil || os.Remove(p) != nil {
		return "", false
	}
	return string(b), true
}

func (s *server) writeControl(name, content string) {
	os.WriteFile(filepath.Join(s.o.userDir, gameserver.ControlDirRel, name), []byte(content), 0o600)
}

func (s *server) answerQueries() {
	var challenge [4]byte
	rand.Read(challenge[:])
	buf := make([]byte, 512)
	for {
		n, addr, err := s.queryLn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // Windows reports ICMP errors here; keep serving
		}
		if s.o.noQuery {
			continue
		}
		msg := string(buf[:n])
		if got, ok := gameserver.ParseInfoRequest(buf[:n]); ok && s.ready.Load() {
			if string(got) != string(challenge[:]) {
				s.queryLn.WriteTo(gameserver.ChallengeReply(challenge), addr)
				continue
			}
			s.queryLn.WriteTo(gameserver.InfoReply(s.info()), addr)
			continue
		}
		for ask, want := range map[string]string{askJoin: s.o.secrets.ServerPassword, askAdmin: s.o.secrets.AdminPassword} {
			if given, ok := strings.CutPrefix(msg, ask); ok {
				answer := answerDenied
				if given == want {
					answer = answerOK
				}
				s.queryLn.WriteTo([]byte(answer), addr)
			}
		}
	}
}

// info is the fake's answer to A2S_INFO.
func (s *server) info() gameserver.Info {
	osByte := byte('l')
	if runtime.GOOS == "windows" {
		osByte = 'w'
	}
	return gameserver.Info{Protocol: 17, Name: s.o.serverName, Map: shortMap(s.o.mapName), Folder: "fake", Game: "Fake",
		Players: int(s.players.Load()), MaxPlayers: s.o.maxPlayers, ServerType: 'd', OS: osByte, Password: s.o.secrets.ServerPassword != "",
		Version: findVersion()}
}

// savePath is the world's save file: below the user folder's save folder,
// named after map and world.
func (s *server) savePath() string {
	return filepath.Join(s.o.userDir, filepath.FromSlash(gameserver.SaveDirRel), saveFolder, s.slot()+".json")
}

// fakeSave is the content of the fake's save file.
type fakeSave struct {
	World   string `json:"world"`
	Counter int    `json:"counter"`
}

// loadCounter continues the save counter of an existing save file.
func (s *server) loadCounter() int {
	b, err := os.ReadFile(s.savePath())
	if err != nil {
		return 0
	}
	var f fakeSave
	if json.Unmarshal(b, &f) != nil {
		return 0
	}
	return f.Counter
}

// save writes the world save, keeping the two saves before it as .bak1 and
// .bak2, and a small metadata file next to it.
func (s *server) save() {
	s.counter++
	p := s.savePath()
	content, _ := json.Marshal(fakeSave{World: s.o.worldName, Counter: s.counter})
	content = append(content, '\n')
	if _, err := os.Stat(p); err == nil {
		os.Rename(p+".bak1", p+".bak2")
		copyFile(p, p+".bak1")
	}
	if err := writeAtomic(p, content); err != nil {
		s.logf("Save failed: %v", err)
		return
	}
	meta := strings.TrimSuffix(p, ".json") + ".meta.json"
	writeAtomic(meta, []byte(fmt.Sprintf("{\"saved\":%q}\n", time.Now().UTC().Format(time.RFC3339Nano))))
	s.status.LastSave = time.Now().UTC()
	s.writeStatus()
	s.logf("%s", gameserver.WorldSavedLine(saveFolder+"/"+filepath.Base(p), len(content)))
}

func copyFile(src, dst string) {
	if b, err := os.ReadFile(src); err == nil {
		os.WriteFile(dst, b, 0o600)
	}
}

func (s *server) writeStatus() {
	if s.o.noStatusFile {
		return
	}
	b, _ := json.MarshalIndent(s.status, "", "  ")
	if err := writeAtomic(gameserver.StatusFilePath(s.o.userDir), append(b, '\n')); err != nil {
		s.logf("Status write failed: %v", err)
	}
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	var err error
	for deadline := time.Now().Add(2 * time.Second); ; {
		err = os.Rename(tmp, path)
		if err == nil || runtime.GOOS != "windows" || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond) // a reader holds the file open
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// findVersion reports the build version from manifest.json in the
// executable's folder or one of its two parents.
func findVersion() string {
	exe, err := os.Executable()
	if err != nil {
		return "fake-dev"
	}
	dir := filepath.Dir(exe)
	for range 3 {
		b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err == nil {
			var m struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &m) == nil && m.Version != "" {
				return m.Version
			}
		}
		dir = filepath.Dir(dir)
	}
	return "fake-dev"
}
