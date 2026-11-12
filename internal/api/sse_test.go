package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
)

// openStream opens GET /api/v1/events with the given request header and
// returns a channel of the lines it reads; the channel closes when the
// stream ends.
func openStream(t *testing.T, srv *httptest.Server, set func(*http.Request)) <-chan string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	set(req)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}
	lines := make(chan string, 100)
	go func() {
		defer close(lines)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for l := range lines {
		if l == ": connected" {
			return lines
		}
	}
	t.Fatal("the stream ended before it connected")
	return nil
}

// endsWithout waits for the stream to end and fails if it delivered an
// event first.
func endsWithout(t *testing.T, what string, lines <-chan string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return
			}
			if strings.HasPrefix(l, "event:") || strings.HasPrefix(l, "data:") {
				t.Fatalf("%s: the stream delivered %q", what, l)
			}
		case <-timeout:
			t.Fatalf("%s: the stream is still open", what)
		}
	}
}

// An event stream lasts only as long as the login it was opened with:
// once its token is deleted, or its session logged out, the next event is
// not sent and the stream ends; with no events, the next keep-alive ends
// it.
func TestEventStreamEndsWithItsLogin(t *testing.T) {
	for _, keepAlive := range []time.Duration{time.Hour, 50 * time.Millisecond} {
		rg := newRig(t, Options{KeepAlive: keepAlive})
		srv := httptest.NewServer(rg.s.Handler())
		publish := func() {
			if keepAlive == time.Hour {
				// Give the stream time to see the change only through the
				// event itself.
				time.Sleep(50 * time.Millisecond)
				rg.bus.Publish(events.TypeState, "alpha", map[string]string{"state": "running"})
			}
		}

		plain, tok, err := rg.auth.CreateToken("t", auth.RoleAdmin)
		if err != nil {
			t.Fatal(err)
		}
		lines := openStream(t, srv, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+plain) })
		if err := rg.auth.DeleteToken(tok.ID); err != nil {
			t.Fatal(err)
		}
		publish()
		endsWithout(t, "deleted token", lines)

		if _, err := rg.auth.CreateUser("admin", "correct horse battery", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		sess, err := rg.auth.Login("", "admin", "correct horse battery")
		if err != nil {
			t.Fatal(err)
		}
		lines = openStream(t, srv, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.ID})
		})
		rg.auth.Logout(sess.ID)
		publish()
		endsWithout(t, "logged out", lines)
		srv.Close()
	}
}
