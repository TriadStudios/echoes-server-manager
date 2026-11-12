// Command esm-fakeserver stands in for the Echoes of Durinn dedicated server
// in tests. It follows the assumptions in internal/gameserver exactly: it
// takes the same command line, writes the same server log lines, status
// file and save files, answers the query port, and stops the same way.
//
// Extra switches, only for tests:
//
//	-FakeSaveInterval=1s   time between world saves
//	-FakeReadyDelay=300ms  time before the server reports ready
//	-FakeCrashAfter=<dur>  exit with the crash code after this long
//	-FakeIgnoreStop        ignore stop requests (only a kill ends it)
//	-FakeNoReady           never report ready
//	-FakeFailStart         exit with code 3 at once
//	-FakeExitCode=N        exit code used for crashes (default 1)
//
// The same switches may be set in fake.json next to the executable (for
// example {"FakeFailStart": true}), so a whole build can be made broken.
// Command-line values win over fake.json.
//
// Request files in <data>/control/: "stop" (polite stop), "crash" (exit with
// the crash code now), "hang" (ignore stop requests from now on).
//
// The query port answers PING with PONG only once the server is ready.
package main

import (
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
)

// Test-only request file names in <data>/control/.
const (
	controlCrash = "crash"
	controlHang  = "hang"
)

type options struct {
	mapName      string
	port         int
	queryPort    int
	multihome    string
	serverName   string
	worldName    string
	maxPlayers   int
	dataDir      string
	saveInterval time.Duration
	readyDelay   time.Duration
	crashAfter   time.Duration
	ignoreStop   bool
	noReady      bool
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

// parseArgs reads the adapter's command line: an optional positional map
// first, then -Key=Value or -Flag entries.
func parseArgs(args []string, kv map[string]string) (mapName string, err error) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			if i == 0 {
				mapName = a
				continue
			}
			return "", fmt.Errorf("unexpected argument %q", a)
		}
		k, v, ok := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !ok {
			v = "true"
		}
		kv[k] = v
	}
	return mapName, nil
}

func buildOptions(mapName string, kv map[string]string) (options, error) {
	o := options{
		mapName:      mapName,
		multihome:    kv["MULTIHOME"],
		serverName:   kv["ServerName"],
		worldName:    kv["WorldName"],
		dataDir:      kv[strings.TrimPrefix(gameserver.DataDirSwitch, "-")],
		saveInterval: time.Second,
		readyDelay:   300 * time.Millisecond,
		exitCode:     1,
	}
	var err error
	num := func(key string, dst *int) {
		if v, ok := kv[key]; ok && err == nil {
			*dst, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("-%s: %w", key, err)
			}
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v, ok := kv[key]; ok && err == nil {
			*dst, err = time.ParseDuration(v)
			if err != nil {
				err = fmt.Errorf("-%s: %w", key, err)
			}
		}
	}
	flag := func(key string, dst *bool) {
		if v, ok := kv[key]; ok && err == nil {
			*dst, err = strconv.ParseBool(v)
			if err != nil {
				err = fmt.Errorf("-%s: %w", key, err)
			}
		}
	}
	num("Port", &o.port)
	num("QueryPort", &o.queryPort)
	num("MaxPlayers", &o.maxPlayers)
	num("FakeExitCode", &o.exitCode)
	dur("FakeSaveInterval", &o.saveInterval)
	dur("FakeReadyDelay", &o.readyDelay)
	dur("FakeCrashAfter", &o.crashAfter)
	flag("FakeIgnoreStop", &o.ignoreStop)
	flag("FakeNoReady", &o.noReady)
	flag("FakeFailStart", &o.failStart)
	if err != nil {
		return o, err
	}
	if o.worldName == "" {
		o.worldName = "world"
	}
	if o.dataDir == "" {
		o.dataDir = "Saved"
	}
	if o.saveInterval <= 0 {
		o.saveInterval = time.Second
	}
	o.dataDir, err = filepath.Abs(o.dataDir)
	return o, err
}

type server struct {
	o       options
	logMu   sync.Mutex
	logFile *os.File
	out     io.Writer
	gameLn  net.PacketConn
	queryLn net.PacketConn
	counter int
	status  gameserver.Status
	ready   atomic.Bool // the query port answers only once ready
}

func (s *server) logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s\n", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		s.logFile.WriteString(line)
	}
	io.WriteString(s.out, line)
}

func run(args []string) int {
	kv, err := loadFakeJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	mapName, err := parseArgs(args, kv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	o, err := buildOptions(mapName, kv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	s := &server{o: o, out: os.Stdout}
	for _, d := range []string{"Logs", "SaveGames", gameserver.ControlDirRel} {
		if err := os.MkdirAll(filepath.Join(o.dataDir, d), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
			return 2
		}
	}
	s.logFile, err = os.OpenFile(gameserver.ServerLogPath(o.dataDir), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "esm-fakeserver:", err)
		return 2
	}
	defer s.logFile.Close()

	s.logf("Fake server starting: pid %d, argv %q", os.Getpid(), gameserver.MaskArgs(args))
	if o.failStart {
		s.logf("Failing start on purpose")
		return 3
	}
	s.status = gameserver.Status{State: gameserver.StateStarting, PID: os.Getpid(), Version: findVersion()}
	host := o.multihome
	if host == "" {
		host = "0.0.0.0"
	}
	s.gameLn, err = net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(o.port)))
	if err != nil {
		s.logf("Cannot bind game port: %v", err)
		return 4
	}
	defer s.gameLn.Close()
	s.queryLn, err = net.ListenPacket("udp", net.JoinHostPort(host, strconv.Itoa(o.queryPort)))
	if err != nil {
		s.logf("Cannot bind query port: %v", err)
		return 4
	}
	defer s.queryLn.Close()
	s.status.Ports = gameserver.Ports{
		Game:  s.gameLn.LocalAddr().(*net.UDPAddr).Port,
		Query: s.queryLn.LocalAddr().(*net.UDPAddr).Port,
	}
	s.writeStatus()
	go s.answerQueries()
	go drain(s.gameLn)
	s.counter = s.loadCounter()

	sig := make(chan os.Signal, 2)
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

	stop := func(why string) (exit bool) {
		if ignoreStop {
			s.logf("Ignoring stop request (%s)", why)
			return false
		}
		s.logf("Stop requested (%s)", why)
		s.status.State = gameserver.StateStopping
		s.writeStatus()
		s.save()
		s.logf("Shutting down")
		return true
	}

	for {
		select {
		case <-ready:
			ready = nil
			s.status.State = gameserver.StateReady
			s.writeStatus()
			s.ready.Store(true)
			s.logf("Server is ready (players 0/%d, map %s, world %s)", o.maxPlayers, o.mapName, o.worldName)
		case <-saveTick.C:
			s.save()
		case <-crash:
			s.logf("Crashing on purpose (exit %d)", o.exitCode)
			return o.exitCode
		case <-poll.C:
			if s.takeControl(controlCrash) {
				s.logf("Crash requested (exit %d)", o.exitCode)
				return o.exitCode
			}
			if s.takeControl(controlHang) {
				ignoreStop = true
				s.logf("Hang requested: stop requests are ignored from now on")
			}
			if s.takeControl(gameserver.ControlStop) && stop("stop request file") {
				return 0
			}
		case got := <-sig:
			if stop(got.String()) {
				return 0
			}
		}
	}
}

func (s *server) takeControl(name string) bool {
	p := filepath.Join(s.o.dataDir, gameserver.ControlDirRel, name)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	os.Remove(p)
	return true
}

func (s *server) answerQueries() {
	buf := make([]byte, 512)
	for {
		n, addr, err := s.queryLn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // Windows reports ICMP errors here; keep serving
		}
		if string(buf[:n]) == "PING" && s.ready.Load() {
			s.queryLn.WriteTo([]byte("PONG"), addr)
		}
	}
}

func drain(c net.PacketConn) {
	buf := make([]byte, 2048)
	for {
		if _, _, err := c.ReadFrom(buf); errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

func (s *server) savePath() string {
	return filepath.Join(s.o.dataDir, filepath.FromSlash(gameserver.SaveFileRel(s.o.worldName)))
}

// loadCounter continues the save counter of an existing save file.
func (s *server) loadCounter() int {
	b, err := os.ReadFile(s.savePath())
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(b)) {
		if v, ok := strings.CutPrefix(f, "counter="); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	return 0
}

func (s *server) save() {
	s.counter++
	content := fmt.Sprintf("fake-save world=%s counter=%d\n", s.o.worldName, s.counter)
	if err := writeAtomic(s.savePath(), []byte(content)); err != nil {
		s.logf("Save failed: %v", err)
		return
	}
	s.status.LastSave = time.Now().UTC()
	s.writeStatus()
	s.logf("World saved (%d)", s.counter)
}

func (s *server) writeStatus() {
	b, _ := json.MarshalIndent(s.status, "", "  ")
	if err := writeAtomic(gameserver.StatusFilePath(s.o.dataDir), append(b, '\n')); err != nil {
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
