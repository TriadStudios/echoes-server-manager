// Package api is ESM's HTTP API under /api/v1, plus the embedded web page
// at "/".
//
// Requests are authenticated by a bearer token (Authorization: Bearer), by
// the session cookie of a logged-in user (with X-CSRF-Token on every
// change), or by arriving on the unix socket (admin). Tokens are never read
// from URLs. Every call that changes something writes an audit entry, and
// so does every refused call. Errors always have the shape
// {"error": {"code": "...", "message": "..."}}.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

// MaxBody is the largest request body accepted.
const MaxBody = 1 << 20

// Options wire a Server to the rest of ESM.
type Options struct {
	Version    string
	Config     *config.Config
	Auth       *auth.Manager
	Audit      *audit.Log
	Bus        *events.Bus
	Instances  *instances.Manager
	Builds     *builds.Manager
	Supervisor supervisor.Supervisor
	Logger     *slog.Logger
	// TLS marks the TCP listener as TLS (session cookies get Secure).
	TLS bool
	// Started is when the service started.
	Started time.Time
	// KeepAlive is the SSE keep-alive interval (default 15s).
	KeepAlive time.Duration
	// PortHost is where doctor test-binds free ports (default all
	// interfaces).
	PortHost string
}

// Server serves the API and the page.
type Server struct {
	o     Options
	log   *slog.Logger
	mux   *http.ServeMux
	paths *http.ServeMux      // every API path once, for 405 answers
	allow map[string][]string // path pattern -> methods
}

// access says who may call a route.
type access int

const (
	public   access = iota // no login; credentials are not even read
	optional               // credentials are checked when present
	required               // a login with at least the route's role
)

type handler func(s *Server, w http.ResponseWriter, r *http.Request, c *call) error

// route is one API endpoint.
type route struct {
	method, path string
	access       access
	role         auth.Role
	action       string // audit action
	h            handler
}

// Route describes a registered API endpoint.
type Route struct {
	Method string
	Path   string
	Role   string // "" for endpoints that need no login
}

// Routes lists every API endpoint.
func Routes() []Route {
	out := make([]Route, 0, len(routes))
	for _, rt := range routes {
		role := string(rt.role)
		if rt.access != required {
			role = ""
		}
		out = append(out, Route{Method: rt.method, Path: rt.path, Role: role})
	}
	return out
}

var routes []route

func init() {
	const (
		read  = auth.RoleRead
		oper  = auth.RoleOperator
		admin = auth.RoleAdmin
	)
	r := func(method, path string, ac access, role auth.Role, action string, h handler) {
		routes = append(routes, route{method, path, ac, role, action, h})
	}
	const p = "/api/v1"
	r("GET", p+"/setup", public, "", "setup.status", (*Server).setupStatus)
	r("POST", p+"/setup", public, "", "setup", (*Server).setup)
	r("POST", p+"/login", public, "", "login", (*Server).login)
	r("POST", p+"/logout", optional, "", "logout", (*Server).logout)
	r("GET", p+"/session", required, read, "session.get", (*Server).session)
	r("GET", p+"/box", required, read, "box.get", (*Server).box)
	r("GET", p+"/box/doctor", required, read, "box.doctor", (*Server).doctor)
	r("GET", p+"/builds", required, read, "build.list", (*Server).listBuilds)
	r("POST", p+"/builds", required, oper, "build.install", (*Server).installBuild)
	r("DELETE", p+"/builds/{version}", required, admin, "build.delete", (*Server).deleteBuild)
	r("POST", p+"/builds/prune", required, admin, "build.prune", (*Server).pruneBuilds)
	r("GET", p+"/instances", required, read, "instance.list", (*Server).listInstances)
	r("POST", p+"/instances", required, admin, "instance.create", (*Server).createInstance)
	r("GET", p+"/instances/{name}", required, read, "instance.get", (*Server).getInstance)
	r("PATCH", p+"/instances/{name}", required, admin, "instance.change", (*Server).patchInstance)
	r("DELETE", p+"/instances/{name}", required, admin, "instance.delete", (*Server).deleteInstance)
	r("PUT", p+"/instances/{name}/secrets", required, admin, "instance.secrets", (*Server).putSecrets)
	r("POST", p+"/instances/{name}/start", required, oper, "instance.start", (*Server).start)
	r("POST", p+"/instances/{name}/stop", required, oper, "instance.stop", (*Server).stop)
	r("POST", p+"/instances/{name}/restart", required, oper, "instance.restart", (*Server).restart)
	r("POST", p+"/instances/{name}/update", required, oper, "instance.update", (*Server).update)
	r("POST", p+"/instances/{name}/backup", required, oper, "instance.backup", (*Server).backup)
	r("POST", p+"/instances/{name}/restore", required, oper, "instance.restore", (*Server).restore)
	r("GET", p+"/instances/{name}/status", required, read, "instance.status", (*Server).status)
	r("GET", p+"/instances/{name}/logs", required, read, "instance.logs", (*Server).logs)
	r("GET", p+"/instances/{name}/backups", required, read, "instance.backups", (*Server).listBackups)
	r("GET", p+"/events", required, read, "events", (*Server).events)
	r("GET", p+"/clusters", required, read, "cluster.list", (*Server).clusters)
	r("GET", p+"/tokens", required, admin, "token.list", (*Server).listTokens)
	r("POST", p+"/tokens", required, admin, "token.create", (*Server).createToken)
	r("DELETE", p+"/tokens/{id}", required, admin, "token.delete", (*Server).deleteToken)
	r("GET", p+"/audit", required, admin, "audit.list", (*Server).auditList)
}

// New builds the server and its routes.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.KeepAlive <= 0 {
		o.KeepAlive = 15 * time.Second
	}
	if o.Started.IsZero() {
		o.Started = time.Now()
	}
	s := &Server{o: o, log: o.Logger, mux: http.NewServeMux(), paths: http.NewServeMux(), allow: map[string][]string{}}
	for _, rt := range routes {
		s.mux.Handle(rt.method+" "+rt.path, s.wrap(rt))
		if _, ok := s.allow[rt.path]; !ok {
			s.paths.Handle(rt.path, http.NotFoundHandler())
		}
		s.allow[rt.path] = append(s.allow[rt.path], rt.method)
	}
	s.mux.Handle("/api/", s.wrap(route{access: public, action: "api.unknown", h: (*Server).unknownAPI}))
	page := web.Handler()
	s.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			page.ServeHTTP(w, r)
			return
		}
		web.SetSecurityHeaders(w.Header())
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET and HEAD are allowed here")
	}))
	return s
}

type ctxKey int

const socketKey ctxKey = 1

// Handler serves requests arriving over TCP.
func (s *Server) Handler() http.Handler { return s.mux }

// SocketHandler serves requests arriving on the unix socket: they act as
// admin (actor kind "socket").
func (s *Server) SocketHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), socketKey, true)))
	})
}

// call carries per-request facts between the wrapper and a handler.
type call struct {
	actor   audit.Actor
	role    auth.Role
	session *auth.Session
	target  string
	errMsg  string
}

// apiError is an error with an explicit HTTP status and code.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func errStatus(status int, code, format string, args ...any) error {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &apiError{status: status, code: code, msg: msg}
}

// statusWriter records the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) wrap(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		web.SetSecurityHeaders(sw.Header())
		sw.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(sw, r.Body, MaxBody)
		c := &call{actor: audit.Actor{Kind: audit.ActorAnonymous, ID: auth.ClientIP(r)}}
		if r.Context().Value(socketKey) == true {
			c.actor.ID = "socket"
		}
		err := s.authenticate(r, rt, c)
		if err == nil {
			err = rt.h(s, sw, r, c)
		}
		if err != nil {
			s.fail(sw, err, c)
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if !auth.SafeMethod(r.Method) || status == http.StatusForbidden {
			s.auditCall(rt, c, status)
		}
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", status, "ms", time.Since(start).Milliseconds())
	})
}

// authenticate fills c with the caller and checks the route's role.
func (s *Server) authenticate(r *http.Request, rt route, c *call) error {
	if rt.access == public {
		return nil
	}
	err := s.identify(r, c)
	if rt.access == optional {
		if errors.Is(err, auth.ErrCSRF) || errors.Is(err, auth.ErrOrigin) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if c.role == "" {
		return errStatus(http.StatusUnauthorized, "unauthorized", "log in, or send Authorization: Bearer <token>")
	}
	if !c.role.Allows(rt.role) {
		return errStatus(http.StatusForbidden, "forbidden", "this needs the %s role; you have %s", rt.role, c.role)
	}
	return nil
}

// identify works out who is calling. No credentials is not an error (the
// role stays empty).
func (s *Server) identify(r *http.Request, c *call) error {
	if r.Context().Value(socketKey) == true {
		c.actor = audit.Actor{Kind: audit.ActorSocket, ID: "socket", Role: string(auth.RoleAdmin)}
		c.role = auth.RoleAdmin
		return nil
	}
	if r.Header.Get("Authorization") != "" {
		tok, ok := auth.BearerToken(r)
		if !ok {
			return errStatus(http.StatusUnauthorized, "unauthorized", "only bearer tokens are accepted in Authorization")
		}
		t, err := s.o.Auth.AuthenticateToken(tok)
		if err != nil {
			return err
		}
		c.actor = audit.Actor{Kind: audit.ActorToken, ID: t.ID, Role: string(t.Role)}
		c.role = t.Role
		return nil
	}
	ck, err := r.Cookie(auth.SessionCookieName)
	if err != nil || ck.Value == "" {
		return nil
	}
	sess, err := s.o.Auth.Session(ck.Value)
	if err != nil {
		return err
	}
	c.actor = audit.Actor{Kind: audit.ActorUser, ID: sess.Username, Role: string(sess.Role)}
	if err := auth.CheckCookieRequest(r, sess); err != nil {
		return err
	}
	c.role = sess.Role
	c.session = sess
	return nil
}

func (s *Server) auditCall(rt route, c *call, status int) {
	if s.o.Audit == nil {
		return
	}
	e := audit.Entry{Actor: c.actor, Action: rt.action, Target: c.target, OK: status < 400, Error: c.errMsg}
	if err := s.o.Audit.Append(e); err != nil {
		s.log.Error("cannot write the audit log", "err", err)
	}
}

// fail writes err as the standard error response.
func (s *Server) fail(w http.ResponseWriter, err error, c *call) {
	status, code, msg := classify(err)
	var rl *auth.RateLimitError
	if errors.As(err, &rl) {
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())+1))
	}
	msg = audit.Scrub(msg)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "err", msg)
	}
	c.errMsg = msg
	writeError(w, status, code, msg)
}

// classify maps an error to status, code and message.
func classify(err error) (int, string, string) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status, ae.code, ae.msg
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge, "too_large", "request body is larger than 1 MiB"
	}
	var rl *auth.RateLimitError
	switch {
	case errors.As(err, &rl):
		return http.StatusTooManyRequests, "rate_limited", err.Error()
	case errors.Is(err, auth.ErrBadCredentials):
		return http.StatusUnauthorized, "bad_credentials", err.Error()
	case errors.Is(err, auth.ErrBadSetupCode):
		return http.StatusUnauthorized, "bad_setup_code", err.Error()
	case errors.Is(err, auth.ErrBadToken), errors.Is(err, auth.ErrNoSession):
		return http.StatusUnauthorized, "unauthorized", err.Error()
	case errors.Is(err, auth.ErrCSRF):
		return http.StatusForbidden, "csrf", err.Error()
	case errors.Is(err, auth.ErrOrigin):
		return http.StatusForbidden, "origin", err.Error()
	case errors.Is(err, auth.ErrForbidden):
		return http.StatusForbidden, "forbidden", err.Error()
	case errors.Is(err, auth.ErrSetupDone):
		return http.StatusConflict, "setup_done", err.Error()
	case errors.Is(err, auth.ErrUserExists):
		return http.StatusConflict, "conflict", err.Error()
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrBadUsername), errors.Is(err, auth.ErrBadRole):
		return http.StatusUnprocessableEntity, "invalid", err.Error()
	case errors.Is(err, auth.ErrNotFound):
		return http.StatusNotFound, "not_found", err.Error()
	}
	code := fault.CodeOf(err)
	switch fault.KindOf(err) {
	case fault.BadRequest:
		return http.StatusBadRequest, code, err.Error()
	case fault.Invalid, fault.Failed:
		return http.StatusUnprocessableEntity, code, err.Error()
	case fault.NotFound:
		return http.StatusNotFound, code, err.Error()
	case fault.Conflict:
		return http.StatusConflict, code, err.Error()
	case fault.Forbidden:
		return http.StatusForbidden, code, err.Error()
	}
	return http.StatusInternalServerError, "internal", err.Error()
}

// ErrorBody is the shape of every error response.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inside of an error response.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, err = w.Write(append(b, '\n'))
	return err
}

// decode reads a JSON body into v. An empty body leaves v as it is.
func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fault.BadRequestf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return fault.BadRequestf("invalid JSON body: more than one value")
	}
	return nil
}

func (s *Server) unknownAPI(w http.ResponseWriter, r *http.Request, c *call) error {
	if _, pattern := s.paths.Handler(r); pattern != "" {
		methods := slices.Clone(s.allow[pattern])
		if slices.Contains(methods, http.MethodGet) {
			methods = append(methods, http.MethodHead)
		}
		w.Header().Set("Allow", strings.Join(methods, ", "))
		return errStatus(http.StatusMethodNotAllowed, "method_not_allowed", "%s is not allowed on %s", r.Method, r.URL.Path)
	}
	return errStatus(http.StatusNotFound, "not_found", "no API endpoint %s", r.URL.Path)
}

// queryInt reads an integer query parameter with a default and bounds.
func queryInt(r *http.Request, key string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fault.Invalidf("%s must be a number from %d to %d", key, lo, hi)
	}
	return n, nil
}

func queryBool(r *http.Request, key string) (bool, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fault.Invalidf("%s must be true or false", key)
	}
	return b, nil
}
