package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// callAs sends a request over TCP with the bearer token, and the header
// that names the person it acts for when who is not empty. It returns the
// status and the code of an error answer; an event stream is answered with
// its status as soon as it opens and then closed.
func (h *harness) callAs(token, who, method, path string, body any) (int, string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req := newRequest(h.t, method, h.base+path, body).WithContext(ctx)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if who != "" {
		req.Header.Set(api.ActingUserHeader, who)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var eb api.ErrorBody
	json.Unmarshal(b, &eb)
	return resp.StatusCode, eb.Error.Code
}

// siteCase is what one route is called with and what a site token gets.
// allowed says that the role check lets the site role through (the answer
// is then the route's own, status and code); a refusal is 403 forbidden (or
// socket_only for the one call that only the unix socket answers).
type siteCase struct {
	allowed bool
	body    any
	status  int
	code    string
}

func refused() siteCase { return siteCase{status: http.StatusForbidden, code: "forbidden"} }

func answers(status int, code string) siteCase {
	return siteCase{allowed: true, status: status, code: code}
}

// The calls with no role to check (a token is not even read), and what each
// is called with to get a harmless answer.
var siteCases = map[string]siteCase{
	"GET /api/v1/setup":                                  answers(200, ""),
	"POST /api/v1/setup":                                 {allowed: true, body: api.SetupRequest{Code: "nope", Username: "web", Password: "a long password 1"}, status: 401, code: "bad_setup_code"},
	"POST /api/v1/login":                                 {allowed: true, body: api.LoginRequest{Username: "nobody", Password: "wrong"}, status: 401, code: "bad_credentials"},
	"POST /api/v1/logout":                                answers(200, ""),
	"GET /api/v1/session":                                answers(200, ""),
	"GET /api/v1/box":                                    answers(200, ""),
	"GET /api/v1/box/doctor":                             refused(),
	"GET /api/v1/box/firewall":                           answers(200, ""),
	"POST /api/v1/box/release-check":                     answers(200, ""),
	"POST /api/v1/box/self-update":                       {allowed: true, body: api.SelfUpdateRequest{}, status: 422, code: "update_failed"}, // no release is looked up in tests
	"GET /api/v1/steam":                                  answers(200, ""),
	"PUT /api/v1/steam/{name}/password":                  refused(),
	"GET /api/v1/builds":                                 answers(200, ""),
	"POST /api/v1/builds":                                {allowed: true, body: api.InstallRequest{Source: "steam:ghost"}, status: 422, code: "no_source"},
	"DELETE /api/v1/builds/{version}":                    refused(),
	"POST /api/v1/builds/prune":                          answers(200, ""),
	"GET /api/v1/instances":                              answers(200, ""),
	"POST /api/v1/instances":                             refused(),
	"GET /api/v1/instances/{name}":                       answers(404, "not_found"),
	"PATCH /api/v1/instances/{name}":                     {allowed: true, body: map[string]any{"display_name": "x"}, status: 404, code: "not_found"},
	"DELETE /api/v1/instances/{name}":                    refused(),
	"PUT /api/v1/instances/{name}/secrets":               refused(),
	"POST /api/v1/instances/{name}/start":                answers(404, "not_found"),
	"POST /api/v1/instances/{name}/stop":                 answers(404, "not_found"),
	"POST /api/v1/instances/{name}/restart":              answers(404, "not_found"),
	"POST /api/v1/instances/{name}/update":               {allowed: true, body: api.UpdateRequest{To: "1.0"}, status: 404, code: "not_found"},
	"POST /api/v1/instances/{name}/backup":               answers(404, "not_found"),
	"POST /api/v1/instances/{name}/restore":              {allowed: true, body: api.RestoreRequest{Backup: "b"}, status: 404, code: "not_found"},
	"GET /api/v1/jobs":                                   answers(200, ""),
	"GET /api/v1/jobs/{id}":                              answers(404, "not_found"),
	"DELETE /api/v1/jobs/{id}":                           answers(404, "not_found"),
	"GET /api/v1/instances/{name}/status":                answers(404, "not_found"),
	"GET /api/v1/instances/{name}/logs":                  answers(404, "not_found"),
	"GET /api/v1/instances/{name}/logs/download":         answers(404, "not_found"),
	"GET /api/v1/instances/{name}/backups":               answers(404, "not_found"),
	"GET /api/v1/instances/{name}/backups/{id}/download": answers(404, "not_found"),
	"GET /api/v1/events":                                 answers(200, ""),
	"GET /api/v1/clusters":                               answers(200, ""),
	"GET /api/v1/game-settings":                          answers(200, ""),
	"GET /api/v1/tokens":                                 refused(),
	"POST /api/v1/tokens":                                refused(),
	"DELETE /api/v1/tokens/{id}":                         refused(),
	"GET /api/v1/audit":                                  refused(),
	"GET /api/v1/users":                                  refused(),
	// A password is changed by a login's own session: a token, of any role,
	// is told so once it is through the role check.
	"POST /api/v1/session/password": {allowed: true, body: api.PasswordChangeRequest{CurrentPassword: "x", NewPassword: "a long password 1"}, status: 403, code: "session_only"},
	// Only the unix socket answers this one, for every token.
	"PUT /api/v1/users/{name}/password": {body: api.PasswordResetRequest{Password: "a long password 1"}, status: 403, code: "socket_only"},
}

// siteGate says whether a call got through the role check (and what it
// answered then), or was stopped by it.
func gateRefused(status int, code string) bool {
	return status == http.StatusForbidden && (code == "forbidden" || code == "socket_only")
}

func routePath(path string) string {
	return strings.NewReplacer("{name}", "ghost", "{version}", "9.9", "{id}", "tok_none").Replace(path)
}

// For every route there is, a site token gets exactly the answer a site
// token should: the operator's calls (the jobs, which it reads and, of an
// install from a Steam source, cancels, among them) and the four more that
// are open to it (changing an instance's settings, installing from a Steam
// source, pruning builds and updating ESM), and 403 on everything else. The
// table has a line for every route; a new route has to be put in it, which
// is the moment to decide what the site role does there. The operator and
// read tokens meet exactly the roles of the routes, as before.
func TestSiteRoleOnEveryRoute(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site, op, rd := h.token("site"), h.token("operator"), h.token("read")
	seen := map[string]bool{}
	var refusals int
	for _, rt := range api.Routes() {
		key := rt.Method + " " + rt.Path
		seen[key] = true
		c, ok := siteCases[key]
		if !ok {
			t.Errorf("no line in the table for %s: say what the site role does there", key)
			continue
		}
		status, code := h.callAs(site, "jane", rt.Method, routePath(rt.Path), c.body)
		if status != c.status || code != c.code || gateRefused(status, code) == c.allowed {
			t.Errorf("site token: %s: %d %q, want %d %q (role check passed: %v)", key, status, code, c.status, c.code, c.allowed)
		}
		if !gateRefused(status, code) {
			if rt.Role != "" && !rt.SocketOnly && !rt.Site {
				t.Errorf("%s: the site role got through, and Routes() says it does not", key)
			}
		} else {
			refusals++
		}
		if rt.Role != "" && !rt.SocketOnly && rt.Site != c.allowed {
			t.Errorf("%s: Routes() says site %v, the call says %v", key, rt.Site, c.allowed)
		}
		if rt.Role == "" && rt.Site {
			t.Errorf("%s: needs no login and has a site flag", key)
		}
		// Of the others, only the routes that need a login are called, and
		// what they get is by the roles as ever: admin routes are for the
		// admin role alone, whatever the site role may do.
		if rt.Role == "" {
			continue
		}
		for name, tok := range map[string]string{"operator": op, "read": rd} {
			status, code := h.callAs(tok, "", rt.Method, routePath(rt.Path), c.body)
			want := auth.Role(name).Allows(auth.Role(rt.Role)) && !rt.SocketOnly
			if gateRefused(status, code) == want {
				t.Errorf("%s token: %s: %d %q, role check should pass: %v", name, key, status, code, want)
			}
		}
	}
	for key := range siteCases {
		if !seen[key] {
			t.Errorf("the table has %s, which is no route", key)
		}
	}
	// Every refusal is in the audit log, with the person the token acted
	// for, and none of the allowed calls were made as somebody else.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=1000", nil, &al)
	var recorded int
	for _, e := range al.Entries {
		if e.Actor.Role == "site" && !e.OK && e.Actor.ActingUser == "jane" && e.Actor.Kind == "token" {
			recorded++
		}
		if e.Actor.Role != "site" && e.Actor.ActingUser != "" {
			t.Errorf("an acting user on a call of the %q role: %+v", e.Actor.Role, e)
		}
	}
	if refusals < 12 || recorded < refusals {
		t.Errorf("%d calls of the site token were refused, %d of them are in the audit log", refusals, recorded)
	}
}

// What a panel does with a real server, as a site token: look at it,
// change its settings, start it, back it up and download the backup, read
// its log, update it to an installed build, stop it, and prune the builds
// nothing uses; every change is in the audit log for the person it was
// made for. And what it may not do stays refused.
func TestSiteTokenRunsAServer(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.install(testkit.BuildOptions{Version: "1.1"})
	h.install(testkit.BuildOptions{Version: "0.9"})
	h.create("alpha", "1.0", fastRules(nil))

	as := func(who, method, path string, body, out any) int {
		t.Helper()
		req := newRequest(t, method, h.base+path, body)
		req.Header.Set("Authorization", "Bearer "+site)
		req.Header.Set(api.ActingUserHeader, who)
		return h.send(h.http, req, out)
	}
	// Its event stream goes on while the login (a token of the site role)
	// holds, and has no audit events: those are the admin's.
	stream := h.openEvents("", site, "")
	var iv api.InstanceChange
	if st := as("jane", "PATCH", "/api/v1/instances/alpha", map[string]any{"display_name": "Alpha Realm", "auto_update": "off",
		"max_players": 12, "rules": fastRules(nil)}, &iv); st != http.StatusOK || iv.DisplayName != "Alpha Realm" || iv.MaxPlayers != 12 {
		t.Fatalf("change settings: %d %+v", st, iv.View)
	}
	var stt instances.Status
	if st := as("jane", "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &stt); st != http.StatusOK || stt.State != instances.Running {
		t.Fatalf("start: %d %+v", st, stt)
	}
	stream.next(t, 10*time.Second, func(e events.Event) bool {
		if e.Type == events.TypeAudit {
			t.Errorf("an audit event for a site token: %+v", e)
		}
		return e.Type == events.TypeState && e.Instance == "alpha" && stateOf(e).State == instances.Running
	})
	if st := as("joe", "GET", "/api/v1/instances/alpha/status", nil, &stt); st != http.StatusOK || stt.State != instances.Running {
		t.Fatalf("status: %d %+v", st, stt)
	}
	if st, _ := h.callAs(site, "", "GET", "/api/v1/events?types=audit", nil); st != http.StatusForbidden {
		t.Fatalf("audit events for a site token: %d", st)
	}
	var logs api.Logs
	if st := as("joe", "GET", "/api/v1/instances/alpha/logs?lines=5", nil, &logs); st != http.StatusOK || len(logs.Lines) == 0 {
		t.Fatalf("logs: %d %+v", st, logs)
	}
	var bk instances.Backup
	if st := as("jane", "POST", "/api/v1/instances/alpha/backup", struct{}{}, &bk); st != http.StatusCreated || bk.ID == "" {
		t.Fatalf("backup: %d %+v", st, bk)
	}
	req := newRequest(t, "GET", h.base+"/api/v1/instances/alpha/backups/"+bk.ID+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+site)
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || n == 0 || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("download: %d, %d bytes, %s", resp.StatusCode, n, resp.Header.Get("Content-Type"))
	}
	if st := as("jane", "POST", "/api/v1/instances/alpha/stop", struct{}{}, &stt); st != http.StatusOK {
		t.Fatalf("stop: %d", st)
	}
	if st := as("jane", "POST", "/api/v1/instances/alpha/restore", api.RestoreRequest{Backup: bk.ID}, &stt); st != http.StatusOK {
		t.Fatalf("restore: %d", st)
	}
	if st := as("jane", "POST", "/api/v1/instances/alpha/update", api.UpdateRequest{To: "1.1"}, &stt); st != http.StatusOK || stt.Build != "1.1" {
		t.Fatalf("update: %d %+v", st, stt)
	}
	var rm api.Removed
	if st := as("jane", "POST", "/api/v1/builds/prune", struct{}{}, &rm); st != http.StatusOK || len(rm.Removed) == 0 {
		t.Fatalf("prune: %d %+v", st, rm)
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) < 1 || bl.Builds[0].Version != "1.1" {
		t.Fatalf("builds after the prune: %+v", bl)
	}
	for _, b := range bl.Builds {
		if b.Version == "0.9" {
			t.Fatalf("an unused build is still there: %+v", bl)
		}
	}

	// What it may not: the world is still as it was.
	for _, c := range []struct{ method, path string }{
		{"DELETE", "/api/v1/instances/alpha"},
		{"DELETE", "/api/v1/builds/1.1"},
		{"POST", "/api/v1/instances"},
		{"PUT", "/api/v1/instances/alpha/secrets"},
	} {
		if st := as("jane", c.method, c.path, map[string]any{"name": "beta", "server_password": "a password 1"}, nil); st != http.StatusForbidden {
			t.Errorf("%s %s: %d", c.method, c.path, st)
		}
	}
	var list api.InstanceList
	h.must(http.StatusOK, "GET", "/api/v1/instances", nil, &list)
	if len(list.Instances) != 1 || list.Instances[0].Name != "alpha" || list.Instances[0].DisplayName != "Alpha Realm" {
		t.Fatalf("instances: %+v", list.Instances)
	}

	// Each call is in the audit log, for the person it names.
	var al api.AuditList
	h.must(http.StatusOK, "GET", "/api/v1/audit?limit=1000", nil, &al)
	got := map[string]string{}
	for _, e := range al.Entries {
		if e.Actor.Role == "site" && e.OK {
			got[e.Action] = e.Actor.ActingUser
		}
	}
	for action, who := range map[string]string{"instance.change": "jane", "instance.start": "jane", "instance.backup": "jane", "instance.stop": "jane",
		"instance.restore": "jane", "instance.update": "jane", "build.prune": "jane"} {
		if got[action] != who {
			t.Errorf("audit: %s was for %q, want %q (%v)", action, got[action], who, got)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(h.root, "audit.log")); strings.Contains(string(log), site) {
		t.Error("the token is in the audit log")
	}
}

// A site token installs a build from a Steam source of esm.json, and from
// nothing else: a folder in the import folder, an archive and an address
// are refused with 403 and install nothing, while an admin still installs
// them.
func TestSiteTokenInstallsOnlyFromSteam(t *testing.T) {
	steamDir := t.TempDir()
	content := filepath.Join(t.TempDir(), "content")
	exes := testkit.MakeContent(t, content, testkit.BuildOptions{Version: "x"})
	ctl := testkit.SteamControl{AppID: "1000001", BuildID: "100", Content: content, Branch: "testing", Record: filepath.Join(steamDir, "record.jsonl")}
	exe := testkit.FakeSteamCMD(t, steamDir, ctl)
	h := newESM(t, func(cfg map[string]any) {
		cfg["steamcmd"] = exe
		cfg["steam_sources"] = []map[string]any{{"name": "main", "app_id": "1000001", "branch": "testing", "executable": exes,
			"default_map": testkit.DefaultMap, "channel": "live", "check_every": "0"}}
	})
	site := h.token("site")
	dir := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "7.7"})

	var b api.BuildView
	if st := h.call("POST", "/api/v1/builds", site, api.InstallRequest{Source: "steam:main"}, &b); st != http.StatusCreated || b.Version != "steam-main-100" {
		t.Fatalf("Steam install by a site token: %d %+v", st, b)
	}
	if st := h.call("POST", "/api/v1/builds", site, api.InstallRequest{Source: " steam:main"}, &b); st != http.StatusOK || b.Version != "steam-main-100" {
		t.Fatalf("the same build id again: %d %+v", st, b)
	}
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/builds", site, api.InstallRequest{Source: "steam:nope"}, &eb); st != http.StatusUnprocessableEntity || eb.Error.Code != "no_source" {
		t.Fatalf("a Steam source that is not in esm.json: %d %+v", st, eb)
	}
	for _, src := range []string{dir, filepath.Base(dir), dir + ".tar.gz", "https://example.invalid/build.tar.gz", "STEAM:main"} {
		eb = api.ErrorBody{}
		if st := h.call("POST", "/api/v1/builds", site, api.InstallRequest{Source: src, SHA256: strings.Repeat("0", 64)}, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" {
			t.Errorf("site token installs %q: %d %+v", src, st, eb)
		}
	}
	var bl api.BuildList
	h.must(http.StatusOK, "GET", "/api/v1/builds", nil, &bl)
	if len(bl.Builds) != 1 || bl.Builds[0].Version != "steam-main-100" {
		t.Fatalf("builds after the refusals: %+v", bl)
	}
	// An admin still does.
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, &b)
	if b.Version != "7.7" {
		t.Fatalf("admin install: %+v", b)
	}
}

// A site token may look for a release and update ESM, but never with
// exit_anyway, however ESM is run: nothing is changed by the refusal.
func TestSiteTokenCannotMakeESMExit(t *testing.T) {
	exe, tune := releaseServer(t, "")
	h := newESMTuned(t, "process", tune)
	site := h.token("site")
	var eb api.ErrorBody
	if st := h.call("POST", "/api/v1/box/self-update", site, api.SelfUpdateRequest{Force: true, ExitAnyway: true}, &eb); st != http.StatusForbidden || eb.Error.Code != "forbidden" ||
		!strings.Contains(eb.Error.Message, "exit_anyway") {
		t.Fatalf("exit_anyway by a site token: %d %+v", st, eb)
	}
	// Without it, with nothing to start ESM again, the update is refused as
	// for an admin.
	if st := h.call("POST", "/api/v1/box/self-update", site, api.SelfUpdateRequest{Force: true}, &eb); st != http.StatusConflict || eb.Error.Code != "no_service_manager" {
		t.Fatalf("a site token without exit_anyway and no service manager: %d %+v", st, eb)
	}
	if b, _ := os.ReadFile(exe); string(b) != "the old esm" {
		t.Fatalf("binary changed to %q", b)
	}
	select {
	case <-h.svc.Exit():
		t.Fatal("the service was asked to end")
	case <-time.After(1500 * time.Millisecond):
	}
}

// esm token create --role site makes a token that works as one: the
// answer says its role, it reads and runs, and it makes no tokens. A role
// that is none is refused with the list of the four.
func TestCLITokenCreateSite(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	code, out, errOut := h.runCLI("token", "create", "--role", "site", "--name", "panel")
	line := oneLine(t, "token create --role site", out)
	if code != 0 || !strings.HasPrefix(line, "esm_") || !strings.Contains(line, "role site;") || errOut != "" {
		t.Fatalf("token create: %d %q %q", code, out, errOut)
	}
	tok := strings.Fields(line)[0]
	code, out, errOut = h.runCLI("--json", "token", "create", "--role", "site")
	var nt api.NewToken
	if code != 0 || json.Unmarshal([]byte(oneLine(t, "--json", out)), &nt) != nil || nt.Role != "site" || nt.Name != "" || !strings.HasPrefix(nt.Token, "esm_") {
		t.Fatalf("token create --json: %d %q %q", code, out, errOut)
	}
	var list api.TokenList
	h.must(http.StatusOK, "GET", "/api/v1/tokens", nil, &list)
	var roles []string
	for _, tv := range list.Tokens {
		roles = append(roles, tv.Name+":"+tv.Role)
	}
	if !strings.Contains(strings.Join(roles, " "), "panel:site") {
		t.Fatalf("tokens: %v", roles)
	}
	// The token works over --url, as an operator's does.
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "list")
	if code != 0 || !strings.HasPrefix(oneLine(t, "list", out), "1 instance: alpha stopped") {
		t.Fatalf("list with the site token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "start", "alpha")
	if code != 0 || !strings.HasPrefix(oneLine(t, "start", out), "alpha running") {
		t.Fatalf("start with the site token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "stop", "alpha")
	if code != 0 {
		t.Fatalf("stop with the site token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "token", "create", "--role", "read")
	if code != 1 || out != "" || !strings.Contains(errOut, "admin") {
		t.Fatalf("token create with the site token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("token", "create", "--role", "superuser")
	if code != 1 || out != "" || !strings.Contains(errOut, "read, operator, admin or site") {
		t.Fatalf("token create --role superuser: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("token", "create")
	if code == 0 || out != "" || !strings.Contains(errOut, "read, operator, admin or site") {
		t.Fatalf("token create without a role: %d %q %q", code, out, errOut)
	}
}

// A site token changes what a panel needs of a server (its name, players,
// rules, update and backup settings, the schedule and the like) and
// nothing that reaches the server's command line or its host: the call
// that names such a field, whatever the value, is refused with 403 that
// names it, and changes nothing, not even the fields beside it. An admin
// still changes them.
func TestSiteTokenChangesOnlyWhatAPanelNeeds(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	site := h.token("site")
	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "1.0", fastRules(nil))
	const path = "/api/v1/instances/alpha"
	raw := func() string {
		var b json.RawMessage
		h.must(http.StatusOK, "GET", path, nil, &b)
		return string(b)
	}

	for _, body := range []map[string]any{
		{"display_name": "Alpha Realm"}, {"max_players": 15}, {"listed": false}, {"rules": fastRules(map[string]string{"Banner": "Hello"})},
		{"auto_update": "install"}, {"update_max_wait": "2h"}, {"autostart": true}, {"restart_on_crash": false},
		{"schedule": map[string]any{"restart_at": []string{"04:00"}}}, {"backup": map[string]any{"every": "1h", "keep": 5}},
		{"map": testkit.DefaultMap}, {"world": "alpha2"}, {"cluster": "main"}, {"channel": "live"},
	} {
		var out api.InstanceChange
		if st := h.call("PATCH", path, site, body, &out); st != http.StatusOK {
			t.Errorf("site token changes %v: %d", body, st)
		}
	}
	var v instances.View
	h.must(http.StatusOK, "GET", path, nil, &v)
	if v.DisplayName != "Alpha Realm" || v.MaxPlayers != 15 || v.Listed || v.Rules["Banner"] != "Hello" || v.AutoUpdate != "install" ||
		v.UpdateMaxWait.D() != 2*time.Hour || !v.Autostart || v.RestartOnCrash || len(v.Schedule.RestartAt) != 1 || v.Backup.Keep != 5 ||
		v.Backup.Every.D() != time.Hour || v.Map != testkit.DefaultMap || v.World != "alpha2" || v.Cluster != "main" || v.Channel != "live" {
		t.Fatalf("after the changes: %+v", v)
	}

	before := raw()
	for _, body := range []map[string]any{
		{"extra_args": []string{"-Verbose"}}, {"limits": map[string]any{"memory_max": "4G"}}, {"bind_ip": "127.0.0.1"},
		{"ports": map[string]any{"game": 30500, "query": 31500}}, {"secret_delivery": "command_line"}, {"build": "1.0"},
		{"extra_args": nil}, {"display_name": "Hijacked", "extra_args": []string{"-Verbose"}},
	} {
		var eb api.ErrorBody
		st := h.call("PATCH", path, site, body, &eb)
		var field string
		for k := range body {
			if k != "display_name" {
				field = k
			}
		}
		if st != http.StatusForbidden || eb.Error.Code != "forbidden" || !strings.Contains(eb.Error.Message, "may not change "+field) ||
			strings.Contains(eb.Error.Message, "may not change display_name") {
			t.Errorf("site token changes %v: %d %+v", body, st, eb)
		}
	}
	if after := raw(); after != before {
		t.Fatalf("a refused change changed the instance:\n%s\n%s", before, after)
	}

	// An admin is not limited.
	var out api.InstanceChange
	h.must(http.StatusOK, "PATCH", path, map[string]any{"extra_args": []string{"-Verbose"}, "bind_ip": "127.0.0.1",
		"limits": map[string]any{"memory_max": "4G"}}, &out)
	if len(out.ExtraArgs) != 1 || out.BindIP != "127.0.0.1" || out.Limits.MemoryMax != "4G" {
		t.Fatalf("admin change: %+v", out.View)
	}
}
