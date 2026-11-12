// Package auth holds ESM's logins: the one-time setup code, users with
// PBKDF2 password hashes, browser sessions, API tokens, roles, the login rate
// limit and the CSRF and Origin checks for cookie requests.
//
// Files live under <root>/secrets/ (directory 0700, files 0600):
//
//	setup.json   hash and expiry of the one-time setup code
//	users.json   users and password hashes
//	tokens.json  API tokens (only their SHA-256 is stored)
//
// Sessions are kept in memory only.
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
	ErrBadUsername    = errors.New("username must be 1-64 characters: letters, digits, '.', '_' or '-', starting with a letter or digit")
	ErrUserExists     = errors.New("user already exists")
	ErrBadRole        = errors.New("role must be read, operator or admin")
	ErrNotFound       = errors.New("not found")
	ErrBadToken       = errors.New("invalid token")
	ErrNoSession      = errors.New("not logged in")
	ErrForbidden      = errors.New("forbidden")
	ErrCSRF           = errors.New("missing or wrong X-CSRF-Token header")
	ErrOrigin         = errors.New("origin header does not match the host")
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
	DefaultSessionIdle  = 12 * time.Hour
	DefaultSetupTTL     = 24 * time.Hour
	DefaultRateWindow   = 5 * time.Minute
	DefaultRateMax      = 5
	SessionCookieName   = "esm_session"
	CSRFHeader          = "X-CSRF-Token"
	TokenPrefix         = "esm_"
	secretsDir          = "secrets"
	setupFile           = "secrets/setup.json"
	usersFile           = "secrets/users.json"
	tokensFile          = "secrets/tokens.json"
	lastUsedPersistStep = time.Minute
)

// Options tune a Manager. Zero values mean the defaults above.
type Options struct {
	Iterations  int           // PBKDF2 iterations for new hashes
	SessionIdle time.Duration // idle expiry of sessions
	SetupTTL    time.Duration // lifetime of a setup code
	RateWindow  time.Duration // login failure window
	RateMax     int           // failures allowed per window
	Now         func() time.Time
}

func (o *Options) fill() {
	if o.Iterations <= 0 {
		o.Iterations = DefaultIterations
	}
	if o.SessionIdle <= 0 {
		o.SessionIdle = DefaultSessionIdle
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

// Session is a logged-in browser session.
type Session struct {
	ID       string // value of the esm_session cookie
	Username string
	Role     Role
	CSRF     string // value the client must send in X-CSRF-Token
	Created  time.Time
	LastSeen time.Time
}

type setupRecord struct {
	SHA256  string    `json:"sha256"`
	Expires time.Time `json:"expires"`
}

type usersDoc struct {
	Users []User `json:"users"`
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
	return m, nil
}

// Limiter returns the login rate limiter (shared by login and setup).
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

// Setup uses the setup code to create the first admin. It works only while
// no admin exists, consumes the code, and counts failures against clientIP
// in the rate limiter.
func (m *Manager) Setup(clientIP, code, username, password string) (User, error) {
	keys := []string{"ip:" + clientIP}
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

func (m *Manager) addUserLocked(username, password string, role Role) (User, error) {
	if !usernameRe.MatchString(username) {
		return User{}, ErrBadUsername
	}
	if !role.Valid() {
		return User{}, ErrBadRole
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
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	}
	return nil
}

// Login checks a username and password and opens a session. Failures count
// against both the client IP and the username; once either has RateMax
// failures inside the window, attempts are refused with *RateLimitError
// before the password is even checked. Each attempt is counted as a failure
// before the (slow) check runs and given back only on success, so parallel
// attempts cannot get past the limit.
func (m *Manager) Login(clientIP, username, password string) (*Session, error) {
	keys := []string{"ip:" + clientIP, "user:" + strings.ToLower(username)}
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
	m.limiter.Reset("user:" + strings.ToLower(username))
	return m.newSession(found.Username, found.Role)
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

func (m *Manager) newSession(username string, role Role) (*Session, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := m.now()
	s := &Session{ID: id, Username: username, Role: role, CSRF: csrf, Created: now, LastSeen: now}
	m.mu.Lock()
	m.sessions[sha256Hex(id)] = s
	m.mu.Unlock()
	cp := *s
	return &cp, nil
}

// Session looks up a session by cookie value and refreshes its idle timer.
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
	if now.Sub(s.LastSeen) > m.opts.SessionIdle {
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

// PruneSessions drops sessions idle for longer than the idle expiry.
func (m *Manager) PruneSessions() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if now.Sub(s.LastSeen) > m.opts.SessionIdle {
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
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrCSRF), errors.Is(err, ErrOrigin):
		return http.StatusForbidden
	case errors.Is(err, ErrSetupDone), errors.Is(err, ErrUserExists):
		return http.StatusConflict
	case errors.Is(err, ErrWeakPassword), errors.Is(err, ErrBadUsername), errors.Is(err, ErrBadRole):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
