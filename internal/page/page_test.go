package page_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

var portBlock atomic.Int32

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// site is a running ESM, as a browser sees it.
type site struct {
	t      *testing.T
	root   string
	base   string
	svc    *service.Service
	stderr *lockedBuffer
	http   *http.Client // follows no redirects
	// After setup: the admin's session cookie and CSRF token, and tokens.
	session string
	csrf    string
	admin   string
}

func newSite(t *testing.T) *site {
	t.Helper()
	root, err := os.MkdirTemp("", "esmp")
	if err != nil {
		t.Fatal(err)
	}
	blk := int(portBlock.Add(1))
	cfg := map[string]any{
		"listen":           "127.0.0.1:0",
		"game_ports":       fmt.Sprintf("%d-%d", 33000+blk*20, 33009+blk*20),
		"query_ports":      fmt.Sprintf("%d-%d", 34000+blk*20, 34009+blk*20),
		"supervisor":       "process",
		"stop_timeout":     "5s",
		"ready_timeout":    "10s",
		"backup_save_wait": "3s",
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(root+string(os.PathSeparator)+"esm.json", b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, root: root, stderr: &lockedBuffer{},
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	s.svc, err = service.Start(service.Options{
		Root: root, Version: "test", Stderr: s.stderr,
		WatchInterval: 50 * time.Millisecond, SchedulerInterval: time.Hour, KeepAlive: 200 * time.Millisecond,
		PortHost: "127.0.0.1", AuthOptions: auth.Options{Iterations: 1000}, FailDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s.base = "http://" + s.svc.Addr()
	t.Cleanup(func() {
		if s.admin != "" {
			var list api.InstanceList
			if s.api("GET", "/api/v1/instances", s.admin, nil, &list) == http.StatusOK {
				for _, v := range list.Instances {
					s.api("POST", "/api/v1/instances/"+v.Name+"/stop", s.admin, struct{}{}, nil)
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		s.svc.Shutdown(ctx)
		os.RemoveAll(root)
	})
	return s
}

// api calls the API with a bearer token, or with the session cookie and
// CSRF token when token is "session".
func (s *site) api(method, path, token string, body, out any) int {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, r)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch token {
	case "":
	case "session":
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.session})
		req.Header.Set(auth.CSRFHeader, s.csrf)
	default:
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			s.t.Fatalf("%s %s: %q: %v", method, path, b, err)
		}
	}
	if login := resp.Cookies(); path == "/api/v1/login" && len(login) > 0 {
		s.session = login[0].Value
	}
	return resp.StatusCode
}

func (s *site) must(want int, method, path, token string, body, out any) {
	s.t.Helper()
	var raw json.RawMessage
	if st := s.api(method, path, token, body, &raw); st != want {
		s.t.Fatalf("%s %s: %d, want %d: %s", method, path, st, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			s.t.Fatal(err)
		}
	}
}

var setupCodeRe = regexp.MustCompile(`ESM setup code: (\S+)`)

// setup makes the admin with the printed setup code, logs in and makes an
// admin token.
func (s *site) setup() {
	s.t.Helper()
	m := setupCodeRe.FindStringSubmatch(s.stderr.String())
	if m == nil {
		s.t.Fatalf("no setup code in %q", s.stderr)
	}
	s.must(http.StatusCreated, "POST", "/api/v1/setup", "", api.SetupRequest{Code: m[1], Username: "admin", Password: "a long password 1"}, nil)
	var lr api.LoginResult
	s.must(http.StatusOK, "POST", "/api/v1/login", "", api.LoginRequest{Username: "admin", Password: "a long password 1"}, &lr)
	s.csrf = lr.CSRFToken
	if s.session == "" || s.csrf == "" || s.session == s.csrf {
		s.t.Fatal("no session")
	}
	s.admin = s.token("admin")
}

func (s *site) token(role string) string {
	s.t.Helper()
	var tok api.NewToken
	s.must(http.StatusCreated, "POST", "/api/v1/tokens", "session", api.TokenRequest{Name: role + " page test", Role: role}, &tok)
	return tok.Token
}

// get fetches a page as a browser would: with the session cookie when who
// is "session", a bearer token otherwise, nothing when who is "".
func (s *site) get(path, who string) (*http.Response, string) {
	s.t.Helper()
	return s.do("GET", path, who)
}

func (s *site) do(method, path, who string) (*http.Response, string) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	switch who {
	case "":
	case "session":
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: s.session})
	default:
		req.Header.Set("Authorization", "Bearer "+who)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// checkHeaders fails unless the reply carries every security header.
func checkHeaders(t *testing.T, what string, h http.Header) {
	t.Helper()
	if h.Get("Content-Security-Policy") != web.CSP || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("%s: security headers: %v", what, h)
	}
}

var (
	styleAttrRe   = regexp.MustCompile(`(?i)\sstyle\s*=`)
	handlerAttrRe = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	scriptRe      = regexp.MustCompile(`(?i)<script[^>]*>`)
)

const theScript = `<script src="/static/esm.js" defer>`

// checkHTML checks a page as HTML: no inline script or style anywhere, no
// style or event handler attributes, nothing from another origin, every
// form control labelled, every table header with a scope, every action a
// real button, every link with a target. csrf is what the CSRF meta tag
// must hold; secrets must not appear.
func checkHTML(t *testing.T, what, body, csrf string, secrets ...string) {
	t.Helper()
	if s := scriptRe.FindAllString(body, -1); len(s) != 1 || s[0] != theScript || !strings.Contains(body, theScript+"</script>") {
		t.Errorf("%s: scripts %q", what, s)
	}
	if strings.Contains(strings.ToLower(body), "<style") || styleAttrRe.MatchString(body) {
		t.Errorf("%s: inline style", what)
	}
	if m := handlerAttrRe.FindString(body); m != "" {
		t.Errorf("%s: event handler attribute %q", what, m)
	}
	if strings.Contains(strings.ToLower(body), "javascript:") {
		t.Errorf("%s: javascript: link", what)
	}
	if !strings.Contains(body, `<meta name="csrf-token" content="`+csrf+`">`) {
		t.Errorf("%s: the CSRF meta tag does not hold %q", what, csrf)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(body, s) {
			t.Errorf("%s: shows a secret (%d characters)", what, len(s))
		}
	}
	for _, leak := range []string{"{{", "}}", "ZgotmplZ"} {
		if strings.Contains(body, leak) {
			t.Errorf("%s: template text or a refused value %q", what, leak)
		}
	}

	d := xml.NewDecoder(strings.NewReader(body))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	type control struct{ tag, id, name string }
	var unlabelled []control
	labelFor := map[string]bool{}
	ids := map[string]bool{}
	inLabel := 0
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Errorf("%s: not well-formed: %v", what, err)
			return
		}
		switch e := tok.(type) {
		case xml.StartElement:
			a := map[string]string{}
			for _, at := range e.Attr {
				a[strings.ToLower(at.Name.Local)] = at.Value
			}
			tag := strings.ToLower(e.Name.Local)
			if id, ok := a["id"]; ok {
				if ids[id] {
					t.Errorf("%s: id %q twice", what, id)
				}
				ids[id] = true
			}
			for _, k := range []string{"href", "src", "action"} {
				if v := a[k]; strings.HasPrefix(v, "//") || strings.Contains(v, "://") {
					t.Errorf("%s: <%s %s=%q> leaves the site", what, tag, k, v)
				}
			}
			if a["role"] == "button" {
				t.Errorf("%s: role=button on <%s>", what, tag)
			}
			if _, ok := a["data-act"]; ok && tag != "button" {
				t.Errorf("%s: an action on <%s>", what, tag)
			}
			switch tag {
			case "label":
				inLabel++
				if f := a["for"]; f != "" {
					labelFor[f] = true
				}
			case "input", "select", "textarea":
				if a["type"] == "hidden" {
					break
				}
				if inLabel == 0 && a["aria-label"] == "" && a["aria-labelledby"] == "" {
					unlabelled = append(unlabelled, control{tag, a["id"], a["name"]})
				}
			case "th":
				if s := a["scope"]; s != "col" && s != "row" {
					t.Errorf("%s: <th> without scope", what)
				}
			case "button":
				if a["type"] != "button" && a["type"] != "submit" {
					t.Errorf("%s: <button> without type", what)
				}
			case "a":
				if a["href"] == "" {
					t.Errorf("%s: <a> without href", what)
				}
			}
		case xml.EndElement:
			if strings.ToLower(e.Name.Local) == "label" {
				inLabel--
			}
		}
	}
	for _, c := range unlabelled {
		if c.id == "" || !labelFor[c.id] {
			t.Errorf("%s: <%s name=%q id=%q> has no label", what, c.tag, c.name, c.id)
		}
	}
}

// checkPage fetches a page and checks it as HTML with its headers.
func (s *site) checkPage(path, who, csrf string, secrets ...string) string {
	s.t.Helper()
	resp, body := s.get(path, who)
	what := path + " as " + map[bool]string{true: "session", false: "token"}[who == "session"]
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-store" {
		s.t.Fatalf("%s: %d %v\n%s", what, resp.StatusCode, resp.Header, body)
	}
	checkHeaders(s.t, what, resp.Header)
	checkHTML(s.t, what, body, csrf, secrets...)
	return body
}

// screens are every screen of the page for two servers.
func screens() []string {
	out := []string{"/", "/servers", "/builds", "/clusters", "/box", "/box?check=1", "/audit"}
	for _, n := range []string{"alpha", "beta"} {
		b := "/servers/" + n
		out = append(out, b, b+"/settings", b+"/log", b+"/log?source=console", b+"/players", b+"/backups", b+"/schedule")
	}
	return out
}

// A new box: every screen leads to the setup form; after setup, to the
// login. Then every screen renders for every role, holds no inline code,
// labels its controls, and never shows the session id or a password.
func TestEveryScreen(t *testing.T) {
	s := newSite(t)

	// Before setup.
	for _, p := range append(screens(), "/servers/ghost") {
		resp, _ := s.get(p, "")
		want := "/setup?next=" + url.QueryEscape(strings.SplitN(p, "?", 2)[0])
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
			t.Errorf("%s without a login: %d to %q, want %q", p, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		checkHeaders(t, p+" redirect", resp.Header)
	}
	body := s.checkPage("/setup?next=%2Fbuilds", "", "")
	if !strings.Contains(body, `data-form="setup"`) || !strings.Contains(body, `data-next="/builds"`) || strings.Contains(body, `data-form="login"`) {
		t.Fatalf("setup page: %s", body)
	}

	s.setup()
	body = s.checkPage("/setup?next=//elsewhere.example", "", "")
	if !strings.Contains(body, `data-form="login"`) || !strings.Contains(body, `data-next="/"`) || strings.Contains(body, `data-form="setup"`) {
		t.Fatalf("login page: %s", body)
	}
	for next, want := range map[string]string{"": "/", "%2Fbuilds": "/builds", "%2F%2Fevil.example": "/", "%2Fapi%2Fv1%2Fbox": "/", "https%3A%2F%2Fx.example": "/"} {
		resp, _ := s.get("/setup?next="+next, "session")
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != want {
			t.Errorf("logged in, next %q: %d to %q", next, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// Two servers: alpha in the default mode, beta with its passwords on
	// the command line; both run, alpha has a backup.
	// Local builds come from the import folder of the root.
	src := s.root + string(os.PathSeparator) + "imports"
	for _, v := range []string{"1.0", "1.1"} {
		dir := testkit.MakeBuild(t, src, testkit.BuildOptions{Version: v})
		s.must(http.StatusCreated, "POST", "/api/v1/builds", s.admin, api.InstallRequest{Source: dir}, nil)
	}
	fast := map[string]string{"FakeReadyDelay": "100ms", "FakeSaveInterval": "200ms", "Banner": "<b>hard</b>"}
	s.must(http.StatusCreated, "POST", "/api/v1/instances", s.admin, map[string]any{"name": "alpha", "display_name": "Alpha <Wilds>",
		"bind_ip": "127.0.0.1", "build": "1.0", "cluster": "north", "rules": fast, "limits": map[string]any{"memory_max": "1G"},
		"schedule": map[string]any{"restart_at": []string{"04:00"}}}, nil)
	s.must(http.StatusCreated, "POST", "/api/v1/instances", s.admin, map[string]any{"name": "beta", "bind_ip": "127.0.0.1",
		"build": "1.0", "rules": fast, "secret_delivery": "command_line"}, nil)
	const alphaPass, betaPass, betaAdmin = "alpha-join-secret", "beta-join-secret", "beta-admin-secret"
	s.must(http.StatusOK, "PUT", "/api/v1/instances/alpha/secrets", s.admin, map[string]string{"server_password": alphaPass}, nil)
	s.must(http.StatusOK, "PUT", "/api/v1/instances/beta/secrets", s.admin, map[string]string{"server_password": betaPass, "admin_password": betaAdmin}, nil)
	for _, n := range []string{"alpha", "beta"} {
		s.must(http.StatusOK, "POST", "/api/v1/instances/"+n+"/start?wait=true", s.admin, struct{}{}, nil)
	}
	var backup instances.Backup
	s.must(http.StatusCreated, "POST", "/api/v1/instances/alpha/backup", s.admin, struct{}{}, &backup)
	read, oper := s.token("read"), s.token("operator")
	secrets := []string{s.session, alphaPass, betaPass, betaAdmin, s.admin, read, oper}

	// Every screen, as the admin's browser session and with each token.
	pages := map[string]map[string]string{}
	for _, who := range []string{"session", oper, read} {
		csrf := ""
		if who == "session" {
			csrf = s.csrf
		}
		pages[who] = map[string]string{}
		for _, p := range screens() {
			pages[who][p] = s.checkPage(p, who, csrf, secrets...)
		}
	}
	admin, op, rd := pages["session"], pages[oper], pages[read]

	// What each role is offered.
	if !strings.Contains(admin["/"], `data-act="stop" data-name="alpha"`) || !strings.Contains(op["/"], `data-act="stop" data-name="alpha"`) ||
		strings.Contains(rd["/"], `data-act="stop"`) || strings.Contains(rd["/"], `data-act="start"`) || strings.Contains(rd["/servers/alpha"], `data-act="backup"`) {
		t.Error("lifecycle buttons: admin and operator get them, read does not")
	}
	if !strings.Contains(admin["/"], "data-activity") || strings.Contains(rd["/"], "data-activity") || !strings.Contains(rd["/"], "needs the admin role") {
		t.Error("recent activity is for admins only")
	}
	if !strings.Contains(admin["/box"], `data-act="revoke"`) || !strings.Contains(rd["/box"], "need the admin role") || strings.Contains(op["/box"], `data-form="token"`) {
		t.Error("tokens are for admins only")
	}
	if !strings.Contains(admin["/audit"], "instance.backup") || !strings.Contains(rd["/audit"], "needs the admin role") {
		t.Error("the audit log is for admins only")
	}
	dl := "/api/v1/instances/alpha/backups/" + backup.ID + "/download"
	if !strings.Contains(op["/servers/alpha/backups"], dl) || strings.Contains(rd["/servers/alpha/backups"], dl) || !strings.Contains(rd["/servers/alpha/backups"], "Made by hand") {
		t.Error("backup downloads are for operators; read still sees the list")
	}
	if strings.Contains(op["/servers/alpha/settings"], `data-save="later"`) || !strings.Contains(admin["/servers/alpha/settings"], `data-save="later"`) ||
		!strings.Contains(op["/servers/alpha/settings"], "needs the admin role") {
		t.Error("saving settings is for admins only")
	}

	// Passwords: never a value, only whether one is set; unsafe mode warned.
	if !strings.Contains(admin["/servers/alpha/settings"], `name="server_password" maxlength="128" autocomplete="new-password" value=""`) ||
		!strings.Contains(admin["/servers/alpha/settings"], "Join password (set)") || !strings.Contains(admin["/servers/alpha/settings"], "Admin password (none)") {
		t.Error("password fields")
	}
	for _, who := range []string{"session", oper, read} {
		if !strings.Contains(pages[who]["/servers/beta"], "UNSAFE on a shared box") || !strings.Contains(pages[who]["/"], "UNSAFE: passwords on the command line") ||
			!strings.Contains(pages[who]["/box"], "UNSAFE on a shared box: beta") {
			t.Error("the UNSAFE warning of a command-line instance")
		}
	}

	// Server output: shown to operators and admins, never to read for the
	// command-line instance (the API's notice instead).
	notice := html.EscapeString(instances.HiddenNotice)
	for _, p := range []string{"/servers/beta", "/servers/beta/log", "/servers/beta/log?source=console"} {
		if !strings.Contains(rd[p], notice) || strings.Contains(rd[p], "data-log-of") || strings.Contains(rd[p], "logs/download") {
			t.Errorf("read, %s: no notice, or output shown", p)
		}
		if strings.Contains(admin[p], notice) || !strings.Contains(admin[p], `data-log-of="beta"`) {
			t.Errorf("admin, %s: output not shown", p)
		}
	}
	if !strings.Contains(rd["/servers/alpha/log"], `data-log-of="alpha"`) || !strings.Contains(rd["/servers/alpha/log"], "/api/v1/instances/alpha/logs/download?source=server") {
		t.Error("read sees the output of an instance in the default mode")
	}
	if !strings.Contains(admin["/servers/beta/log?source=console"], gameserverLine) {
		t.Errorf("the console of beta holds no server line")
	}

	// Values the API returned are escaped, the live facts and the next
	// scheduled restart are shown.
	if !strings.Contains(admin["/"], "Alpha &lt;Wilds&gt;") || !strings.Contains(admin["/servers/alpha/settings"], `value="&lt;b&gt;hard&lt;/b&gt;"`) {
		t.Error("escaping")
	}
	if !strings.Contains(admin["/servers/alpha/schedule"], "Next timed restart") || strings.Contains(admin["/servers/alpha/schedule"], "<dd class=\"mono\">none</dd>") {
		t.Error("the next timed restart")
	}
	if !strings.Contains(admin["/servers/alpha"], "/ 1.0 GiB") {
		t.Error("the memory ceiling")
	}
	if !strings.Contains(admin["/box"], "sudo ufw allow") {
		t.Error("firewall commands")
	}
	// The health check, which test-binds ports, runs only when an admin
	// asks.
	if strings.Contains(admin["/box"], "game_ports") || !strings.Contains(admin["/box"], "Run the health check") ||
		!strings.Contains(admin["/box?check=1"], "game_ports") || !strings.Contains(admin["/box?check=1"], "Run again") {
		t.Error("the health check")
	}
	for _, pg := range []map[string]string{op, rd} {
		if strings.Contains(pg["/box?check=1"], "game_ports") || strings.Contains(pg["/box"], "Run the health check") || !strings.Contains(pg["/box"], "health check needs the admin role") {
			t.Error("the health check is for admins only")
		}
	}
	// A build runs with ESM's own rights: only admins install one.
	if !strings.Contains(admin["/builds"], "Check and install") || strings.Contains(op["/builds"], "Check and install") ||
		!strings.Contains(op["/builds"], "Installing builds needs the admin role") {
		t.Error("installing builds is for admins only")
	}

	// Live updates: the event streams each screen opens.
	for p, want := range map[string]string{"/": `data-events="types=state,audit"`, "/servers/alpha/log": `data-events="instance=alpha&amp;types=state,log"`} {
		if !strings.Contains(admin[p], want) {
			t.Errorf("%s: events %q missing", p, want)
		}
	}
	if !strings.Contains(rd["/"], `data-events="types=state"`) {
		t.Error("read's dashboard asks for audit events")
	}

	// A notice after an action is one of the fixed texts, nothing else.
	_, body = s.get("/servers/alpha?done=saved", "session")
	if !strings.Contains(body, ">Saved.</p>") {
		t.Error("the saved notice")
	}
	_, body = s.get("/servers/alpha?done=%3Cb%3Ex", "session")
	if !strings.Contains(body, `id="flash" class="flash" role="status" aria-live="polite" hidden></p>`) {
		t.Error("an unknown notice code is shown")
	}

	// Errors are pages too, with every header.
	for p, want := range map[string]string{"/nope": "There is no page here.", "/servers/ghost": "not found", "/templates/layout.html": "There is no page here."} {
		resp, body := s.get(p, "session")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, want) || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
		checkHeaders(t, p, resp.Header)
		checkHTML(t, p, body, "")
	}
	resp, _ := s.do("POST", "/servers/alpha", "session")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to a page: %d", resp.StatusCode)
	}
	checkHeaders(t, "POST to a page", resp.Header)

	// A session that ended leads back to the login.
	s.must(http.StatusOK, "POST", "/api/v1/logout", "session", struct{}{}, nil)
	resp, _ = s.get("/servers/alpha", "session")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?next=%2Fservers%2Falpha" {
		t.Fatalf("after logout: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// gameserverLine is part of what the fake server writes to its console
// when it starts: its command line.
const gameserverLine = "-Port="

// The stylesheet and the script are served with every header; nothing else
// is reached through /static/, however the path is written.
func TestStaticFiles(t *testing.T) {
	s := newSite(t)
	for p, ct := range map[string]string{"/static/esm.css": "text/css; charset=utf-8", "/static/esm.js": "text/javascript; charset=utf-8"} {
		resp, body := s.get(p, "")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != ct || len(body) < 1000 {
			t.Errorf("%s: %d %s %d bytes", p, resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
		checkHeaders(t, p, resp.Header)
	}
	for _, p := range []string{
		"/static/",
		"/static/missing.css",
		"/static/%2e%2e/page.go",
		"/static/%2e%2e/%2e%2e/%2e%2e/go.mod",
		"/static/..%2f..%2fgo.mod",
		"/static/..%5c..%5cpage.go",
		"/static/..%5C..%5C..%5Cgo.mod",
		"/static/%5c%5cserver%5cshare",
		"/static/C:/Windows/win.ini",
		"/static/c:%5cwindows%5cwin.ini",
		"/static/esm.css::$DATA",
		"/static/.hidden",
		"/static/%2ehidden",
		"/static/esm.css%00.txt",
		"/static/templates/layout.html",
	} {
		resp, body := s.get(p, "")
		var eb api.ErrorBody
		if resp.StatusCode != http.StatusNotFound || json.Unmarshal([]byte(body), &eb) != nil || eb.Error.Code != "not_found" {
			t.Errorf("%s: %d %q", p, resp.StatusCode, body)
		}
		checkHeaders(t, p, resp.Header)
		for _, leak := range []string{"package ", "module ", "{{define", "--ground"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: shows a file", p)
			}
		}
	}
	// Plain dot segments are cleaned by the router first, and the clean
	// path is then refused like any other.
	for _, p := range []string{"/static/../page.go", "/static/../../go.mod", "/static/./esm.css/../../templates/layout.html"} {
		resp, _ := s.get(p, "")
		loc := resp.Header.Get("Location")
		if resp.StatusCode/100 != 3 || strings.Contains(loc, "..") {
			t.Errorf("%s: %d to %q", p, resp.StatusCode, loc)
		}
		checkHeaders(t, p, resp.Header)
		resp, body := s.get(loc, "")
		if resp.StatusCode != http.StatusNotFound || strings.Contains(body, "package ") || strings.Contains(body, "{{define") {
			t.Errorf("%s then %s: %d", p, loc, resp.StatusCode)
		}
	}
}

// The script and the stylesheet keep to the rules of the page: no eval, no
// inline style set by the script, nothing from elsewhere.
func TestScriptAndStylesheet(t *testing.T) {
	s := newSite(t)
	_, js := s.get("/static/esm.js", "")
	for _, bad := range []string{"eval(", "new Function", "innerHTML", ".style.", "setAttribute('style'", "document.write", "http://", "https://", "localStorage", "sessionStorage"} {
		if strings.Contains(js, bad) {
			t.Errorf("esm.js uses %q", bad)
		}
	}
	for _, need := range []string{"X-CSRF-Token", "meta[name=\"csrf-token\"]", "EventSource", "es.close()", "/api/v1/session", "toLogin()", "Math.min(wait * 2, 30000)"} {
		if !strings.Contains(js, need) {
			t.Errorf("esm.js lacks %q", need)
		}
	}
	_, css := s.get("/static/esm.css", "")
	for _, bad := range []string{"@import", "url(", "http://", "https://"} {
		if strings.Contains(css, bad) {
			t.Errorf("esm.css uses %q", bad)
		}
	}
	for step := 5; step <= 100; step += 5 {
		if !strings.Contains(css, fmt.Sprintf(".w%d {", step)) {
			t.Errorf("esm.css has no bar step w%d", step)
		}
	}
}
