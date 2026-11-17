// Package auth holds ESM's logins: the one-time setup code, users with
// PBKDF2 password hashes, browser sessions, API tokens, roles, the login rate
// limit and the CSRF and Origin checks for cookie requests.
//
// Files live under <root>/secrets/ (directory 0700, files 0600):
//
//	setup.json   hash and expiry of the one-time setup code
//	users.json   users and password hashes
//	logins.json  when each user last logged in
//	tokens.json  API tokens (only their SHA-256 is stored)
//
// Sessions are kept in memory only.
//
// A user changes their own password with the current one (ChangePassword,
// which ends the user's other sessions); the box owner sets one without it
// (ResetPassword, which ends them all).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// Errors returned by the package. StatusCode maps them to HTTP statuses.
var (
	ErrBadCredentials = errors.New("wrong username or password")
	ErrBadSetupCode   = errors.New("setup code is wrong or expired")
	ErrSetupDone      = errors.New("setup is already done: an admin exists")
	ErrWeakPassword   = errors.New("password must be at least 12 characters")
	ErrLongPassword   = fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	ErrBadUsername    = errors.New("username must be 1-64 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
	ErrUserExists     = errors.New("user already exists")
	ErrBadRole        = errors.New("role must be read, operator, admin or site")
	ErrNotFound       = errors.New("not found")
	ErrBadToken       = errors.New("invalid token")
	ErrNoSession      = errors.New("not logged in")
	ErrForbidden      = errors.New("forbidden")
	ErrCSRF           = errors.New("missing or wrong X-CSRF-Token header")
	ErrOrigin         = errors.New("origin header does not match the host")
	ErrNotJSON        = errors.New("the body must be JSON, sent with Content-Type: application/json")
	// ErrWrongPassword is a wrong current password given to change it.
	ErrWrongPassword = errors.New("the current password is wrong")
)

// RateLimitError is returned when a client or username has failed too often.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("too many failed attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Defaults.
const (
	DefaultIterations   = 600000
	MinPasswordLength   = 12
	MaxPasswordLength   = 1024
	DefaultSessionLife  = 12 * time.Hour
	DefaultSetupTTL     = 24 * time.Hour
	DefaultRateWindow   = 5 * time.Minute
	DefaultRateMax      = 5
	SessionCookieName   = "esm_session"
	CSRFHeader          = "X-CSRF-Token"
	TokenPrefix         = "esm_"
	secretsDir          = "secrets"
	setupFile           = "secrets/setup.json"
	usersFile           = "secrets/users.json"
	loginsFile          = "secrets/logins.json"
	tokensFile          = "secrets/tokens.json"
	lastUsedPersistStep = time.Minute
)

// Options tune a Manager. Zero values mean the defaults above.
type Options struct {
	Iterations  int           // PBKDF2 iterations for new hashes
	SessionLife time.Duration // how long a session lasts after login
	SetupTTL    time.Duration // lifetime of a setup code
	RateWindow  time.Duration // login failure window
	RateMax     int           // failures allowed per window
	Now         func() time.Time
}

func (o *Options) fill() {
	if o.Iterations <= 0 {
		o.Iterations = DefaultIterations
	}
	if o.SessionLife <= 0 {
		o.SessionLife = DefaultSessionLife
	}
	if o.SetupTTL <= 0 {
		o.SetupTTL = DefaultSetupTTL
	}
	if o.RateWindow <= 0 {
		o.RateWindow = DefaultRateWindow
	}
	if o.RateMax <= 0 {
		o.RateMax = DefaultRateMax
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// User is a stored login.
type User struct {
	Username string       `json:"username"`
	Role     Role         `json:"role"`
	Password PasswordHash `json:"password"`
	Created  time.Time    `json:"created"`
}

// Token is a stored API token. The token itself is never stored.
type Token struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Role     Role      `json:"role"`
	SHA256   string    `json:"sha256"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

// Session is a logged-in browser session. It lasts a fixed time after
// login (Options.SessionLife), however much it is used; the session cookie
// carries the same lifetime as its Max-Age.
type Session struct {
	ID       string // value of the esm_session cookie; never in a reply body
	Username string
	Role     Role
	// CSRF is the value the client must send in X-CSRF-Token: a random
	// value of its own, not derived from the session id.
	CSRF     string
	Created  time.Time
	Expires  time.Time
	LastSeen time.Time
}

// Life is how long the session lasts from its login: the Max-Age of its
// cookie.
func (s *Session) Life() time.Duration { return s.Expires.Sub(s.Created) }

type setupRecord struct {
	SHA256  string    `json:"sha256"`
	Expires time.Time `json:"expires"`
}

type usersDoc struct {
	Users []User `json:"users"`
}

// loginsDoc is logins.json: when each user (by name) last logged in. It is a
// file of its own, not a field of users.json, so that an older ESM, which
// refuses a users.json with a field it does not know, still reads its
// users after a login with this one.
type loginsDoc struct {
	LastLogin map[string]time.Time `json:"last_login"`
}

// Login is a user as the list of logins shows it: never the password hash.
type Login struct {
	Username string
	Role     Role
	Created  time.Time
	// LastLogin is when the user last logged in (zero: never, or not since
	// this ESM began to keep it).
	LastLogin time.Time
	// Sessions is how many sessions of the user are open now.
	Sessions int
}

type tokensDoc struct {
	Tokens []Token `json:"tokens"`
}

// Manager owns users, tokens, sessions and the rate limiter.
type Manager struct {
	st   *store.Store
	opts Options

	mu        sync.Mutex
	users     []User
	tokens    []Token
	persisted map[string]time.Time // token id -> last_used as written to disk
	sessions  map[string]*Session  // sha256(session id) -> session
	lastLogin map[string]time.Time // user name -> last login

	limiter *Limiter
}

// New loads users and tokens from the store.
func New(st *store.Store, opts Options) (*Manager, error) {
	opts.fill()
	m := &Manager{
		st:        st,
		opts:      opts,
		sessions:  make(map[string]*Session),
		persisted: make(map[string]time.Time),
		limiter:   NewLimiter(opts.RateWindow, opts.RateMax, opts.Now),
	}
	if err := st.MkdirAll(secretsDir, 0o700); err != nil {
		return nil, err
	}
	var ud usersDoc
	if err := st.ReadJSON(usersFile, &ud); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("auth: %w", err)
	}
	var td tokensDoc
	if err := st.ReadJSON(tokensFile, &td); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("auth: %w", err)
	}
	m.users = ud.Users
	m.tokens = td.Tokens
	for _, t := range m.tokens {
		m.persisted[t.ID] = t.LastUsed
	}
	// The last logins are for showing only: a file that cannot be read is
	// started anew rather than keeping ESM from starting.
	var ld loginsDoc
	if err := st.ReadJSON(loginsFile, &ld); err == nil && ld.LastLogin != nil {
		m.lastLogin = ld.LastLogin
	} else {
		m.lastLogin = map[string]time.Time{}
	}
	return m, nil
}

// Limiter returns the login rate limiter (shared by login, setup and the
// change of a password).
func (m *Manager) Limiter() *Limiter { return m.limiter }

func (m *Manager) now() time.Time { return m.opts.Now().UTC() }

// HasAdmin reports whether at least one admin user exists.
func (m *Manager) HasAdmin() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasAdminLocked()
}

func (m *Manager) hasAdminLocked() bool {
	for _, u := range m.users {
		if u.Role == RoleAdmin {
			return true
		}
	}
	return false
}

// EnsureSetupCode makes sure a setup code exists while no admin exists.
//
// It returns the new code only when it created one; the caller prints it
// once. While an unexpired code exists it returns "" (only its hash is kept,
// so it cannot be shown again; deleting secrets/setup.json forces a new one
// at the next start). When an admin exists any leftover code is removed.
func (m *Manager) EnsureSetupCode() (code string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasAdminLocked() {
		return "", m.st.Remove(setupFile)
	}
	var rec setupRecord
	err = m.st.ReadJSON(setupFile, &rec)
	if err == nil && m.now().Before(rec.Expires) {
		return "", nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("auth: %w", err)
	}
	var raw [15]byte // 15 bytes = 24 base32 characters, no padding
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	code = base32.StdEncoding.EncodeToString(raw[:])
	rec = setupRecord{SHA256: sha256Hex(normalizeSetupCode(code)), Expires: m.now().Add(m.opts.SetupTTL)}
	if err := m.st.WriteJSON(setupFile, rec, 0o600); err != nil {
		return "", err
	}
	return code, nil
}

func normalizeSetupCode(c string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), "-", ""))
}

// setupKey is the one limiter key of every setup code tried from an
// address shared by many clients (see LimitAddress).
const setupKey = "setup"

// Setup uses the setup code to create the first admin. It works only while
// no admin exists, consumes the code, and counts failures in the rate
// limiter: against the client address addr, or, when addr is "" (an address
// shared by many clients), against one key for all setup attempts.
func (m *Manager) Setup(addr, code, username, password string) (User, error) {
	keys := []string{setupKey}
	if addr != "" {
		keys = []string{"ip:" + addr}
	}
	// The attempt counts as a failure from the start; only attempts that
	// do not test the code (or succeed) give the reservation back.
	release, err := m.limiter.Reserve(keys...)
	if err != nil {
		return User{}, err
	}
	failed := false
	defer func() {
		if !failed {
			release()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasAdminLocked() {
		return User{}, ErrSetupDone
	}
	var rec setupRecord
	if err := m.st.ReadJSON(setupFile, &rec); err != nil {
		failed = true
		return User{}, ErrBadSetupCode
	}
	got := sha256Hex(normalizeSetupCode(code))
	if subtle.ConstantTimeCompare([]byte(got), []byte(rec.SHA256)) != 1 || !m.now().Before(rec.Expires) {
		failed = true
		return User{}, ErrBadSetupCode
	}
	u, err := m.addUserLocked(username, password, RoleAdmin)
	if err != nil {
		return User{}, err
	}
	m.limiter.Reset(keys...)
	if err := m.st.Remove(setupFile); err != nil {
		return u, err
	}
	return u, nil
}

// CreateUser adds a user with the given role.
func (m *Manager) CreateUser(username, password string, role Role) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addUserLocked(username, password, role)
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidUsername reports whether name has the form of a user name.
func ValidUsername(name string) bool { return usernameRe.MatchString(name) }

// Placeholders recorded in place of a name typed at login or setup.
const (
	// InvalidName stands for a name that cannot be a user name.
	InvalidName = "(invalid)"
	// UnknownName stands for a name of no existing user.
	UnknownName = "(unknown)"
)

// AuditName is how a name typed at login or setup may be recorded (in the
// audit log, and so in the event stream): the user's own name when such a
// user exists, else InvalidName or UnknownName. A name that is no user's may
// be a password typed into the wrong field, so it is never kept.
func (m *Manager) AuditName(name string) string {
	if !ValidUsername(name) {
		return InvalidName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Username, name) {
			return u.Username
		}
	}
	return UnknownName
}

// userKey is the limiter key of a name typed at login: a hash of the name
// (any case), so the limiter keeps no typed text; every name that cannot
// be a user name shares one key.
func userKey(name string) string {
	if !ValidUsername(name) {
		return "user:" + InvalidName
	}
	return "user:" + sha256Hex(strings.ToLower(name))[:32]
}

func (m *Manager) addUserLocked(username, password string, role Role) (User, error) {
	if !usernameRe.MatchString(username) {
		return User{}, ErrBadUsername
	}
	if !role.Valid() {
		return User{}, ErrBadRole
	}
	if !role.CanLogin() {
		return User{}, fmt.Errorf("%w: a login's role is read, operator or admin; %s is for API tokens only", ErrBadRole, role)
	}
	if err := CheckPassword(password); err != nil {
		return User{}, err
	}
	for _, u := range m.users {
		if strings.EqualFold(u.Username, username) {
			return User{}, ErrUserExists
		}
	}
	h, err := HashPassword(password, m.opts.Iterations)
	if err != nil {
		return User{}, err
	}
	u := User{Username: username, Role: role, Password: h, Created: m.now()}
	users := append(slices.Clone(m.users), u)
	if err := m.st.WriteJSON(usersFile, usersDoc{Users: users}, 0o600); err != nil {
		return User{}, err
	}
	m.users = users
	return u, nil
}

// Users returns the stored users (password hashes included; do not send
// them anywhere).
func (m *Manager) Users() []User {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.users)
}

// CheckPassword applies the password policy.
func CheckPassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLength {
		return ErrWeakPassword
	}
	if len(pw) > MaxPasswordLength {
		return ErrLongPassword
	}
	return nil
}

// Login checks a username and password and opens a session. Failures count
// against the user name and, unless addr is "", against the client address
// addr; once either has RateMax failures inside the window, attempts are
// refused with *RateLimitError before the password is even checked. The
// caller passes "" for an address that many clients share (see
// LimitAddress): a limit on it would let anyone lock everyone out. Each
// attempt is counted as a failure before the (slow) check runs and given
// back only on success, so parallel attempts cannot get past the limit.
func (m *Manager) Login(addr, username, password string) (*Session, error) {
	keys := []string{userKey(username)}
	if addr != "" {
		keys = append(keys, "ip:"+addr)
	}
	release, err := m.limiter.Reserve(keys...)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	var found *User
	for i := range m.users {
		if strings.EqualFold(m.users[i].Username, username) {
			u := m.users[i]
			found = &u
			break
		}
	}
	m.mu.Unlock()
	ok := false
	if found != nil {
		ok = VerifyPassword(found.Password, password)
	} else {
		// Spend the same time as a real check so unknown names do not
		// answer faster.
		_ = VerifyPassword(dummyHash(m.opts.Iterations), password)
	}
	if !ok {
		return nil, ErrBadCredentials // the reservation stays as the failure
	}
	release()
	m.limiter.Reset(userKey(username))
	s, err := m.newSession(found.Username, found.Role, found.Password)
	if err != nil {
		return nil, err
	}
	m.noteLogin(found.Username)
	return s, nil
}

// noteLogin records a login of username now, in memory and in
// logins.json. Only names of existing users are kept. A failure to write
// the file does not fail the login: the time is kept in memory all the
// same.
func (m *Manager) noteLogin(username string) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]time.Time{}
	for _, u := range m.users {
		if t, ok := m.lastLogin[u.Username]; ok {
			next[u.Username] = t
		}
	}
	next[username] = now
	_ = m.st.WriteJSON(loginsFile, loginsDoc{LastLogin: next}, 0o600)
	m.lastLogin = next
}

// Logins lists the users as the list of logins shows them, in the order
// they were made.
func (m *Manager) Logins() []Login {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Login, 0, len(m.users))
	for _, u := range m.users {
		l := Login{Username: u.Username, Role: u.Role, Created: u.Created, LastLogin: m.lastLogin[u.Username]}
		for _, s := range m.sessions {
			if s.Username == u.Username && now.Before(s.Expires) {
				l.Sessions++
			}
		}
		out = append(out, l)
	}
	return out
}

// userIndexLocked is the index of the user named name (any case), or -1.
func (m *Manager) userIndexLocked(name string) int {
	return slices.IndexFunc(m.users, func(u User) bool { return strings.EqualFold(u.Username, name) })
}

// ChangePassword gives the user username the password next, when current
// is the user's password now, and ends every other session of the user:
// all but the one whose id is keep ("" ends them all). It returns how many
// sessions it ended.
//
// The new password must pass CheckPassword. That is checked first, so a
// new password that is refused tests nothing and counts nothing. A wrong
// current password (ErrWrongPassword) counts against the login rate limit
// exactly like a failed login: against the user name and, unless addr is
// "", the client address addr, with the same keys, so that this call is no
// faster a way to guess a password than the login. While either key is at
// the limit the attempt is refused with *RateLimitError before the
// password is checked, and so is a login after failures here.
func (m *Manager) ChangePassword(addr, username, current, next, keep string) (ended int, err error) {
	if err := CheckPassword(next); err != nil {
		return 0, err
	}
	keys := []string{userKey(username)}
	if addr != "" {
		keys = append(keys, "ip:"+addr)
	}
	release, err := m.limiter.Reserve(keys...)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	i := m.userIndexLocked(username)
	var was PasswordHash
	if i >= 0 {
		was = m.users[i].Password
	}
	m.mu.Unlock()
	if i < 0 {
		_ = VerifyPassword(dummyHash(m.opts.Iterations), current)
		return 0, ErrWrongPassword // the reservation stays as the failure
	}
	if !VerifyPassword(was, current) {
		return 0, ErrWrongPassword // the reservation stays as the failure
	}
	release()
	m.limiter.Reset(userKey(username))
	h, err := HashPassword(next, m.opts.Iterations)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i = m.userIndexLocked(username)
	if i < 0 || m.users[i].Password != was {
		// Changed (or reset) meanwhile: current is no longer the password.
		return 0, ErrWrongPassword
	}
	if err := m.setPasswordLocked(i, h); err != nil {
		return 0, err
	}
	return m.endSessionsLocked(m.users[i].Username, keep), nil
}

// ResetPassword gives the user username (any case) the password next
// without asking for the current one, and ends every session of the user.
// It is for the box owner, who reaches it over the unix socket only (esm
// user passwd): the way back in after a forgotten password. It also forgets
// the failed logins counted against the user name, so the user can log in
// with the new password at once. It returns the user and how many sessions
// it ended; ErrNotFound when there is no such user.
func (m *Manager) ResetPassword(username, next string) (User, int, error) {
	if err := CheckPassword(next); err != nil {
		return User{}, 0, err
	}
	h, err := HashPassword(next, m.opts.Iterations)
	if err != nil {
		return User{}, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.userIndexLocked(username)
	if i < 0 {
		return User{}, 0, ErrNotFound
	}
	if err := m.setPasswordLocked(i, h); err != nil {
		return User{}, 0, err
	}
	u := m.users[i]
	m.limiter.Reset(userKey(u.Username))
	return u, m.endSessionsLocked(u.Username, ""), nil
}

// setPasswordLocked stores h as the password of the user at index i.
func (m *Manager) setPasswordLocked(i int, h PasswordHash) error {
	users := slices.Clone(m.users)
	users[i].Password = h
	if err := m.st.WriteJSON(usersFile, usersDoc{Users: users}, 0o600); err != nil {
		return err
	}
	m.users = users
	return nil
}

// endSessionsLocked ends every session of the user username except the one
// whose id is keep, and returns how many of them had not expired yet.
func (m *Manager) endSessionsLocked(username, keep string) int {
	keepKey := ""
	if keep != "" {
		keepKey = sha256Hex(keep)
	}
	now := m.now()
	n := 0
	for k, s := range m.sessions {
		if k == keepKey || !strings.EqualFold(s.Username, username) {
			continue
		}
		if now.Before(s.Expires) {
			n++
		}
		delete(m.sessions, k)
	}
	return n
}

var (
	dummyOnce sync.Once
	dummy     PasswordHash
)

func dummyHash(iter int) PasswordHash {
	dummyOnce.Do(func() {
		dummy, _ = HashPassword("not-a-real-password-at-all", iter)
	})
	return dummy
}

// newSession opens a session for the user whose password was checked
// against verified. When the password changed while the check ran (a
// change or a reset), no session is opened: it would outlive the change,
// which ends the sessions made with the old password.
func (m *Manager) newSession(username string, role Role, verified PasswordHash) (*Session, error) {
	// A browser session never has the site role, whatever users.json says
	// (a hand-edited file): that role is for API tokens only.
	if !role.CanLogin() {
		return nil, fmt.Errorf("%w: a login cannot have the %q role, which is for API tokens only", ErrForbidden, role)
	}
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := m.now()
	s := &Session{ID: id, Username: username, Role: role, CSRF: csrf, Created: now,
		Expires: now.Add(m.opts.SessionLife), LastSeen: now}
	m.PruneSessions()
	m.mu.Lock()
	if i := m.userIndexLocked(username); i < 0 || m.users[i].Password != verified {
		m.mu.Unlock()
		return nil, ErrBadCredentials
	}
	m.sessions[sha256Hex(id)] = s
	m.mu.Unlock()
	cp := *s
	return &cp, nil
}

// Session looks up a session by cookie value. A session past its expiry
// is dropped.
func (m *Manager) Session(id string) (*Session, error) {
	if id == "" {
		return nil, ErrNoSession
	}
	key := sha256Hex(id)
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok {
		return nil, ErrNoSession
	}
	if !now.Before(s.Expires) {
		delete(m.sessions, key)
		return nil, ErrNoSession
	}
	s.LastSeen = now
	cp := *s
	return &cp, nil
}

// Logout ends a session. Unknown ids are ignored.
func (m *Manager) Logout(id string) {
	m.mu.Lock()
	delete(m.sessions, sha256Hex(id))
	m.mu.Unlock()
}

// PruneSessions drops expired sessions. Every login does it first, so
// sessions that are never used again do not pile up.
func (m *Manager) PruneSessions() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if !now.Before(s.Expires) {
			delete(m.sessions, k)
		}
	}
}

// CreateToken makes a new API token. The returned plain text is the only
// copy: only its SHA-256 is stored.
func (m *Manager) CreateToken(name string, role Role) (plain string, tok Token, err error) {
	if !role.Valid() {
		return "", Token{}, ErrBadRole
	}
	if len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", Token{}, errors.New("token name must be at most 64 printable characters")
	}
	secret, err := randomToken(32)
	if err != nil {
		return "", Token{}, err
	}
	plain = TokenPrefix + secret
	var idb [6]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return "", Token{}, err
	}
	tok = Token{
		ID:      "tok_" + hex.EncodeToString(idb[:]),
		Name:    name,
		Role:    role,
		SHA256:  sha256Hex(plain),
		Created: m.now(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	tokens := append(slices.Clone(m.tokens), tok)
	if err := m.st.WriteJSON(tokensFile, tokensDoc{Tokens: tokens}, 0o600); err != nil {
		return "", Token{}, err
	}
	m.tokens = tokens
	m.persisted[tok.ID] = time.Time{}
	return plain, tok, nil
}

// Tokens lists tokens (hashes included; the API should drop them).
func (m *Manager) Tokens() []Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tokens)
}

// DeleteToken removes a token by id.
func (m *Manager) DeleteToken(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.tokens, func(t Token) bool { return t.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	tokens := slices.Delete(slices.Clone(m.tokens), i, i+1)
	if err := m.st.WriteJSON(tokensFile, tokensDoc{Tokens: tokens}, 0o600); err != nil {
		return err
	}
	m.tokens = tokens
	delete(m.persisted, id)
	return nil
}

// TokenRole returns the role of the token with this id, or ErrBadToken
// once the token is deleted.
func (m *Manager) TokenRole(id string) (Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.ID == id {
			return t.Role, nil
		}
	}
	return "", ErrBadToken
}

// AuthenticateToken checks a bearer token. Every stored hash is compared in
// constant time; last_used is refreshed (and written at most once a minute).
func (m *Manager) AuthenticateToken(plain string) (Token, error) {
	if !strings.HasPrefix(plain, TokenPrefix) || len(plain) > 128 {
		return Token{}, ErrBadToken
	}
	h := []byte(sha256Hex(plain))
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	idx := -1
	for i := range m.tokens {
		if subtle.ConstantTimeCompare(h, []byte(m.tokens[i].SHA256)) == 1 {
			idx = i
		}
	}
	if idx < 0 {
		return Token{}, ErrBadToken
	}
	m.tokens[idx].LastUsed = now
	tok := m.tokens[idx]
	if now.Sub(m.persisted[tok.ID]) >= lastUsedPersistStep {
		if err := m.st.WriteJSON(tokensFile, tokensDoc{Tokens: m.tokens}, 0o600); err == nil {
			m.persisted[tok.ID] = now
		}
	}
	return tok, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StatusCode maps an error from this package to an HTTP status.
func StatusCode(err error) int {
	var rl *RateLimitError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &rl):
		return http.StatusTooManyRequests
	case errors.Is(err, ErrBadCredentials), errors.Is(err, ErrBadSetupCode),
		errors.Is(err, ErrBadToken), errors.Is(err, ErrNoSession):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrCSRF), errors.Is(err, ErrOrigin), errors.Is(err, ErrWrongPassword):
		// A wrong current password is not 401: the caller is logged in.
		return http.StatusForbidden
	case errors.Is(err, ErrSetupDone), errors.Is(err, ErrUserExists):
		return http.StatusConflict
	case errors.Is(err, ErrWeakPassword), errors.Is(err, ErrLongPassword), errors.Is(err, ErrBadUsername), errors.Is(err, ErrBadRole):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrNotJSON):
		return http.StatusUnsupportedMediaType
	default:
		return http.StatusInternalServerError
	}
}
