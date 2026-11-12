package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// 1: create, start, ready, logs, stop with a saved world.
func TestLifecycle(t *testing.T) {
	t.Parallel()
	lifecycle(t, newESM(t), "alpha")
}

// newSystemdESM starts a service with the systemd supervisor, or skips.
// Tests that start systemd user units only run on request (Linux):
//
//	ESM_TEST_SYSTEMD=1 go test ./internal/e2e -run Systemd
func newSystemdESM(t *testing.T) *harness {
	t.Helper()
	if os.Getenv("ESM_TEST_SYSTEMD") != "1" {
		t.Skip("set ESM_TEST_SYSTEMD=1 to run tests that start systemd user units")
	}
	if !supervisor.SystemdAvailable() {
		t.Skip("systemctl --user does not work here")
	}
	h := newESMWith(t, "systemd")
	var box api.Box
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
	if box.Supervisor != "systemd" || !box.LimitsEnforced {
		t.Fatalf("box %+v", box)
	}
	return h
}

// unitSafeName is an instance name no other test run uses, because user
// unit names are shared by everything the user runs.
func unitSafeName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1_000_000_000)
}

// The same life cycle, crash restart and adoption under systemd user units.
func TestLifecycleSystemd(t *testing.T) {
	lifecycle(t, newSystemdESM(t), unitSafeName("sd"))
}

func TestCrashRestartSystemd(t *testing.T) {
	crashRestart(t, newSystemdESM(t), unitSafeName("sdc"))
}

func TestReadoptionSystemd(t *testing.T) {
	readoption(t, newSystemdESM(t), unitSafeName("sda"), unitSafeName("sdb"))
}

func lifecycle(t *testing.T, h *harness, name string) {
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create(name, "", nil)
	if v.Build != "1.0" || v.Channel != "live" || !v.Listed || !v.RestartOnCrash || v.Ports.Game != h.gameLo {
		t.Fatalf("created %+v", v)
	}
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, &st)
	if st.State != instances.Running || st.ReadySource == "" || st.PID == 0 {
		t.Fatalf("after start: %+v", st)
	}
	var logs api.Logs
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/logs?lines=20", nil, &logs)
	if len(logs.Lines) == 0 || logs.Source != "server" {
		t.Fatalf("logs %+v", logs)
	}
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/logs?source=console", nil, &logs)
	if len(logs.Lines) == 0 {
		t.Fatal("console log is empty")
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/stop", struct{}{}, &st)
	if st.State != instances.Stopped {
		t.Fatalf("after stop: %+v", st)
	}
	if c := h.liveCounter(name, name); c < 1 {
		t.Fatalf("no world save after the stop (counter %d)", c)
	}
	b, _ := os.ReadFile(gameserver.ServerLogPath(h.dataDir(name)))
	if !strings.Contains(string(b), "Stop requested") {
		t.Fatalf("server was not stopped politely:\n%s", b)
	}
}

// 2: a crash leads to an automatic restart.
func TestCrashRestart(t *testing.T) {
	t.Parallel()
	crashRestart(t, newESM(t), "alpha")
}

func crashRestart(t *testing.T, h *harness, name string) {
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create(name, "", nil)
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, &st)
	pid := st.PID
	if err := testkit.WriteControl(h.dataDir(name), testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 15*time.Second, func() bool {
		st = h.status(name)
		return st.State == instances.Running && st.PID != pid
	}, "no automatic restart")
	if st.Crashes != 1 || st.GaveUp || st.ExitCode != nil {
		t.Fatalf("after restart: %+v", st)
	}
}

// 3: repeated crashes end in giving up.
func TestGiveUp(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", map[string]string{"FakeCrashAfter": "250ms"})
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start", struct{}{}, nil)
	st := h.waitState("alpha", instances.Failed, 30*time.Second)
	if !st.GaveUp || st.Crashes != 5 || st.ExitCode == nil || *st.ExitCode != 1 {
		t.Fatalf("after giving up: %+v", st)
	}
	time.Sleep(400 * time.Millisecond)
	if st := h.status("alpha"); st.State != instances.Failed {
		t.Fatalf("restarted after giving up: %+v", st)
	}
}

// 4: a restarted ESM adopts a server it left running, and starts the
// autostart instances that are not running.
func TestReadoption(t *testing.T) {
	t.Parallel()
	readoption(t, newESM(t), "alpha", "beta")
}

func readoption(t *testing.T, h *harness, name, auto string) {
	h.install(testkit.BuildOptions{Version: "1.0"})
	v := h.create(name, "", nil)
	h.create(auto, "", nil)
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/"+auto, map[string]any{"autostart": true}, nil)
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/"+name+"/start?wait=true", struct{}{}, &st)
	pid := st.PID
	h.stopService()
	if !strings.Contains(h.stderr.String(), "leaving 1 game servers running") {
		t.Fatalf("shutdown did not report the running server:\n%s", h.stderr.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gameserver.Probe(ctx, "127.0.0.1", v.Ports.Query); err != nil {
		t.Fatalf("server did not survive the ESM shutdown: %v", err)
	}
	h.startService()
	st = h.waitState(name, instances.Running, 10*time.Second)
	if st.PID != pid {
		t.Fatalf("adopted pid %d, want %d", st.PID, pid)
	}
	h.waitState(auto, instances.Running, 10*time.Second)
	for _, n := range []string{name, auto} {
		h.must(http.StatusOK, "POST", "/api/v1/instances/"+n+"/stop", struct{}{}, &st)
		if st.State != instances.Stopped {
			t.Fatalf("stop of %s after the restart: %+v", n, st)
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if gameserver.Probe(ctx2, "127.0.0.1", v.Ports.Query) == nil {
		t.Fatal("server still answers after the stop")
	}
}

// 5: an update to a broken build rolls back; a good one sticks.
func TestUpdateRollback(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.install(testkit.BuildOptions{Version: "1.1-broken", Fake: map[string]any{"FakeFailStart": true}})
	h.install(testkit.BuildOptions{Version: "1.2"})
	h.create("alpha", "1.0", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/instances/alpha/update", h.admin, api.UpdateRequest{To: "1.1-broken"}, &eb); st != http.StatusUnprocessableEntity ||
		eb.Error.Code != "update_failed" || !strings.Contains(eb.Error.Message, "rolled back to 1.0") {
		t.Fatalf("broken update: %d %+v", st, eb)
	}
	st := h.status("alpha")
	if st.State != instances.Running || st.Build != "1.0" {
		t.Fatalf("after rollback: %+v", st)
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.2"}, &st)
	if st.State != instances.Running || st.Build != "1.2" {
		t.Fatalf("after good update: %+v", st)
	}
	var v instances.View
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &v)
	if v.Build != "1.2" || !slices.Equal(v.PreviousBuilds, []string{"1.0"}) {
		t.Fatalf("instance after updates: build %s, previous %v", v.Build, v.PreviousBuilds)
	}
	var backups api.BackupList
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/backups", nil, &backups)
	if len(backups.Backups) != 2 || backups.Backups[0].Reason != instances.ReasonPreUpdate {
		t.Fatalf("pre-update backups: %+v", backups.Backups)
	}
	if st := h.call("POST", "/api/v1/instances/alpha/update", h.admin, api.UpdateRequest{To: "9.9"}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("update to a missing build: %d %+v", st, eb)
	}
}

// 6: backup on demand and restore.
func TestBackupRestore(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	var b instances.Backup
	h.must(http.StatusCreated, "POST", "/api/v1/instances/alpha/backup", struct{}{}, &b)
	if !b.SaveConfirmed || !slices.Equal(b.Files, []string{"SaveGames/alpha.sav"}) || b.Reason != "manual" || b.SizeBytes == 0 {
		t.Fatalf("backup %+v", b)
	}
	saved, err := os.ReadFile(filepath.Join(h.root, "backups", "alpha", b.ID, "SaveGames", "alpha.sav"))
	if err != nil {
		t.Fatal(err)
	}
	n := saveCounter(saved)
	testkit.Eventually(t, 10*time.Second, func() bool { return h.liveCounter("alpha", "alpha") >= n+5 }, "the world is not being saved")

	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: b.ID}, &st)
	if st.State != instances.Running {
		t.Fatalf("after restore: %+v", st)
	}
	var list api.BackupList
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/backups", nil, &list)
	if len(list.Backups) != 2 || list.Backups[0].Reason != instances.ReasonPreRestore {
		t.Fatalf("backups after restore: %+v", list.Backups)
	}
	pre, _ := os.ReadFile(filepath.Join(h.root, "backups", "alpha", list.Backups[0].ID, "SaveGames", "alpha.sav"))
	if c := h.liveCounter("alpha", "alpha"); c >= saveCounter(pre) {
		t.Fatalf("the restored world went on from %d, the pre-restore world was at %d", c, saveCounter(pre))
	}
	// Restoring a stopped instance puts the exact bytes back.
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: b.ID}, &st)
	if st.State != instances.Stopped {
		t.Fatalf("restore of a stopped instance started it: %+v", st)
	}
	got, _ := os.ReadFile(filepath.Join(h.dataDir("alpha"), "SaveGames", "alpha.sav"))
	if !bytes.Equal(got, saved) {
		t.Fatalf("restored save %q, backup %q", got, saved)
	}
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/instances/alpha/restore", h.admin, api.RestoreRequest{Backup: "../../x"}, &eb); st != http.StatusNotFound {
		t.Fatalf("restore of a bad id: %d %+v", st, eb)
	}
}

// 7: two instances run side by side on different ports.
func TestTwoInstances(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	a := h.create("alpha", "", nil)
	b := h.create("beta", "", nil)
	if a.Ports == b.Ports || a.Ports.Game == b.Ports.Game || a.Ports.Query == b.Ports.Query {
		t.Fatalf("ports %+v and %+v", a.Ports, b.Ports)
	}
	for _, n := range []string{"alpha", "beta"} {
		h.must(http.StatusOK, "POST", "/api/v1/instances/"+n+"/start?wait=true", struct{}{}, nil)
	}
	var list api.InstanceList
	h.must(http.StatusOK, "GET", "/api/v1/instances", nil, &list)
	if len(list.Instances) != 2 {
		t.Fatalf("list %+v", list)
	}
	for _, v := range list.Instances {
		if v.Status.State != instances.Running {
			t.Fatalf("%s is %s", v.Name, v.Status.State)
		}
	}
	var box api.Box
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &box)
	if box.Instances != 2 || box.Running != 2 || box.Supervisor != "process" || box.LimitsEnforced {
		t.Fatalf("box %+v", box)
	}
}

// 8: a read token may read but not change anything.
func TestReadToken(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	var tok api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "viewer", Role: "read"}, &tok)
	read := tok.Token
	for _, p := range []string{"/api/v1/instances", "/api/v1/instances/alpha", "/api/v1/box", "/api/v1/builds", "/api/v1/clusters", "/api/v1/session"} {
		if st := h.call("GET", p, read, nil, nil); st != http.StatusOK {
			t.Errorf("GET %s with a read token: %d", p, st)
		}
	}
	refused := []struct{ method, path string }{
		{"POST", "/api/v1/instances/alpha/start"},
		{"POST", "/api/v1/instances/alpha/stop"},
		{"POST", "/api/v1/instances"},
		{"POST", "/api/v1/tokens"},
		{"GET", "/api/v1/tokens"},
		{"GET", "/api/v1/audit"},
	}
	for _, r := range refused {
		var eb api.ErrorBody
		if st := h.call(r.method, r.path, read, map[string]any{}, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
			t.Errorf("%s %s with a read token: %d %+v", r.method, r.path, st, eb)
		}
	}
	if st := h.call("GET", "/api/v1/instances?token="+read, "", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("token in the URL was accepted: %d", st)
	}
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=100", nil, &al)
	refusals := 0
	for _, e := range al.Entries {
		if !e.OK && e.Actor.Kind == audit.ActorToken && e.Actor.ID == tok.ID && e.Actor.Role == "read" {
			refusals++
		}
	}
	if refusals != len(refused) {
		t.Fatalf("audit has %d refusals for the read token, want %d: %+v", refusals, len(refused), al.Entries)
	}
	var tl api.TokenList
	h.must(http.StatusOK, "GET", "/api/v1/tokens", nil, &tl)
	raw, _ := json.Marshal(tl)
	if strings.Contains(string(raw), "sha256") || strings.Contains(string(raw), read) {
		t.Fatalf("token list shows secrets: %s", raw)
	}
	h.must(http.StatusOK, "DELETE", "/api/v1/tokens/"+tok.ID, struct{}{}, nil)
	if st := h.call("GET", "/api/v1/box", read, nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("deleted token still works: %d", st)
	}
}

// 9: setup code, admin login, cookie rules.
func TestSetupAndLogin(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	m := regexp.MustCompile(`ESM setup code: ([A-Z2-7]{24})`).FindStringSubmatch(h.stderr.String())
	if m == nil {
		t.Fatalf("no setup code printed:\n%s", h.stderr.String())
	}
	code := m[1]
	var ss api.SetupStatus
	h.call("GET", "/api/v1/setup", "", nil, &ss)
	if !ss.SetupNeeded {
		t.Fatal("setup not needed before setup")
	}
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/setup", "", api.SetupRequest{Code: "AAAAAAAAAAAAAAAAAAAAAAAA", Username: "admin", Password: "correct horse battery"}, &eb); st != http.StatusUnauthorized {
		t.Fatalf("wrong setup code: %d %+v", st, eb)
	}
	if st := h.call("POST", "/api/v1/setup", "", api.SetupRequest{Code: code, Username: "admin", Password: "short"}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("weak password: %d %+v", st, eb)
	}
	var u api.UserInfo
	if st := h.call("POST", "/api/v1/setup", "", api.SetupRequest{Code: code, Username: "admin", Password: "correct horse battery"}, &u); st != http.StatusCreated || u.Role != "admin" {
		t.Fatalf("setup: %d %+v", st, u)
	}
	if st := h.call("POST", "/api/v1/setup", "", api.SetupRequest{Code: code, Username: "other", Password: "correct horse battery"}, &eb); st != http.StatusConflict {
		t.Fatalf("second setup: %d %+v", st, eb)
	}
	h.call("GET", "/api/v1/setup", "", nil, &ss)
	if ss.SetupNeeded {
		t.Fatal("setup still needed")
	}

	login := newRequest(t, "POST", h.base+"/api/v1/login", api.LoginRequest{Username: "admin", Password: "correct horse battery"})
	resp, err := h.http.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	var lr api.LoginResult
	json.NewDecoder(resp.Body).Decode(&lr)
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if resp.StatusCode != http.StatusOK || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || lr.CSRFToken == "" {
		t.Fatalf("login: %d cookie %+v result %+v", resp.StatusCode, cookie, lr)
	}
	withCookie := func(method, path string, body any, csrf, origin string, out any) int {
		req := newRequest(t, method, h.base+path, body)
		req.AddCookie(cookie)
		if csrf != "" {
			req.Header.Set(auth.CSRFHeader, csrf)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return h.send(h.http, req, out)
	}
	if st := withCookie("GET", "/api/v1/instances", nil, "", "", nil); st != http.StatusOK {
		t.Fatalf("GET with the cookie: %d", st)
	}
	var si api.SessionInfo
	withCookie("GET", "/api/v1/session", nil, "", "", &si)
	if si.Kind != "user" || si.ID != "admin" || si.Role != "admin" || si.CSRFToken != lr.CSRFToken {
		t.Fatalf("session %+v", si)
	}
	body := api.TokenRequest{Name: "x", Role: "read"}
	if st := withCookie("POST", "/api/v1/tokens", body, "", "", &eb); st != http.StatusForbidden || eb.Error.Code != "csrf" {
		t.Fatalf("cookie POST without CSRF: %d %+v", st, eb)
	}
	if st := withCookie("POST", "/api/v1/tokens", body, lr.CSRFToken, "http://192.0.2.1:8740", &eb); st != http.StatusForbidden || eb.Error.Code != "origin" {
		t.Fatalf("cookie POST from another origin: %d %+v", st, eb)
	}
	// The same host from an https page is another origin on plain http.
	if st := withCookie("POST", "/api/v1/tokens", body, lr.CSRFToken, "https://"+strings.TrimPrefix(h.base, "http://"), &eb); st != http.StatusForbidden || eb.Error.Code != "origin" {
		t.Fatalf("cookie POST from the https origin of a plain listener: %d %+v", st, eb)
	}
	if st := withCookie("POST", "/api/v1/tokens", body, lr.CSRFToken, h.base, nil); st != http.StatusCreated {
		t.Fatalf("cookie POST with CSRF: %d", st)
	}
	if st := withCookie("POST", "/api/v1/logout", struct{}{}, lr.CSRFToken, "", nil); st != http.StatusOK {
		t.Fatalf("logout: %d", st)
	}
	if st := withCookie("GET", "/api/v1/instances", nil, "", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("GET after logout: %d", st)
	}
	// Too many failures from one client are refused for a while.
	var last int
	for i := 0; i < 6; i++ {
		last = h.call("POST", "/api/v1/login", "", api.LoginRequest{Username: "admin", Password: "wrong password!!"}, &eb)
	}
	if last != http.StatusTooManyRequests || eb.Error.Code != "rate_limited" {
		t.Fatalf("after 6 failures: %d %+v", last, eb)
	}
}

// 10: builds from a folder, a .zip and a .tar.gz; prune keeps what is used.
func TestBuilds(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	zipDir := testkit.MakeBuild(t, filepath.Join(h.src, "z"), testkit.BuildOptions{Version: "2.0"})
	testkit.MakeZip(t, zipDir, filepath.Join(h.src, "b2.zip"))
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: filepath.Join(h.src, "b2.zip")}, nil)
	tgzDir := testkit.MakeBuild(t, filepath.Join(h.src, "t"), testkit.BuildOptions{Version: "3.0"})
	testkit.MakeTarGz(t, tgzDir, filepath.Join(h.src, "b3.tar.gz"))
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: filepath.Join(h.src, "b3.tar.gz")}, nil)
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/builds", h.admin, api.InstallRequest{Source: filepath.Join(h.src, "b3.tar.gz")}, &eb); st != http.StatusConflict || eb.Error.Code != "build_exists" {
		t.Fatalf("installing twice: %d %+v", st, eb)
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 3 {
		t.Fatalf("builds %+v", bl)
	}
	h.create("alpha", "1.0", nil)
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "2.0"}, nil)
	if st := h.call("DELETE", "/api/v1/builds/2.0", h.admin, struct{}{}, &eb); st != http.StatusConflict || eb.Error.Code != "build_in_use" {
		t.Fatalf("delete of a used build: %d %+v", st, eb)
	}
	var removed api.Removed
	h.must(http.StatusOK, "POST", "/api/v1/builds/prune", struct{}{}, &removed)
	if !slices.Equal(removed.Removed, []string{"3.0"}) {
		t.Fatalf("pruned %v, want only 3.0", removed.Removed)
	}
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	got := map[string][]string{}
	for _, b := range bl.Builds {
		got[b.Version] = b.UsedBy
	}
	if len(got) != 2 || !slices.Equal(got["1.0"], []string{"alpha"}) || !slices.Equal(got["2.0"], []string{"alpha"}) {
		t.Fatalf("builds after prune: %v", got)
	}
	if st := h.call("POST", "/api/v1/builds", h.admin, api.InstallRequest{Source: "http://192.0.2.1/b.zip"}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("plain http source: %d %+v", st, eb)
	}
}

// 11: state changes arrive as server-sent events, and Last-Event-ID
// resumes.
func TestEvents(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	s := h.openEvents("?instance=alpha&types=state", h.admin, "")
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start", struct{}{}, nil)
	first := s.next(t, 10*time.Second, func(e events.Event) bool { return stateOf(e).State == instances.Starting })
	if first.Type != events.TypeState || first.Instance != "alpha" {
		t.Fatalf("event %+v", first)
	}
	s.next(t, 10*time.Second, func(e events.Event) bool { return stateOf(e).State == instances.Running })
	s.close()
	// Reconnect from the first event: the running event comes again.
	s2 := h.openEvents("?instance=alpha&types=state", h.admin, strconv.FormatUint(first.ID, 10))
	e := s2.next(t, 5*time.Second, func(e events.Event) bool { return true })
	if e.ID <= first.ID || stateOf(e).State != instances.Running {
		t.Fatalf("resumed with %+v after id %d, want the running event", e, first.ID)
	}
	// A read token never sees audit events.
	var tok api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "viewer", Role: "read"}, &tok)
	var eb api.ErrorBody
	if st := h.call("GET", "/api/v1/events?types=audit", tok.Token, nil, &eb); st != http.StatusForbidden {
		t.Fatalf("audit events for a read token: %d %+v", st, eb)
	}
}

// Error shape, unknown API paths, methods, the page and its headers.
func TestErrorsAndPage(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	var eb api.ErrorBody
	if st := h.call("GET", "/api/v1/nope", h.admin, nil, &eb); st != http.StatusNotFound || eb.Error.Code != "not_found" {
		t.Fatalf("unknown API path: %d %+v", st, eb)
	}
	if st := h.call("PUT", "/api/v1/instances", h.admin, nil, &eb); st != http.StatusMethodNotAllowed || eb.Error.Code != "method_not_allowed" {
		t.Fatalf("wrong method: %d %+v", st, eb)
	}
	if st := h.call("GET", "/api/v1/instances", "", nil, &eb); st != http.StatusUnauthorized || eb.Error.Code != "unauthorized" {
		t.Fatalf("no credentials: %d %+v", st, eb)
	}
	if st := h.call("GET", "/api/v1/instances", "esm_wrong", nil, &eb); st != http.StatusUnauthorized {
		t.Fatalf("bad token: %d %+v", st, eb)
	}
	if st := h.call("GET", "/api/v1/instances/ghost", h.admin, nil, &eb); st != http.StatusNotFound || eb.Error.Code != "not_found" {
		t.Fatalf("missing instance: %d %+v", st, eb)
	}
	if st := h.call("POST", "/api/v1/instances", h.admin, map[string]any{"name": "x", "bogus": 1}, &eb); st != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %+v", st, eb)
	}
	big := map[string]string{"name": strings.Repeat("x", 2<<20)}
	if st := h.call("POST", "/api/v1/instances", h.admin, big, &eb); st != http.StatusRequestEntityTooLarge || eb.Error.Code != "too_large" {
		t.Fatalf("large body: %d %+v", st, eb)
	}
	for _, p := range []string{"/", "/instances/alpha/logs"} {
		resp, err := h.http.Get(h.base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
			!strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("page %s: %d %v", p, resp.StatusCode, resp.Header)
		}
	}
	var doc struct {
		Checks []struct{ Name, Status string } `json:"checks"`
	}
	h.must(http.StatusOK, "GET", "/api/v1/box/doctor", nil, &doc)
	if len(doc.Checks) < 5 {
		t.Fatalf("doctor %+v", doc)
	}
}

// Secrets are stored and never shown, and no log has them; changes to a
// running instance wait for the next start.
func TestSecretsAndChanges(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	const pw = "hunter2-but-longer"
	var sr api.SecretsResult
	h.must(http.StatusOK, "PUT", "/api/v1/instances/alpha/secrets", map[string]string{"server_password": pw, "admin_password": pw + "!"}, &sr)
	if !sr.Secrets.ServerPasswordSet || !sr.Secrets.AdminPasswordSet || sr.RestartRequired {
		t.Fatalf("secrets result %+v", sr)
	}
	var raw json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/instances/alpha", nil, &raw)
	if strings.Contains(string(raw), pw) || !strings.Contains(string(raw), `"server_password_set":true`) {
		t.Fatalf("instance shows the password or misses the flag: %s", raw)
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
	var ch api.InstanceChange
	h.must(http.StatusOK, "PATCH", "/api/v1/instances/alpha", map[string]any{"max_players": 20, "cluster": "eu-1"}, &ch)
	if !ch.RestartRequired || !ch.Status.RestartRequired || ch.MaxPlayers != 20 {
		t.Fatalf("change while running: %+v", ch)
	}
	var cl api.ClusterList
	h.must(http.StatusOK, "GET", "/api/v1/clusters", nil, &cl)
	if len(cl.Clusters) != 1 || cl.Clusters[0].Name != "eu-1" || !slices.Equal(cl.Clusters[0].Instances, []string{"alpha"}) {
		t.Fatalf("clusters %+v", cl)
	}
	var eb api.ErrorBody
	if st := h.call("PATCH", "/api/v1/instances/alpha", h.admin, map[string]any{"build": "2.0"}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("build change by PATCH: %d %+v", st, eb)
	}
	// A password may not become part of a setting, nor be short; the
	// refusals do not repeat it.
	eb = api.ErrorBody{}
	if st := h.call("PATCH", "/api/v1/instances/alpha", h.admin, map[string]any{"display_name": "x " + strings.ToUpper(pw)}, &eb); st != http.StatusUnprocessableEntity ||
		eb.Error.Code != "password_in_settings" || strings.Contains(strings.ToLower(eb.Error.Message), pw) {
		t.Fatalf("password into display_name: %d %+v", st, eb)
	}
	eb = api.ErrorBody{}
	if st := h.call("PUT", "/api/v1/instances/alpha/secrets", h.admin, map[string]string{"admin_password": "Zq7-abc"}, &eb); st != http.StatusUnprocessableEntity ||
		eb.Error.Code != "password_too_short" || strings.Contains(eb.Error.Message, "Zq7") {
		t.Fatalf("short password: %d %+v", st, eb)
	}
	if st := h.call("PATCH", "/api/v1/instances/alpha", h.admin, map[string]any{"rules": map[string]string{"Port": "1"}}, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("reserved rule: %d %+v", st, eb)
	}
	if st := h.call("DELETE", "/api/v1/instances/alpha", h.admin, struct{}{}, &eb); st != http.StatusConflict || eb.Error.Code != "instance_running" {
		t.Fatalf("delete while running: %d %+v", st, eb)
	}
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/restart?wait=true", struct{}{}, &st)
	if st.State != instances.Running || st.RestartRequired {
		t.Fatalf("after restart: %+v", st)
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)
	// The server prints its command line into its own log and output. The
	// passwords are not on it: it names the secrets file.
	for _, source := range []string{"server", "console"} {
		var logs api.Logs
		h.must(http.StatusOK, "GET", "/api/v1/instances/alpha/logs?lines=500&source="+source, nil, &logs)
		text := strings.Join(logs.Lines, "\n")
		if strings.Contains(text, pw) || strings.Contains(strings.ToLower(text), "password") ||
			!strings.Contains(text, "Command line: ") || !strings.Contains(text, " "+gameserver.SecretsFileSwitch+"=") {
			t.Fatalf("the %s log shows a password or a password switch, or no command line:\n%s", source, text)
		}
	}
	if b, err := os.ReadFile(filepath.Join(h.root, "audit.log")); err != nil || strings.Contains(string(b), pw) {
		t.Fatalf("audit.log contains the password (%v)", err)
	}
	// ESM never writes a command line into its own log.
	if log := h.stderr.String(); strings.Contains(log, pw) || strings.Contains(log, "-Port=") || strings.Contains(log, gameserver.SecretsFileSwitch) {
		t.Fatalf("the service log shows the password or a launch line:\n%s", log)
	}

	h.must(http.StatusOK, "DELETE", "/api/v1/instances/alpha", api.DeleteRequest{PurgeBackups: true}, nil)
	if _, err := os.Stat(filepath.Join(h.root, "instances", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("instance folder after delete: %v", err)
	}
}

// The service refuses a non-loopback listen address without TLS.
func TestListenRuleRefused(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp("", "esm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.WriteFile(filepath.Join(root, "esm.json"), []byte(`{"listen":"192.0.2.1:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = service.Start(service.Options{Root: root, Stderr: &lockedBuffer{}})
	if err == nil || !strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("start with an exposed listen address: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "esm.sock")); !os.IsNotExist(err) {
		t.Fatal("a socket was left behind")
	}
}

// An adopted server whose ready signal cannot be seen again keeps running
// past the ready timeout.
func TestAdoptedServerIsNeverStopped(t *testing.T) {
	t.Parallel()
	h := newESM(t, func(cfg map[string]any) { cfg["ready_timeout"] = "1s" })
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	pid := st.PID
	h.stopService()
	if err := testkit.WriteControl(h.dataDir("alpha"), testkit.ControlUnready); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 5*time.Second, func() bool {
		s, err := gameserver.ReadStatus(h.dataDir("alpha"))
		return err == nil && s.State == gameserver.StateStarting
	}, "the fake did not drop its ready state")
	h.startService()
	st = h.status("alpha")
	if st.State != instances.Running || st.ReadySource != instances.ReadyAdopted || st.PID != pid {
		t.Fatalf("right after adoption: %+v", st)
	}
	time.Sleep(2 * time.Second) // twice the ready timeout
	st = h.status("alpha")
	if st.State != instances.Running || st.PID != pid || !strings.Contains(st.Reason, "no ready signal seen") {
		t.Fatalf("after the ready timeout: %+v", st)
	}
	before := h.liveCounter("alpha", "alpha")
	testkit.Eventually(t, 5*time.Second, func() bool { return h.liveCounter("alpha", "alpha") > before }, "the adopted server stopped saving")
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, &st)
	if st.State != instances.Stopped {
		t.Fatalf("stop of the adopted server: %+v", st)
	}
}
