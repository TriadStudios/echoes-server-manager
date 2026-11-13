package api

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/doctor"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
)

// --- logins ---

// SetupStatus answers GET /setup.
type SetupStatus struct {
	SetupNeeded bool `json:"setup_needed"`
}

// SetupRequest is the body of POST /setup.
type SetupRequest struct {
	Code     string `json:"code"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// UserInfo names a user and role.
type UserInfo struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// LoginRequest is the body of POST /login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResult answers POST /login. The session itself is in the
// esm_session cookie; csrf_token goes in X-CSRF-Token on every change.
type LoginResult struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	CSRFToken string `json:"csrf_token"`
}

// SessionInfo answers GET /session: who the caller is.
type SessionInfo struct {
	Kind      string `json:"kind"` // user | token | socket
	ID        string `json:"id"`
	Role      string `json:"role"`
	CSRFToken string `json:"csrf_token,omitempty"` // only for cookie sessions
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request, c *call) error {
	return writeJSON(w, http.StatusOK, SetupStatus{SetupNeeded: !s.o.Auth.HasAdmin()})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := auth.CheckLoginRequest(r, auth.Scheme(s.o.HTTPS)); err != nil {
		return err
	}
	var req SetupRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	// Only a real user's name is recorded (see auth.Manager.AuditName).
	name := s.o.Auth.AuditName(req.Username)
	c.actor = audit.Actor{Kind: audit.ActorUser, ID: name}
	c.target = name
	u, err := s.o.Auth.Setup(auth.LimitAddress(r, s.behindProxy()), req.Code, req.Username, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadSetupCode) {
			s.failPause(r.Context())
		}
		return err
	}
	c.actor = audit.Actor{Kind: audit.ActorUser, ID: u.Username, Role: string(u.Role)}
	c.target = u.Username
	return writeJSON(w, http.StatusCreated, UserInfo{Username: u.Username, Role: string(u.Role)})
}

// behindProxy reports whether behind_tls_proxy is set.
func (s *Server) behindProxy() bool { return s.o.Config != nil && s.o.Config.BehindTLSProxy }

// failPause holds the answer to a failed login or setup code for
// FailDelay, or until the client goes away. No lock is held meanwhile.
func (s *Server) failPause(ctx context.Context) {
	if s.o.FailDelay <= 0 {
		return
	}
	t := time.NewTimer(s.o.FailDelay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := auth.CheckLoginRequest(r, auth.Scheme(s.o.HTTPS)); err != nil {
		return err
	}
	var req LoginRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	// Only a real user's name is recorded (see auth.Manager.AuditName).
	c.actor = audit.Actor{Kind: audit.ActorUser, ID: s.o.Auth.AuditName(req.Username)}
	// Behind a proxy or a tunnel all clients share one address: the limit
	// then counts per user name only (see auth.LimitAddress), and every
	// failure is answered late.
	sess, err := s.o.Auth.Login(auth.LimitAddress(r, s.behindProxy()), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrBadCredentials) {
			s.failPause(r.Context())
		}
		return err
	}
	c.actor = audit.Actor{Kind: audit.ActorUser, ID: sess.Username, Role: string(sess.Role)}
	// The session id goes only into the HttpOnly cookie, never into a
	// body; the CSRF token is a separate random value.
	http.SetCookie(w, auth.SessionCookie(sess.ID, sess.Life(), s.o.HTTPS))
	return writeJSON(w, http.StatusOK, LoginResult{Username: sess.Username, Role: string(sess.Role), CSRFToken: sess.CSRF})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, c *call) error {
	if ck, err := r.Cookie(auth.SessionCookieName); err == nil && ck.Value != "" {
		s.o.Auth.Logout(ck.Value)
	}
	http.SetCookie(w, auth.ClearSessionCookie(s.o.HTTPS))
	return writeJSON(w, http.StatusOK, map[string]bool{"logged_out": true})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request, c *call) error {
	out := SessionInfo{Kind: c.actor.Kind, ID: c.actor.ID, Role: string(c.role)}
	if c.session != nil {
		out.CSRFToken = c.session.CSRF
	}
	return writeJSON(w, http.StatusOK, out)
}

// --- box ---

// Box answers GET /box.
type Box struct {
	Version        string `json:"version"`
	Root           string `json:"root"`
	OS             string `json:"os"`
	Supervisor     string `json:"supervisor"`
	LimitsEnforced bool   `json:"limits_enforced"`
	GamePorts      string `json:"game_ports"`
	QueryPorts     string `json:"query_ports"`
	Listen         string `json:"listen"`
	TLS            bool   `json:"tls"`
	// BehindTLSProxy echoes the behind_tls_proxy setting: browsers reach
	// ESM through a TLS proxy, so the session cookie is Secure.
	BehindTLSProxy bool      `json:"behind_tls_proxy"`
	Started        time.Time `json:"started"`
	UptimeSeconds  int64     `json:"uptime_seconds"`
	Instances      int       `json:"instances"`
	Running        int       `json:"running"`
	Builds         int       `json:"builds"`
	// DefaultSecretDelivery is how a new instance hands its server the
	// passwords unless it says otherwise.
	DefaultSecretDelivery string `json:"default_secret_delivery"`
	// SecretsOnCommandLine names the instances that pass their passwords on
	// the server command line: unsafe on a shared box.
	SecretsOnCommandLine []string `json:"secrets_on_command_line"`
}

func (s *Server) box(w http.ResponseWriter, r *http.Request, c *call) error {
	list := s.o.Instances.List()
	running := 0
	unsafe := []string{}
	for _, v := range list {
		if v.Status.State == instances.Running {
			running++
		}
		if v.OutputHidden() {
			unsafe = append(unsafe, v.Name)
		}
	}
	bl, err := s.o.Builds.List()
	if err != nil {
		return err
	}
	cfg := s.o.Config
	return writeJSON(w, http.StatusOK, Box{
		Version: s.o.Version, Root: cfg.Root, OS: runtime.GOOS,
		Supervisor: s.o.Supervisor.Kind(), LimitsEnforced: s.o.Supervisor.LimitsEnforced(),
		GamePorts: cfg.GamePorts, QueryPorts: cfg.QueryPorts, Listen: cfg.Listen, TLS: s.o.TLS,
		BehindTLSProxy: cfg.BehindTLSProxy,
		Started:        s.o.Started.UTC(), UptimeSeconds: int64(time.Since(s.o.Started).Seconds()),
		Instances: len(list), Running: running, Builds: len(bl),
		DefaultSecretDelivery: gameserver.SecretDelivery, SecretsOnCommandLine: unsafe,
	})
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request, c *call) error {
	// The check binds every free port of both ranges for a moment: one at
	// a time, so that checks cannot keep the ports busy for starts.
	if !s.doctorBusy.CompareAndSwap(false, true) {
		return errStatus(http.StatusConflict, "doctor_running", "a health check is already running; try again when it is done")
	}
	defer s.doctorBusy.Store(false)
	used := map[int]bool{}
	for _, v := range s.o.Instances.List() {
		used[v.Ports.Game], used[v.Ports.Query] = true, true
	}
	rep := doctor.Run(doctor.Options{
		Root: s.o.Config.Root, PortHost: s.o.PortHost, UsedPorts: used,
		Supervisor: s.o.Supervisor.Kind(), LimitsEnforced: s.o.Supervisor.LimitsEnforced(), Service: true,
	})
	return writeJSON(w, http.StatusOK, rep)
}

// --- builds ---

// BuildView is a build with the instances that use it.
type BuildView struct {
	builds.Build
	UsedBy []string `json:"used_by"`
}

// BuildList answers GET /builds.
type BuildList struct {
	Builds []BuildView `json:"builds"`
}

// InstallRequest is the body of POST /builds.
type InstallRequest struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256,omitempty"`
}

// Removed lists removed builds.
type Removed struct {
	Removed []string `json:"removed"`
}

func (s *Server) buildView(b builds.Build, users map[string][]string) BuildView {
	u := users[b.Version]
	if u == nil {
		u = []string{}
	}
	return BuildView{Build: b, UsedBy: u}
}

func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request, c *call) error {
	list, err := s.o.Builds.List()
	if err != nil {
		return err
	}
	users := s.o.Instances.BuildUsers()
	out := BuildList{Builds: []BuildView{}}
	for _, b := range list {
		out.Builds = append(out.Builds, s.buildView(b, users))
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) installBuild(w http.ResponseWriter, r *http.Request, c *call) error {
	var req InstallRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	c.target = builds.RedactSource(req.Source)
	b, err := s.o.Builds.Install(r.Context(), req.Source, req.SHA256)
	if err != nil {
		return err
	}
	c.target = b.Version
	return writeJSON(w, http.StatusCreated, s.buildView(b, s.o.Instances.BuildUsers()))
}

func (s *Server) deleteBuild(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := decode(r, &struct{}{}); err != nil {
		return err
	}
	v := r.PathValue("version")
	c.target = v
	if err := s.o.Instances.DeleteBuild(v); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, Removed{Removed: []string{v}})
}

func (s *Server) pruneBuilds(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := decode(r, &struct{}{}); err != nil {
		return err
	}
	removed, err := s.o.Instances.PruneBuilds()
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, Removed{Removed: removed})
}

// --- instances ---

// InstanceList answers GET /instances.
type InstanceList struct {
	Instances []instances.View `json:"instances"`
}

// InstanceChange answers create and change calls.
type InstanceChange struct {
	instances.View
	RestartRequired bool `json:"restart_required"`
}

// DeleteRequest is the body of DELETE /instances/{name}.
type DeleteRequest struct {
	PurgeBackups bool `json:"purge_backups"`
}

// Deleted names what was deleted.
type Deleted struct {
	Deleted string `json:"deleted"`
}

// SecretsResult answers PUT /instances/{name}/secrets.
type SecretsResult struct {
	Secrets         instances.SecretFlags `json:"secrets"`
	RestartRequired bool                  `json:"restart_required"`
	Warnings        []string              `json:"warnings"`
}

// UpdateRequest is the body of POST /instances/{name}/update.
type UpdateRequest struct {
	To string `json:"to"`
}

// RestoreRequest is the body of POST /instances/{name}/restore.
type RestoreRequest struct {
	Backup string `json:"backup"`
}

// Logs answers GET /instances/{name}/logs.
type Logs struct {
	Instance string   `json:"instance"`
	Source   string   `json:"source"`
	Lines    []string `json:"lines"`
}

// BackupList answers GET /instances/{name}/backups.
type BackupList struct {
	Backups []instances.Backup `json:"backups"`
}

// ClusterList answers GET /clusters.
type ClusterList struct {
	Clusters []instances.Cluster `json:"clusters"`
}

// full reports whether the caller may see everything about an instance.
// The read role may not: of an instance that passes its passwords on the
// server command line it gets neither the server output nor the failure
// details (see instances.HiddenNotice).
func (c *call) full() bool { return c.role.Allows(auth.RoleOperator) }

// show returns an instance as the caller may see it.
func (c *call) show(v instances.View) instances.View {
	if c.full() {
		return v
	}
	return v.ForRead()
}

func (s *Server) listInstances(w http.ResponseWriter, r *http.Request, c *call) error {
	list := s.o.Instances.List()
	for i := range list {
		list[i] = c.show(list[i])
	}
	return writeJSON(w, http.StatusOK, InstanceList{Instances: list})
}

func (s *Server) createInstance(w http.ResponseWriter, r *http.Request, c *call) error {
	var req instances.CreateRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	c.target = req.Name
	v, err := s.o.Instances.Create(req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, InstanceChange{View: c.show(v)})
}

func (s *Server) getInstance(w http.ResponseWriter, r *http.Request, c *call) error {
	v, err := s.o.Instances.Get(r.PathValue("name"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c.show(v))
}

func (s *Server) patchInstance(w http.ResponseWriter, r *http.Request, c *call) error {
	name := r.PathValue("name")
	c.target = name
	var req instances.PatchRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	v, restart, err := s.o.Instances.Patch(name, req)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, InstanceChange{View: c.show(v), RestartRequired: restart})
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request, c *call) error {
	name := r.PathValue("name")
	c.target = name
	var req DeleteRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if err := s.o.Instances.Delete(r.Context(), name, req.PurgeBackups); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, Deleted{Deleted: name})
}

func (s *Server) putSecrets(w http.ResponseWriter, r *http.Request, c *call) error {
	name := r.PathValue("name")
	c.target = name
	var req instances.SecretsRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	flags, restart, err := s.o.Instances.SetSecrets(name, req)
	if err != nil {
		return err
	}
	v, err := s.o.Instances.Get(name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, SecretsResult{Secrets: flags, RestartRequired: restart, Warnings: v.Warnings})
}

// action reads the target instance and an optional JSON body.
func action(r *http.Request, c *call, body any) (string, error) {
	name := r.PathValue("name")
	c.target = name
	if body == nil {
		body = &struct{}{}
	}
	return name, decode(r, body)
}

func (s *Server) start(w http.ResponseWriter, r *http.Request, c *call) error {
	name, err := action(r, c, nil)
	if err != nil {
		return err
	}
	wait, err := queryBool(r, "wait")
	if err != nil {
		return err
	}
	st, err := s.o.Instances.Start(r.Context(), name, wait)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request, c *call) error {
	name, err := action(r, c, nil)
	if err != nil {
		return err
	}
	st, err := s.o.Instances.Stop(r.Context(), name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request, c *call) error {
	name, err := action(r, c, nil)
	if err != nil {
		return err
	}
	wait, err := queryBool(r, "wait")
	if err != nil {
		return err
	}
	st, err := s.o.Instances.Restart(r.Context(), name, "restarted", wait)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) update(w http.ResponseWriter, r *http.Request, c *call) error {
	var req UpdateRequest
	name, err := action(r, c, &req)
	if err != nil {
		return err
	}
	if req.To == "" {
		return fault.Invalidf("to: the build version to update to is required")
	}
	c.target = name + " to " + req.To
	st, err := s.o.Instances.Update(r.Context(), name, req.To)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request, c *call) error {
	name, err := action(r, c, nil)
	if err != nil {
		return err
	}
	b, err := s.o.Instances.Backup(r.Context(), name, instances.ReasonManual)
	if err != nil {
		return err
	}
	c.target = name + "/" + b.ID
	return writeJSON(w, http.StatusCreated, b)
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request, c *call) error {
	var req RestoreRequest
	name, err := action(r, c, &req)
	if err != nil {
		return err
	}
	if req.Backup == "" {
		return fault.Invalidf("backup: the backup id is required")
	}
	c.target = name + "/" + req.Backup
	st, err := s.o.Instances.Restore(r.Context(), name, req.Backup)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, c *call) error {
	v, err := s.o.Instances.Get(r.PathValue("name"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c.show(v).Status)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, c *call) error {
	n, err := queryInt(r, "lines", 200, 1, instances.MaxTailLines)
	if err != nil {
		return err
	}
	source := r.URL.Query().Get("source")
	if source == "" {
		source = instances.SourceServer
	}
	name := r.PathValue("name")
	lines, err := s.o.Instances.Logs(name, source, n, c.full())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, Logs{Instance: name, Source: source, Lines: lines})
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, c *call) error {
	list, err := s.o.Instances.ListBackups(r.PathValue("name"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, BackupList{Backups: list})
}

func (s *Server) clusters(w http.ResponseWriter, r *http.Request, c *call) error {
	return writeJSON(w, http.StatusOK, ClusterList{Clusters: s.o.Instances.Clusters()})
}

// --- tokens and audit ---

// TokenView is a token without its hash.
type TokenView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Role     string    `json:"role"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// TokenList answers GET /tokens.
type TokenList struct {
	Tokens []TokenView `json:"tokens"`
}

// TokenRequest is the body of POST /tokens.
type TokenRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// NewToken answers POST /tokens. Token is shown only this once.
type NewToken struct {
	TokenView
	Token string `json:"token"`
}

// AuditList answers GET /audit.
type AuditList struct {
	Entries []audit.Entry `json:"entries"`
}

func tokenView(t auth.Token) TokenView {
	return TokenView{ID: t.ID, Name: t.Name, Role: string(t.Role), Created: t.Created, LastUsed: t.LastUsed}
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, c *call) error {
	out := TokenList{Tokens: []TokenView{}}
	for _, t := range s.o.Auth.Tokens() {
		out.Tokens = append(out.Tokens, tokenView(t))
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, c *call) error {
	var req TokenRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		return err
	}
	if len(req.Name) > 64 || strings.ContainsFunc(req.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fault.Invalidf("name: at most 64 printable characters")
	}
	plain, t, err := s.o.Auth.CreateToken(req.Name, role)
	if err != nil {
		return err
	}
	c.target = t.ID
	return writeJSON(w, http.StatusCreated, NewToken{TokenView: tokenView(t), Token: plain})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, c *call) error {
	if err := decode(r, &struct{}{}); err != nil {
		return err
	}
	id := r.PathValue("id")
	c.target = id
	if err := s.o.Auth.DeleteToken(id); err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, Deleted{Deleted: id})
}

func (s *Server) auditList(w http.ResponseWriter, r *http.Request, c *call) error {
	n, err := queryInt(r, "limit", 100, 1, 1000)
	if err != nil {
		return err
	}
	entries, err := s.o.Audit.Recent(n)
	if err != nil {
		return err
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	return writeJSON(w, http.StatusOK, AuditList{Entries: entries})
}
