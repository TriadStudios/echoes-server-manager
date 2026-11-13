// Package page is ESM's web page: plain HTML made on the server from
// html/template files, one stylesheet and one small script, all embedded in
// the binary. Nothing is loaded from anywhere else.
//
// The page is a client of the API like any other. Every screen gets its
// data by calling /api/v1 inside the process, as the browser's user (the
// same cookie or token, so the same roles and rules); it never reads ESM's
// state any other way. Every change is made by the script, which calls the
// API with the session's CSRF token from a <meta> tag (a random value of
// its own; the session id stays in its HttpOnly cookie and is never part of
// a page). Live updates come from the API's event stream.
//
// No page has inline script or style, style attributes or event handler
// attributes, so the strict Content-Security-Policy that every reply
// carries holds.
package page

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

//go:embed templates static
var embedded embed.FS

// Page serves the web page.
type Page struct {
	api    http.Handler
	mux    *http.ServeMux
	static http.Handler
	tmpl   map[string]*template.Template
	log    *slog.Logger
}

// screens are the template files, one per screen; each is parsed with the
// layout.
var screens = []string{
	"dashboard", "servers", "overview", "settings", "log", "players", "backups", "schedule",
	"builds", "clusters", "box", "audit", "setup", "error",
}

// New makes the page. apiHandler serves the API (paths under /api/); the
// page gets all its data through it.
func New(apiHandler http.Handler) http.Handler {
	return NewWithLogger(apiHandler, nil)
}

// NewWithLogger is New with a logger for failures to render.
func NewWithLogger(apiHandler http.Handler, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	staticFS, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	p := &Page{api: apiHandler, mux: http.NewServeMux(), static: http.StripPrefix("/static", web.Files(staticFS)),
		tmpl: map[string]*template.Template{}, log: log}
	for _, name := range screens {
		t := template.New(name).Funcs(funcs)
		p.tmpl[name] = template.Must(t.ParseFS(embedded, "templates/layout.html", "templates/"+name+".html"))
	}
	p.routes()
	return p
}

func (p *Page) routes() {
	h := func(pattern string, f func(*Page, *view)) {
		p.mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, r *http.Request) {
			v, ok := p.begin(w, r)
			if !ok {
				return
			}
			f(p, v)
		})
	}
	p.mux.Handle("GET /static/", p.static)
	p.mux.HandleFunc("GET /setup", p.setup)
	h("/{$}", (*Page).dashboard)
	h("/servers", (*Page).servers)
	h("/servers/{name}", (*Page).overview)
	h("/servers/{name}/settings", (*Page).settings)
	h("/servers/{name}/log", (*Page).logScreen)
	h("/servers/{name}/players", (*Page).players)
	h("/servers/{name}/backups", (*Page).backups)
	h("/servers/{name}/schedule", (*Page).schedule)
	h("/builds", (*Page).builds)
	h("/clusters", (*Page).clusters)
	h("/box", (*Page).box)
	h("/audit", (*Page).audit)
	p.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p.fail(w, r, http.StatusNotFound, "There is no page here.")
	})
}

func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	web.SetSecurityHeaders(w.Header())
	p.mux.ServeHTTP(w, r)
}

// view is one page being made: the request, the caller, and what the
// layout shows.
type view struct {
	w http.ResponseWriter
	r *http.Request
	c client

	Title  string
	Nav    string // the menu entry that is active
	Screen string // names the screen for the script
	Events string // event stream query the script opens, "" for none
	Bare   bool   // no menu and no user (setup, login, errors before login)

	Session api.SessionInfo
	Box     api.Box
	Notice  string // one line after an action, from a fixed list
	Data    any
}

// Operator and Admin report what the caller's role allows.
func (v *view) Operator() bool { return auth.Role(v.Session.Role).Allows(auth.RoleOperator) }
func (v *view) Admin() bool    { return auth.Role(v.Session.Role).Allows(auth.RoleAdmin) }

// CSRF is the session's CSRF token for the script ("" for callers that are
// not a cookie session).
func (v *view) CSRF() string { return v.Session.CSRFToken }

// begin starts a page that needs a login. A caller that has none is sent to
// /setup, which asks for one (or for the first setup).
func (p *Page) begin(w http.ResponseWriter, r *http.Request) (*view, bool) {
	v := &view{w: w, r: r, c: client{h: p.api, r: r}}
	if err := v.c.get("/api/v1/session", &v.Session); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			http.Redirect(w, r, "/setup?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
			return nil, false
		}
		p.fail(w, r, statusOf(err), err.Error())
		return nil, false
	}
	if err := v.c.get("/api/v1/box", &v.Box); err != nil {
		p.fail(w, r, statusOf(err), err.Error())
		return nil, false
	}
	v.Notice = notices[r.URL.Query().Get("done")]
	return v, true
}

// notices are the lines a page shows after the script made a change and
// reloaded it with ?done=<code>. Only these texts can be shown this way.
var notices = map[string]string{
	"started":   "Start requested. The state below follows the server.",
	"stopped":   "Stopped.",
	"restarted": "Restart requested. The state below follows the server.",
	"backup":    "Backup made.",
	"restored":  "Backup restored.",
	"updated":   "Updated.",
	"saved":     "Saved.",
	"installed": "Build installed.",
	"removed":   "Removed.",
	"revoked":   "Token revoked.",
}

// render writes the screen template with v.
func (p *Page) render(v *view, screen string, status int) {
	var buf bytes.Buffer
	if err := p.tmpl[screen].ExecuteTemplate(&buf, "layout", v); err != nil {
		p.log.Error("cannot make the page", "screen", screen, "err", err)
		p.fail(v.w, v.r, http.StatusInternalServerError, "This page could not be made: "+err.Error())
		return
	}
	h := v.w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	v.w.WriteHeader(status)
	v.w.Write(buf.Bytes())
}

// fail writes the error page. It needs no login and shows nothing but msg.
func (p *Page) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	v := &view{w: w, r: r, Title: http.StatusText(status), Bare: true, Data: errorData{Status: status, Message: msg}}
	var buf bytes.Buffer
	if err := p.tmpl["error"].ExecuteTemplate(&buf, "layout", v); err != nil {
		web.NotFound(w)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

type errorData struct {
	Status  int
	Message string
}

// statusOf is the HTTP status that an API error stands for.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status >= 400 {
		return ae.Status
	}
	return http.StatusInternalServerError
}

// --- calls to the API ---

// client calls the API inside the process on behalf of the browser's
// request r: with its cookie, its Authorization header and its Origin (so
// the API checks them exactly as for the browser), from its address.
type client struct {
	h http.Handler
	r *http.Request
}

// apiError is an error answer of the API.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string { return e.Message }

// maxReply bounds what the page reads of one API answer.
const maxReply = 16 << 20

// get calls GET path and decodes the answer into out.
func (c client) get(path string, out any) error {
	req, err := http.NewRequestWithContext(c.r.Context(), http.MethodGet, path, http.NoBody)
	if err != nil {
		return err
	}
	req.Host = c.r.Host
	req.RemoteAddr = c.r.RemoteAddr
	for _, k := range []string{"Cookie", "Authorization", "Origin"} {
		for _, val := range c.r.Header.Values(k) {
			req.Header.Add(k, val)
		}
	}
	rec := &recorder{header: http.Header{}}
	c.h.ServeHTTP(rec, req)
	if rec.over {
		return &apiError{Status: http.StatusInternalServerError, Code: "too_large", Message: "the answer of " + path + " is too large for the page"}
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if status >= 400 {
		var eb api.ErrorBody
		if json.Unmarshal(rec.body.Bytes(), &eb) != nil || eb.Error.Code == "" {
			return &apiError{Status: status, Code: "internal", Message: fmt.Sprintf("%s answered %d", path, status)}
		}
		return &apiError{Status: status, Code: eb.Error.Code, Message: eb.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rec.body.Bytes(), out); err != nil {
		return fmt.Errorf("the answer of %s: %w", path, err)
	}
	return nil
}

// recorder takes an API answer inside the process.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	over   bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.body.Len()+len(b) > maxReply {
		r.over = true
		return 0, errors.New("answer too large")
	}
	return r.body.Write(b)
}

// safeNext returns where to go after a login: a page path, never another
// site, the API or the login itself.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\:?#") ||
		strings.HasPrefix(next, "/api/") || strings.HasPrefix(next, "/setup") || strings.HasPrefix(next, "/static/") ||
		strings.ContainsFunc(next, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return "/"
	}
	return next
}
