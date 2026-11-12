package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/web"
)

// redirect stands for any redirect status in the cases below.
const redirect = 3

// Every reply ESM writes, to the page or the API, success or error, carries
// the security headers, frame-ancestors 'none' among them. That includes
// the redirects of the router, which cleans paths such as "/a/../b".
func TestEveryReplyHasSecurityHeaders(t *testing.T) {
	h := New(Options{Page: stubPage}).Handler()
	big := `{"username":"` + strings.Repeat("a", MaxBody) + `"}`
	cases := []struct {
		method, target, ctype, body string
		status                      int
	}{
		{"GET", "/", "", "", http.StatusOK},
		{"HEAD", "/", "", "", http.StatusOK},
		{"GET", "/style.css", "", "", http.StatusOK},
		{"GET", "/instances/alpha", "", "", http.StatusOK},
		{"GET", "/C:/Windows/win.ini", "", "", http.StatusNotFound},
		{"GET", "/..%5c..%5cgo.mod", "", "", http.StatusNotFound},
		{"GET", "/.env", "", "", http.StatusNotFound},
		{"GET", "/%2e%2e/%2e%2e/go.mod", "", "", http.StatusNotFound},
		{"GET", "/a/../style.css", "", "", redirect},
		{"GET", "//style.css", "", "", redirect},
		{"GET", "/a/../../go.mod", "", "", redirect},
		{"POST", "/", "application/json", "{}", http.StatusMethodNotAllowed},
		{"GET", "/api/v1/nothing", "", "", http.StatusNotFound},
		{"DELETE", "/api/v1/instances", "", "", http.StatusMethodNotAllowed},
		{"GET", "/api/v1/instances", "", "", http.StatusUnauthorized},
		{"POST", "/api/v1/login", "text/plain", "{}", http.StatusUnsupportedMediaType},
		{"POST", "/api/v1/login", "application/json", big, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		switch {
		case c.status == redirect:
			// The router's own redirect to the cleaned path (301 or 307,
			// depending on the Go version).
			if loc := rec.Header().Get("Location"); rec.Code/100 != 3 || !strings.HasPrefix(loc, "/") || strings.Contains(loc, "..") {
				t.Errorf("%s %s: %d to %q, want a redirect to a clean path", c.method, c.target, rec.Code, loc)
			}
		case rec.Code != c.status:
			t.Errorf("%s %s: %d, want %d", c.method, c.target, rec.Code, c.status)
		}
		hd := rec.Header()
		if hd.Get("Content-Security-Policy") != web.CSP || !strings.Contains(hd.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			hd.Get("X-Frame-Options") != "DENY" || hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: security headers missing: %v", c.method, c.target, hd)
		}
		if rec.Code == http.StatusOK && c.method == "GET" && strings.Contains(rec.Body.String(), "module ") {
			t.Errorf("%s %s: answered with a file outside the page", c.method, c.target)
		}
	}
}

// A request body larger than 1 MiB is refused everywhere, login and setup
// included.
func TestBodyLimit(t *testing.T) {
	h := New(Options{}).Handler()
	big := bytes.Repeat([]byte(" "), MaxBody) // white space only: valid JSON once it ends
	big = append(big, "{}"...)
	for _, path := range []string{"/api/v1/login", "/api/v1/setup", "/api/v1/logout"} {
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(big))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var eb ErrorBody
		json.Unmarshal(rec.Body.Bytes(), &eb)
		if rec.Code != http.StatusRequestEntityTooLarge || eb.Error.Code != "too_large" {
			t.Errorf("%s with %d bytes: %d %s", path, len(big), rec.Code, rec.Body)
		}
	}
}

// A request body that does not arrive in time ends the request with 408,
// so a client that sends slowly cannot hold it open.
func TestSlowBodyIsCut(t *testing.T) {
	srv := httptest.NewServer(New(Options{BodyTimeout: 300 * time.Millisecond}).Handler())
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	io.WriteString(c, "POST /api/v1/login HTTP/1.1\r\nHost: "+srv.Listener.Addr().String()+
		"\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"username\":")
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	defer resp.Body.Close()
	var eb ErrorBody
	json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusRequestTimeout || eb.Error.Code != "request_timeout" {
		t.Fatalf("stalled body: %d %+v", resp.StatusCode, eb)
	}
	if resp.Header.Get("Content-Security-Policy") != web.CSP {
		t.Fatalf("408 without security headers: %v", resp.Header)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the stalled body was cut only after %s", d)
	}
}

// stallWriter is a reply writer for an event stream that keeps write
// deadlines like a connection: a write after the deadline fails. The first
// free writes go through; once they are used up the client has stopped
// reading, and every write blocks until the deadline has passed.
type stallWriter struct {
	header http.Header
	mu     sync.Mutex
	free   int
	dl     time.Time
	writes int
}

func (w *stallWriter) Header() http.Header { return w.header }
func (w *stallWriter) WriteHeader(int)     {}
func (w *stallWriter) Flush()              {}

func (w *stallWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dl = t
	return nil
}

func (w *stallWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	if !w.dl.IsZero() && time.Now().After(w.dl) {
		w.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	if w.free > 0 {
		w.free--
		w.mu.Unlock()
		return len(b), nil
	}
	w.mu.Unlock()
	for {
		w.mu.Lock()
		dl := w.dl
		w.mu.Unlock()
		if !dl.IsZero() && time.Now().After(dl) {
			return 0, os.ErrDeadlineExceeded
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A reply whose client does not read it is cut off at the write deadline,
// which starts with the reply's first byte: the handler does not hang.
func TestUnreadReplyIsCut(t *testing.T) {
	const writeTimeout = 200 * time.Millisecond
	h := New(Options{WriteTimeout: writeTimeout}).Handler()
	for _, target := range []string{"/", "/api/v1/nothing"} {
		w := &stallWriter{header: http.Header{}} // the client reads nothing
		done := make(chan struct{})
		start := time.Now()
		go func() {
			defer close(done)
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		}()
		select {
		case <-done:
		case <-time.After(writeTimeout + 5*time.Second):
			t.Fatalf("%s: a reply nobody reads held its handler", target)
		}
		w.mu.Lock()
		dl := w.dl
		w.mu.Unlock()
		if dl.IsZero() || dl.Before(start) || dl.After(start.Add(writeTimeout+time.Second)) {
			t.Fatalf("%s: write deadline %v, want about %s after the start", target, dl, writeTimeout)
		}
	}
}

// The write deadline is per write: a stream whose client keeps reading
// runs for much longer than the write timeout.
func TestLongEventStreamKeepsGoing(t *testing.T) {
	bus := events.NewBus(16)
	s := New(Options{Bus: bus, WriteTimeout: 100 * time.Millisecond, KeepAlive: 20 * time.Millisecond})
	w := &stallWriter{header: http.Header{}, free: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.SocketHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx))
	}()
	time.Sleep(time.Second) // ten write timeouts
	select {
	case <-done:
		t.Fatal("a stream that is read was cut")
	default:
	}
	w.mu.Lock()
	before := w.writes
	w.mu.Unlock()
	bus.Publish(events.TypeState, "alpha", nil)
	time.Sleep(100 * time.Millisecond)
	w.mu.Lock()
	after := w.writes
	w.mu.Unlock()
	if before < 20 || after <= before {
		t.Fatalf("writes: %d keep-alives in a second, %d after an event", before, after)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end with its request")
	}
}

// An event stream whose client stops reading never holds up a state change
// or another subscriber: publishing goes on at full speed, the bus drops
// the stalled stream's subscription when its buffer is full, the others
// get every event, and the stalled write ends at its deadline, which ends
// the stream.
func TestStalledEventStreamIsCut(t *testing.T) {
	bus := events.NewBus(4096)
	const writeTimeout = 300 * time.Millisecond
	s := New(Options{Bus: bus, WriteTimeout: writeTimeout, KeepAlive: time.Hour})
	w := &stallWriter{header: http.Header{}, free: 1} // ": connected" gets through
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.SocketHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	}()
	// Wait until the stream has subscribed.
	deadline := time.Now().Add(5 * time.Second)
	for bus.Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stream never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	const n = 5000
	other := bus.Subscribe(events.Filter{}, n)
	start := time.Now()
	for i := range n {
		bus.Publish(events.TypeState, "alpha", map[string]int{"i": i})
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("publishing %d events took %s next to a stalled stream", n, d)
	}
	for i := range n {
		select {
		case e := <-other.C:
			if e.ID != uint64(i+1) {
				t.Fatalf("the other subscriber got event %d as number %d", e.ID, i+1)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the other subscriber got only %d events", i)
		}
	}
	select {
	case <-done:
	case <-time.After(writeTimeout + 5*time.Second):
		t.Fatal("the stalled stream did not end at its write deadline")
	}
	if got := bus.Subscribers(); got != 1 {
		t.Fatalf("%d subscribers left, want the other one only", got)
	}
	w.mu.Lock()
	writes := w.writes
	w.mu.Unlock()
	if writes != 2 {
		t.Fatalf("the stalled stream was written %d times, want 2 (connected, then one stalled event)", writes)
	}
}
