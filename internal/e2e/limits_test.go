package e2e

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The HTTP limits: request headers are capped, log tails are capped in
// lines, and an event client that never reads holds up neither state
// changes nor the other clients.
func TestHTTPLimits(t *testing.T) {
	t.Parallel()
	h := newESM(t)

	// Request headers beyond the cap are refused by the server.
	req := newRequest(t, "GET", h.base+"/api/v1/instances", nil)
	req.Header.Set("Authorization", "Bearer "+h.admin)
	req.Header.Set("X-Filler", strings.Repeat("a", api.MaxHeaderBytes+8<<10))
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversized headers: %d", resp.StatusCode)
	}
	req.Header.Set("X-Filler", strings.Repeat("a", 32<<10))
	if st := h.send(h.http, req, nil); st != http.StatusOK {
		t.Fatalf("headers within the cap: %d", st)
	}

	h.install(testkit.BuildOptions{Version: "1.0"})
	h.create("alpha", "", nil)
	var eb api.ErrorBody
	if st := h.call("GET", fmt.Sprintf("/api/v1/instances/alpha/logs?lines=%d", instances.MaxTailLines+1), h.admin, nil, &eb); st != http.StatusUnprocessableEntity {
		t.Fatalf("lines beyond the cap: %d %+v", st, eb)
	}
	if st := h.call("GET", fmt.Sprintf("/api/v1/instances/alpha/logs?lines=%d", instances.MaxTailLines), h.admin, nil, nil); st != http.StatusOK {
		t.Fatalf("lines at the cap: %d", st)
	}

	// An event client that subscribes and then never reads.
	stalled, err := net.Dial("tcp", strings.TrimPrefix(h.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	if tc, ok := stalled.(*net.TCPConn); ok {
		tc.SetReadBuffer(4 << 10)
	}
	fmt.Fprintf(stalled, "GET /api/v1/events HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n", strings.TrimPrefix(h.base, "http://"), h.admin)
	reader := h.openEvents("?instance=alpha&types=state", h.admin, "")
	for range 3 {
		start := time.Now()
		h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, nil)
		reader.next(t, 10*time.Second, func(e events.Event) bool { return stateOf(e).State == instances.Running })
		h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)
		reader.next(t, 10*time.Second, func(e events.Event) bool { return stateOf(e).State == instances.Stopped })
		if d := time.Since(start); d > 20*time.Second {
			t.Fatalf("a start and stop took %s next to a stalled event client", d)
		}
	}
}
