package auth

import (
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

const goodPassword = "correct horse battery"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newManager(t *testing.T) (*Manager, *store.Store, *clock) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	m, err := New(st, Options{Iterations: 1000, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return m, st, c
}

func TestSetupCodeWorksOnce(t *testing.T) {
	m, st, _ := newManager(t)
	if m.HasAdmin() {
		t.Fatal("fresh manager has an admin")
	}
	code, err := m.EnsureSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 24 {
		t.Fatalf("code %q has length %d, want 24", code, len(code))
	}
	if _, err := base32.StdEncoding.DecodeString(code); err != nil {
		t.Fatalf("code %q is not base32: %v", code, err)
	}
	raw, err := st.ReadFile(setupFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), code) {
		t.Fatal("setup.json stores the code itself")
	}
	again, err := m.EnsureSetupCode()
	if err != nil || again != "" {
		t.Fatalf("second EnsureSetupCode = %q, %v; want no new code", again, err)
	}
	if _, err := m.Setup("192.0.2.1", "AAAAAAAAAAAAAAAAAAAAAAAA", "admin", goodPassword); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := m.Setup("192.0.2.1", code, "admin", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	u, err := m.Setup("192.0.2.1", strings.ToLower(code), "admin", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleAdmin || !m.HasAdmin() {
		t.Fatalf("setup made %+v", u)
	}
	if st.Exists(setupFile) {
		t.Fatal("setup.json still exists after setup")
	}
	if _, err := m.Setup("192.0.2.1", code, "admin2", goodPassword); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup: %v", err)
	}
	if c, err := m.EnsureSetupCode(); c != "" || err != nil {
		t.Fatalf("code issued while an admin exists: %q %v", c, err)
	}
	// The admin survives a reload.
	m2, err := New(st, Options{Iterations: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !m2.HasAdmin() {
		t.Fatal("admin lost on reload")
	}
}

func TestSetupCodeExpires(t *testing.T) {
	m, _, c := newManager(t)
	code, err := m.EnsureSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	c.Add(24*time.Hour + time.Second)
	if _, err := m.Setup("192.0.2.1", code, "admin", goodPassword); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("expired code accepted: %v", err)
	}
	fresh, err := m.EnsureSetupCode()
	if err != nil || fresh == "" || fresh == code {
		t.Fatalf("expired code not replaced: %q %v", fresh, err)
	}
	if _, err := m.Setup("192.0.2.1", fresh, "admin", goodPassword); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordHashDefaults(t *testing.T) {
	h, err := HashPassword(goodPassword, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Algo != "pbkdf2-sha256" || h.Iterations != 600000 {
		t.Fatalf("hash params %+v", h)
	}
	salt, _ := base64.StdEncoding.DecodeString(h.Salt)
	if len(salt) != 16 {
		t.Fatalf("salt length %d", len(salt))
	}
	if !VerifyPassword(h, goodPassword) {
		t.Fatal("correct password refused")
	}
	if VerifyPassword(h, goodPassword+"x") {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword(goodPassword, 1000)
	if h2.Salt == h.Salt || h2.Hash == h.Hash {
		t.Fatal("salts repeat")
	}
	bad := h2
	bad.Iterations = 999
	if VerifyPassword(bad, goodPassword) {
		t.Fatal("tampered iteration count accepted")
	}
	bad = h2
	bad.Algo = "md5"
	if VerifyPassword(bad, goodPassword) {
		t.Fatal("unknown algorithm accepted")
	}
	bad = h2
	bad.Iterations = 1 << 30
	if VerifyPassword(bad, goodPassword) {
		t.Fatal("absurd iteration count accepted")
	}
}

func TestUsersFileHasNoPlainPassword(t *testing.T) {
	m, st, _ := newManager(t)
	if _, err := m.CreateUser("ops", goodPassword, RoleOperator); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.ReadFile(usersFile)
	if strings.Contains(string(raw), goodPassword) {
		t.Fatal("users.json contains the password")
	}
	if _, err := m.CreateUser("OPS", goodPassword, RoleRead); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate user (case-insensitive): %v", err)
	}
	for _, name := range []string{"", "-x", "a b", strings.Repeat("a", 65), "x/y"} {
		if _, err := m.CreateUser(name, goodPassword, RoleRead); !errors.Is(err, ErrBadUsername) {
			t.Errorf("username %q: %v", name, err)
		}
	}
	if _, err := m.CreateUser("x", goodPassword, "root"); !errors.Is(err, ErrBadRole) {
		t.Fatalf("bad role: %v", err)
	}
}

func TestLoginSessionsAndIdleExpiry(t *testing.T) {
	m, _, c := newManager(t)
	if _, err := m.CreateUser("admin", goodPassword, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Login("192.0.2.1", "admin", "wrong password!"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := m.Login("192.0.2.1", "nobody", goodPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	s, err := m.Login("192.0.2.1", "Admin", goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != RoleAdmin || s.ID == "" || s.CSRF == "" || s.ID == s.CSRF {
		t.Fatalf("session %+v", s)
	}
	c.Add(11 * time.Hour)
	if _, err := m.Session(s.ID); err != nil {
		t.Fatalf("session expired early: %v", err)
	}
	c.Add(11 * time.Hour) // 11h after last use: still fine
	if _, err := m.Session(s.ID); err != nil {
		t.Fatalf("idle timer not refreshed: %v", err)
	}
	c.Add(12*time.Hour + time.Second)
	if _, err := m.Session(s.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle session still valid: %v", err)
	}
	s2, _ := m.Login("192.0.2.1", "admin", goodPassword)
	m.Logout(s2.ID)
	if _, err := m.Session(s2.ID); !errors.Is(err, ErrNoSession) {
		t.Fatal("session valid after logout")
	}
	if _, err := m.Session(""); !errors.Is(err, ErrNoSession) {
		t.Fatal("empty session id accepted")
	}
}

func TestTokensAndRoles(t *testing.T) {
	m, st, c := newManager(t)
	plains := map[Role]string{}
	for _, r := range []Role{RoleRead, RoleOperator, RoleAdmin} {
		p, tok, err := m.CreateToken("ci-"+string(r), r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p, "esm_") || len(p) != len("esm_")+43 {
			t.Fatalf("token %q has wrong shape", p)
		}
		if _, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(p, "esm_")); err != nil {
			t.Fatalf("token body not base64url: %v", err)
		}
		if tok.Role != r || tok.SHA256 == "" || !strings.HasPrefix(tok.ID, "tok_") {
			t.Fatalf("token record %+v", tok)
		}
		plains[r] = p
	}
	raw, _ := st.ReadFile(tokensFile)
	for _, p := range plains {
		if strings.Contains(string(raw), p) {
			t.Fatal("tokens.json stores a plain token")
		}
	}
	for r, p := range plains {
		tok, err := m.AuthenticateToken(p)
		if err != nil || tok.Role != r {
			t.Fatalf("auth %s: %+v %v", r, tok, err)
		}
		if tok.LastUsed.IsZero() {
			t.Fatal("last_used not set")
		}
	}
	for _, bad := range []string{"", "esm_", "esm_nope", plains[RoleRead] + "x", "Bearer " + plains[RoleRead]} {
		if _, err := m.AuthenticateToken(bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("token %q accepted: %v", bad, err)
		}
	}
	// Role matrix.
	type tc struct {
		have, need Role
		ok         bool
	}
	for _, x := range []tc{
		{RoleRead, RoleRead, true}, {RoleRead, RoleOperator, false}, {RoleRead, RoleAdmin, false},
		{RoleOperator, RoleRead, true}, {RoleOperator, RoleOperator, true}, {RoleOperator, RoleAdmin, false},
		{RoleAdmin, RoleRead, true}, {RoleAdmin, RoleOperator, true}, {RoleAdmin, RoleAdmin, true},
		{"", RoleRead, false}, {RoleAdmin, "", false},
	} {
		if got := x.have.Allows(x.need); got != x.ok {
			t.Errorf("%q.Allows(%q) = %v", x.have, x.need, got)
		}
	}
	if _, err := ParseRole("superuser"); !errors.Is(err, ErrBadRole) {
		t.Fatal("bad role parsed")
	}
	if _, _, err := m.CreateToken("x", "god"); !errors.Is(err, ErrBadRole) {
		t.Fatal("token with bad role created")
	}
	// Reload keeps tokens; delete removes them.
	c.Add(time.Minute)
	m2, err := New(st, Options{Iterations: 1000, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := m2.AuthenticateToken(plains[RoleOperator])
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.DeleteToken(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.AuthenticateToken(plains[RoleOperator]); !errors.Is(err, ErrBadToken) {
		t.Fatal("deleted token still works")
	}
	if err := m2.DeleteToken(tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete not reported")
	}
	if len(m2.Tokens()) != 2 {
		t.Fatalf("tokens left: %d", len(m2.Tokens()))
	}
}

func TestLoginRateLimit(t *testing.T) {
	m, _, c := newManager(t)
	if _, err := m.CreateUser("admin", goodPassword, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := m.Login("192.0.2.1", "admin", "wrong password "+string(rune('a'+i))); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		c.Add(time.Second)
	}
	_, err := m.Login("192.0.2.1", "admin", goodPassword)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("6th attempt not limited: %v", err)
	}
	if StatusCode(err) != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", StatusCode(err))
	}
	if rl.RetryAfter <= 0 || rl.RetryAfter > 5*time.Minute {
		t.Fatalf("retry after %v", rl.RetryAfter)
	}
	// Per username: another IP is limited for this user too.
	if _, err := m.Login("192.0.2.2", "admin", goodPassword); !errors.As(err, &rl) {
		t.Fatalf("username limit not shared across IPs: %v", err)
	}
	// After the window the right password works again.
	c.Add(5 * time.Minute)
	if _, err := m.Login("192.0.2.1", "admin", goodPassword); err != nil {
		t.Fatalf("after window: %v", err)
	}

	// Per IP: failures against many usernames from one IP.
	for i := range 5 {
		m.Login("192.0.2.9", "user"+string(rune('a'+i)), "whatever whatever")
	}
	if _, err := m.Login("192.0.2.9", "admin", goodPassword); !errors.As(err, &rl) {
		t.Fatalf("IP limit not applied: %v", err)
	}
	if _, err := m.Login("192.0.2.10", "admin", goodPassword); err != nil {
		t.Fatalf("other IP blocked: %v", err)
	}
}

func TestSetupCodeAttemptsAreLimited(t *testing.T) {
	m, _, _ := newManager(t)
	code, _ := m.EnsureSetupCode()
	for range 5 {
		m.Setup("192.0.2.1", "WRONGWRONGWRONGWRONGWRON", "admin", goodPassword)
	}
	if _, err := m.Setup("192.0.2.1", code, "admin", goodPassword); StatusCode(err) != http.StatusTooManyRequests {
		t.Fatalf("setup attempts not limited: %v", err)
	}
}

func TestCSRFRequiredForCookiePost(t *testing.T) {
	s := &Session{ID: "sid", CSRF: "csrf-value-123"}
	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8740/api/v1/instances/a/start", nil)
	post.AddCookie(SessionCookie(s.ID, false))
	err := CheckCookieRequest(post, s)
	if !errors.Is(err, ErrCSRF) || StatusCode(err) != http.StatusForbidden {
		t.Fatalf("POST without CSRF: %v (status %d)", err, StatusCode(err))
	}
	post.Header.Set(CSRFHeader, "wrong")
	if err := CheckCookieRequest(post, s); !errors.Is(err, ErrCSRF) {
		t.Fatalf("POST with wrong CSRF: %v", err)
	}
	post.Header.Set(CSRFHeader, s.CSRF)
	if err := CheckCookieRequest(post, s); err != nil {
		t.Fatalf("POST with CSRF: %v", err)
	}
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		r := httptest.NewRequest(method, "http://127.0.0.1:8740/api/v1/x", nil)
		if err := CheckCookieRequest(r, s); !errors.Is(err, ErrCSRF) {
			t.Errorf("%s without CSRF accepted", method)
		}
	}
	get := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8740/api/v1/instances", nil)
	if err := CheckCookieRequest(get, s); err != nil {
		t.Fatalf("GET refused: %v", err)
	}
	if err := CheckCSRF(&Session{}, ""); !errors.Is(err, ErrCSRF) {
		t.Fatal("empty CSRF on both sides accepted")
	}
}

func TestOriginMismatchRefused(t *testing.T) {
	s := &Session{ID: "sid", CSRF: "c"}
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8740/api/v1/logout", nil)
	r.Header.Set(CSRFHeader, "c")
	r.Header.Set("Origin", "http://evil.example")
	if err := CheckCookieRequest(r, s); !errors.Is(err, ErrOrigin) || StatusCode(err) != http.StatusForbidden {
		t.Fatalf("mismatched origin: %v", err)
	}
	r.Header.Set("Origin", "http://127.0.0.1:8740")
	if err := CheckCookieRequest(r, s); err != nil {
		t.Fatalf("matching origin refused: %v", err)
	}
	cases := []struct {
		origin, host string
		ok           bool
	}{
		{"", "127.0.0.1:8740", true},
		{"https://esm.example:443", "esm.example", true},
		{"https://esm.example", "esm.example:443", true},
		{"http://ESM.example:8080", "esm.example:8080", true},
		{"http://127.0.0.1:8741", "127.0.0.1:8740", false},
		{"null", "127.0.0.1:8740", false},
		{"file://x", "x", false},
		{"http://[::1]:8740", "[::1]:8740", true},
	}
	for _, c := range cases {
		if err := CheckOrigin(c.origin, c.host); (err == nil) != c.ok {
			t.Errorf("CheckOrigin(%q, %q) = %v, want ok=%v", c.origin, c.host, err, c.ok)
		}
	}
}

func TestCookieAndHeaderHelpers(t *testing.T) {
	c := SessionCookie("abc", true)
	if c.Name != "esm_session" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie %+v", c)
	}
	if SessionCookie("abc", false).Secure {
		t.Fatal("Secure set without TLS")
	}
	if ClearSessionCookie(false).MaxAge >= 0 {
		t.Fatal("clear cookie does not expire")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "192.0.2.7:5555"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if ClientIP(r) != "192.0.2.7" {
		t.Fatalf("client ip %q", ClientIP(r))
	}
	r.Header.Set("Authorization", "Bearer esm_abc")
	if tok, ok := BearerToken(r); !ok || tok != "esm_abc" {
		t.Fatalf("bearer %q %v", tok, ok)
	}
	r.Header.Set("Authorization", "Basic xyz")
	if _, ok := BearerToken(r); ok {
		t.Fatal("basic auth read as bearer")
	}
}

func TestStatusCodes(t *testing.T) {
	cases := map[error]int{
		nil:                  200,
		ErrBadCredentials:    401,
		ErrBadToken:          401,
		ErrForbidden:         403,
		ErrCSRF:              403,
		ErrSetupDone:         409,
		ErrWeakPassword:      422,
		ErrNotFound:          404,
		&RateLimitError{}:    429,
		errors.New("boom"):   500,
		ErrBadSetupCode:      401,
		ErrUserExists:        409,
		ErrBadRole:           422,
		ErrNoSession:         401,
		ErrOrigin:            403,
		ErrBadUsername:       422,
		errors.Join(ErrCSRF): 403,
		&RateLimitError{1e9}: 429,
	}
	for err, want := range cases {
		if got := StatusCode(err); got != want {
			t.Errorf("StatusCode(%v) = %d, want %d", err, got, want)
		}
	}
}

// parallel runs n attempts at once and counts the outcomes.
func parallel(n int, attempt func(i int) error) (checked, limited int, other []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := attempt(i)
			var rl *RateLimitError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.As(err, &rl):
				limited++
			case errors.Is(err, ErrBadCredentials), errors.Is(err, ErrBadSetupCode):
				checked++
			default:
				other = append(other, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return checked, limited, other
}

func TestParallelLoginsAreLimited(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Slow enough hashing that the attempts really overlap.
	m, err := New(st, Options{Iterations: 20000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateUser("admin", "correct horse battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(i int) error{
		"one client, one user": func(int) error {
			_, err := m.Login("192.0.2.1", "admin", "wrong password!!")
			return err
		},
		"many clients, one user": func(i int) error {
			_, err := m.Login(fmt.Sprintf("198.51.100.%d", i), "Admin", "wrong password!!")
			return err
		},
		"one client, many users": func(i int) error {
			_, err := m.Login("203.0.113.9", fmt.Sprintf("user%d", i), "wrong password!!")
			return err
		},
	}
	for name, attempt := range cases {
		m.Limiter().Reset("user:admin")
		checked, limited, other := parallel(40, attempt)
		if len(other) > 0 || checked > DefaultRateMax || checked+limited != 40 {
			t.Errorf("%s: %d password checks, %d refused, other %v; want at most %d checks", name, checked, limited, other, DefaultRateMax)
		}
	}
}

func TestParallelSetupCodesAreLimited(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.EnsureSetupCode(); err != nil {
		t.Fatal(err)
	}
	checked, limited, other := parallel(40, func(int) error {
		_, err := m.Setup("192.0.2.7", "AAAAAAAAAAAAAAAAAAAAAAAA", "admin", "correct horse battery")
		return err
	})
	if len(other) > 0 || checked > DefaultRateMax || checked+limited != 40 {
		t.Fatalf("%d code checks, %d refused, other %v; want at most %d checks", checked, limited, other, DefaultRateMax)
	}
}

func TestSuccessGivesTheAttemptBack(t *testing.T) {
	m, _, _ := newManager(t)
	if _, err := m.CreateUser("admin", "correct horse battery", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DefaultRateMax-1; i++ {
		if _, err := m.Login("192.0.2.3", "admin", "wrong password!!"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := m.Login("192.0.2.3", "admin", "correct horse battery"); err != nil {
			t.Fatalf("good login %d after %d failures: %v", i, DefaultRateMax-1, err)
		}
	}
	// The client still has room for exactly one more failure.
	if _, err := m.Login("192.0.2.3", "nobody", "wrong password!!"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("fifth failure: %v", err)
	}
	var rl *RateLimitError
	if _, err := m.Login("192.0.2.3", "admin", "correct horse battery"); !errors.As(err, &rl) {
		t.Fatalf("after five failures: %v", err)
	}
}
