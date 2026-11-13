package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// testAppID is a made-up Steam app id.
const testAppID = "1000001"

// logLines returns the lines of a log file that match.
func logLines(t *testing.T, path string, match func(string) bool) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if match(line) {
			out = append(out, line)
		}
	}
	return out
}

func hasPrefix(p string) func(string) bool {
	return func(line string) bool { return strings.Contains(line, "LogFake: "+p) }
}

// A server started the way a real one is: the build's default map, the
// real switch set, the build folder as working folder, the Steam runtime
// on LD_LIBRARY_PATH, ready from the session line in its log, its Steam
// server id shown while it runs, one stop that saves under the data
// folder, and a restart that resumes the world.
func TestRealServerShape(t *testing.T) {
	t.Parallel()
	h := newESM(t, func(cfg map[string]any) { cfg["steam_app_id"] = testAppID })
	b := h.install(testkit.BuildOptions{Version: "1.0"})
	if b.DefaultMap != testkit.DefaultMap {
		t.Fatalf("build view %+v", b)
	}
	buildDir := filepath.Join(h.root, "builds", "1.0")
	exe := filepath.Join(buildDir, filepath.FromSlash(b.Executable))
	if got, err := os.ReadFile(filepath.Join(filepath.Dir(exe), gameserver.SteamAppIDFile)); err != nil || string(got) != testAppID {
		t.Fatalf("the app id file next to the server binary: %q %v", got, err)
	}
	// No status file and no query answers: only the log can say ready.
	h.create("alpha", "", map[string]string{"FakeNoStatusFile": "true", "FakeNoQuery": "true"})
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	if st.State != instances.Running || st.ReadySource != instances.ReadyLogLine {
		t.Fatalf("after start: %+v", st)
	}
	logFile := h.serverLog("alpha")
	cmd := logLines(t, logFile, hasPrefix("Command line: "))
	data := h.dataDir("alpha")
	want := "Command line: " + testkit.DefaultMap + " -unattended -Port=" // then the ports, names and players
	if len(cmd) != 1 || !strings.Contains(cmd[0], want) || strings.Contains(strings.ToUpper(cmd[0]), "MULTIHOME") ||
		!strings.Contains(cmd[0], " -ListInBrowser=True -UserDir="+data+" -abslog="+logFile+" ") {
		t.Fatalf("the server's command line: %q", cmd)
	}
	if wd := logLines(t, logFile, hasPrefix("Working folder: ")); len(wd) != 1 || !strings.HasSuffix(wd[0], "Working folder: "+buildDir) {
		t.Fatalf("working folder %q, want the build folder %s", wd, buildDir)
	}
	lib := logLines(t, logFile, hasPrefix("Library path: "))
	if runtime.GOOS != "windows" {
		if len(lib) != 1 || !strings.Contains(lib[0], "Library path: "+filepath.Join(h.root, "steam", "linux64")) {
			t.Fatalf("library path %q", lib)
		}
	}
	ids := logLines(t, logFile, func(l string) bool { _, ok := gameserver.ServerID(l); return ok })
	firstID, _ := gameserver.ServerID(ids[0])
	var v instances.View
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
	if v.Live.SteamServerID == nil || *v.Live.SteamServerID != firstID {
		t.Fatalf("live steam server id %v, the log says %s", v.Live.SteamServerID, firstID)
	}

	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, &st)
	if st.State != instances.Stopped {
		t.Fatalf("after stop: %+v", st)
	}
	saved := h.liveCounter("alpha", "alpha")
	if saved < 2 || testkit.SecondSignal(data) {
		t.Fatalf("save counter %d after the stop, second signal %v", saved, testkit.SecondSignal(data))
	}
	if len(logLines(t, logFile, func(l string) bool { return strings.Contains(l, gameserver.FlushedLine) })) != 1 {
		t.Fatal("no flush line at the stop")
	}
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
	if v.Live.SteamServerID != nil {
		t.Fatalf("a stopped server has a steam server id: %s", *v.Live.SteamServerID)
	}

	// The next start resumes the saved world, in a new log, with a new id.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	if n := len(logLines(t, logFile, gameserver.IsWorldResumedLine)); n != 1 {
		t.Fatalf("%d resume lines in the new log", n)
	}
	if len(logLines(t, filepath.Join(h.instDir("alpha"), "logs", "server.previous.log"), gameserver.IsReadyLine)) != 1 {
		t.Fatal("the previous run's log was not kept")
	}
	if h.liveCounter("alpha", "alpha") <= saved {
		t.Fatal("the world did not carry on from its save")
	}
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
	if v.Live.SteamServerID == nil || *v.Live.SteamServerID == firstID {
		t.Fatalf("steam server id after a restart: %v (first run %s)", v.Live.SteamServerID, firstID)
	}
	// A backup holds the whole save folder of the data folder.
	var bk instances.Backup
	h.must(http.StatusCreated, "POST", "/api/v1/instances/alpha/backup", struct{}{}, &bk)
	if len(bk.Files) < 2 || !slices.ContainsFunc(bk.Files, func(f string) bool { return f == fakeSave("alpha") }) {
		t.Fatalf("backup files %q", bk.Files)
	}
	for _, f := range bk.Files {
		if !strings.HasPrefix(f, gameserver.SaveDirRel+"/") {
			t.Fatalf("backup file %q is not below %s", f, gameserver.SaveDirRel)
		}
	}
}

// With no status file and no ready line, the server is seen ready by its
// answer to A2S_INFO on the query port.
func TestReadyThroughQueryPort(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", map[string]string{"FakeNoStatusFile": "true", "FakeNoReadyLine": "true"})
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	if st.State != instances.Running || st.ReadySource != instances.ReadyQueryPort {
		t.Fatalf("after start: %+v", st)
	}
	if n := len(logLines(t, h.serverLog("alpha"), gameserver.IsReadyLine)); n != 0 {
		t.Fatalf("%d ready lines in the log", n)
	}
}

// A build without a default map runs only instances that name their map.
func TestMapFromBuildOrInstance(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0", DefaultMap: "-"})
	h.create("alpha", "", nil)
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/instances/alpha/start", h.admin, struct{}{}, &eb); st != http.StatusConflict || eb.Error.Code != "no_map" {
		t.Fatalf("start without a map: %d %+v", st, eb)
	}
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", map[string]any{"map": "/Levels/Example/Example"}, nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	if cmd := logLines(t, h.serverLog("alpha"), hasPrefix("Command line: /Levels/Example/Example -unattended ")); len(cmd) != 1 {
		t.Fatal("the instance's map is not the first argument")
	}
}

// -MULTIHOME is refused as an extra argument and as a rule.
func TestMultihomeRefused(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	for _, body := range []map[string]any{
		{"name": "alpha", "extra_args": []string{"-MULTIHOME=192.0.2.10"}},
		{"name": "alpha", "extra_args": []string{"-multihome 192.0.2.10"}},
		{"name": "alpha", "rules": map[string]string{"MultiHome": "192.0.2.10"}},
	} {
		var eb api.ErrorBody
		if st := h.call("POST", "/api/v1/instances", h.admin, body, &eb); st != http.StatusUnprocessableEntity ||
			!strings.Contains(eb.Error.Message, "never passed") {
			t.Fatalf("%v: %d %+v", body, st, eb)
		}
	}
}

// A stop is one signal. A server that does not exit in time is left
// running: the stop answers stop_timeout, the instance is failed with a
// reason that says so, and a second stop waits again without a second
// signal.
func TestStopTimeoutLeavesServerRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows (development only) kills a server that does not stop")
	}
	t.Parallel()
	stopTimeoutLeavesServerRunning(t, newESM(t, func(cfg map[string]any) { cfg["stop_timeout"] = "1s" }), "alpha")
}

func TestStopTimeoutLeavesServerRunningSystemd(t *testing.T) {
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	stopTimeoutLeavesServerRunning(t, h, unitSafeName("sdstop"))
}

func stopTimeoutLeavesServerRunning(t *testing.T, h *harness, name string) {
	if len(h.builds()) == 0 {
		h.install(testkit.BuildOptions{Version: "1.0"})
	}
	h.create(name, "", map[string]string{"FakeIgnoreStop": "true"})
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, &st)
	pid := st.PID
	for i := range 2 {
		var eb api.ErrorBody
		if code := h.call("POST", "/api/v1/instances/"+name+"/stop", h.admin, struct{}{}, &eb); code != http.StatusUnprocessableEntity ||
			eb.Error.Code != "stop_timeout" {
			t.Fatalf("stop %d: %d %+v", i+1, code, eb)
		}
		st = h.status(name)
		if st.State != instances.Failed || !strings.Contains(st.Reason, "left running") || !strings.Contains(st.Reason, "no second signal") {
			t.Fatalf("after stop %d: %+v", i+1, st)
		}
	}
	time.Sleep(300 * time.Millisecond)
	data := h.dataDir(name)
	if testkit.SecondSignal(data) {
		t.Fatal("the server got a second stop signal")
	}
	if n := len(logLines(t, h.serverLog(name), hasPrefix("Ignoring stop request"))); n != 1 {
		t.Fatalf("the server saw %d stop requests, want 1", n)
	}
	// It still runs, so it cannot be started a second time.
	var eb api.ErrorBody
	if code := h.call("POST", "/api/v1/instances/"+name+"/start", h.admin, struct{}{}, &eb); code != http.StatusConflict ||
		eb.Error.Code != "already_running" {
		t.Fatalf("start while the old server runs: %d %+v", code, eb)
	}
	if st := h.status(name); st.PID != pid {
		t.Fatalf("the server's pid changed from %d: %+v", pid, st)
	}
}

// builds lists the installed build versions.
func (h *harness) builds() []string {
	h.t.Helper()
	var list api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &list)
	var out []string
	for _, b := range list.Builds {
		out = append(out, b.Version)
	}
	return out
}
