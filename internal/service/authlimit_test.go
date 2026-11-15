package service

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
)

// counting is an HTTP client that counts the connections it opens.
type counting struct {
	c     *http.Client
	conns atomic.Int32
}

func newCounting(tc *tls.Config) *counting {
	return &counting{c: &http.Client{Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tc, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}}}
}

func (cc *counting) do(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if !info.Reused {
				cc.conns.Add(1)
			}
		}}))
	resp, err := cc.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// A flood of calls with a wrong token over one kept-alive connection is
// answered 429 from the call after AuthFailMax on, at once, and leaves two
// entries in audit.log however long it goes on. A valid token from the
// same address goes on working. On the API listener every 401 and 429 ends
// the connection, so a client there needs a new TLS connection for each
// try; the limit is the address's on every listener.
func TestFloodOfWrongTokens(t *testing.T) {
	root := shortRoot(t)
	writeConfig(t, root, map[string]any{"api_listener": map[string]any{"listen": "127.0.0.1:0"}})
	s := startWith(t, root, &syncBuffer{})
	defer stop(t, s)
	admin, _, _ := s.auth.CreateToken("panel", auth.RoleAdmin)
	wrong := "esm_" + strings.Repeat("z", 43)
	info := s.APIListener()
	apiURL := "https://" + info.Listen

	// The API listener: a 401 ends the connection.
	remote := newCounting(pinned(info.Certificate.SPKIPin))
	for i := range 3 {
		resp := remote.do(t, "GET", apiURL+"/api/v1/session", wrong)
		if resp.StatusCode != http.StatusUnauthorized || !resp.Close {
			t.Fatalf("API listener, try %d: %d close=%v", i, resp.StatusCode, resp.Close)
		}
	}
	if n := remote.conns.Load(); n != 3 {
		t.Fatalf("3 tries on the API listener took %d connections, want 3", n)
	}

	// The main listener, one connection.
	main := newCounting(nil)
	const calls = 3000
	start := time.Now()
	first429 := 0
	var limitedAfter time.Duration
	for i := 4; i <= calls; i++ {
		resp := main.do(t, "POST", "http://"+s.Addr()+"/api/v1/instances/x/start", wrong)
		if resp.StatusCode == http.StatusTooManyRequests && first429 == 0 {
			first429, limitedAfter = i, time.Since(start)
			if resp.Header.Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
		}
		if first429 != 0 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("call %d after the limit: %d", i, resp.StatusCode)
		}
	}
	t.Logf("%d calls in %s, the first 429 with call %d after %s", calls, time.Since(start).Round(time.Millisecond), first429, limitedAfter.Round(time.Millisecond))
	if first429 != api.AuthFailMax+1 || limitedAfter > 5*time.Second {
		t.Fatalf("the first 429 with call %d after %s, want call %d", first429, limitedAfter, api.AuthFailMax+1)
	}
	if n := main.conns.Load(); n != 1 {
		t.Fatalf("the flood took %d connections, want 1", n)
	}
	// A valid token from the same address works, on both listeners.
	if resp := main.do(t, "GET", "http://"+s.Addr()+"/api/v1/session", admin); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token, main listener: %d", resp.StatusCode)
	}
	if resp := remote.do(t, "GET", apiURL+"/api/v1/session", admin); resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("valid token, API listener: %d close=%v", resp.StatusCode, resp.Close)
	}
	// The API listener answers the address 429, and ends the connection.
	if resp := remote.do(t, "POST", apiURL+"/api/v1/instances/x/start", wrong); resp.StatusCode != http.StatusTooManyRequests || !resp.Close {
		t.Fatalf("API listener while limited: %d close=%v", resp.StatusCode, resp.Close)
	}

	// audit.log: the first failure and the limit.
	b, err := os.ReadFile(filepath.Join(root, audit.FileName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var actions []string
	for _, l := range lines {
		for _, a := range []string{"instance.start", "auth.limited", "token.create"} {
			if strings.Contains(l, `"action":"`+a+`"`) {
				actions = append(actions, a)
			}
		}
	}
	if fmt.Sprint(actions) != "[instance.start auth.limited]" || len(b) > 4<<10 {
		t.Fatalf("audit.log after %d calls (%d bytes): %q", calls, len(b), actions)
	}
}
