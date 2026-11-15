package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/cli"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
	"github.com/TriadStudios/echoes-server-manager/internal/tlscert"
)

// remote is a client of the API listener, as a control panel on another
// machine is: over TLS, trusting the listener by its public key pin, with
// an API token.
type remote struct {
	t    *testing.T
	base string
	http *http.Client
}

func (h *harness) remote() *remote {
	h.t.Helper()
	info := h.svc.APIListener()
	if info == nil {
		h.t.Fatal("no API listener")
	}
	pin := info.Certificate.SPKIPin
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if tlscert.SPKIPin(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
			return errors.New("the public key pin does not match")
		}
		return nil
	}}}
	h.t.Cleanup(tr.CloseIdleConnections)
	return &remote{t: h.t, base: "https://" + info.Listen, http: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// call sends a request with a bearer token and decodes the answer into
// out; raw is the answer as it came.
func (rc *remote) call(method, path, token string, body, out any) (status int, raw string) {
	rc.t.Helper()
	req := newRequest(rc.t, method, rc.base+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := rc.http.Do(req)
	if err != nil {
		rc.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			rc.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode, string(b)
}

// A control panel reaches ESM through the API listener of a whole service:
// over TLS, pinned, with tokens and their roles. One call of the instance
// list gives it what it shows of each server: state, build, ports, display
// name and the player count (null, with a reason, while the server does not
// report it). The box, the command line (esm tls) and the start log show
// the listener's pins. Shutdown ends its event streams and closes it, as
// fast as without it.
func TestAPIListenerThroughAWholeService(t *testing.T) {
	t.Parallel()
	h := newESM(t, func(cfg map[string]any) {
		cfg["api_listener"] = map[string]any{"listen": "127.0.0.1:0", "host_names": []string{"esm.example.com"}}
	})
	info := h.svc.APIListener()
	rc := h.remote()
	var read, oper api.NewToken
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "panel", Role: "read"}, &read)
	h.must(http.StatusCreated, "POST", "/api/v1/tokens", api.TokenRequest{Name: "bot", Role: "operator"}, &oper)

	h.install(testkit.BuildOptions{Version: "1.0"})
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{"name": "alpha", "bind_ip": "127.0.0.1",
		"display_name": "Alpha Test", "rules": fastRules(nil)}, nil)

	// Stopped: everything a panel shows, the player count null with why.
	var list api.InstanceList
	st, raw := rc.call("GET", "/api/v1/instances", read.Token, nil, &list)
	if st != http.StatusOK || len(list.Instances) != 1 {
		t.Fatalf("list: %d %s", st, raw)
	}
	v := list.Instances[0]
	if v.Status.State != instances.Stopped || v.Build != "1.0" || v.DisplayName != "Alpha Test" || v.Ports.Game == 0 || v.Ports.Query == 0 ||
		v.Live.Players != nil || !strings.Contains(raw, `"players":null`) {
		t.Fatalf("stopped instance: %+v\n%s", v, raw)
	}
	why := ""
	for _, u := range v.Live.Unknown {
		if u.Field == "players" {
			why = u.Reason
		}
	}
	if why == "" {
		t.Fatalf("no reason for the unknown player count: %+v", v.Live.Unknown)
	}

	// Roles: read may not start; operator may.
	if st, raw := rc.call("POST", "/api/v1/instances/alpha/start", read.Token, struct{}{}, nil); st != http.StatusForbidden ||
		!strings.Contains(raw, `"forbidden"`) {
		t.Fatalf("read token started: %d %s", st, raw)
	}
	var started instances.Status
	if st, raw := rc.call("POST", "/api/v1/instances/alpha/start?wait=true", oper.Token, struct{}{}, &started); st != http.StatusOK ||
		started.State != instances.Running {
		t.Fatalf("operator start: %d %s", st, raw)
	}
	testkit.CheckLoopback(t, started.PID)
	if err := testkit.SetPlayers(h.dataDir("alpha"), 3); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		rc.call("GET", "/api/v1/instances", read.Token, nil, &list)
		return len(list.Instances) == 1 && list.Instances[0].Live.Players != nil && *list.Instances[0].Live.Players == 3
	}, "the list never showed 3 players: %+v", &list)
	var one instances.View
	if st, _ := rc.call("GET", "/api/v1/instances/alpha", read.Token, nil, &one); st != http.StatusOK || one.Live.Players == nil ||
		*one.Live.Players != 3 || one.Status.State != instances.Running {
		t.Fatalf("one instance: %d %+v", st, one.Live)
	}

	// No session cookie, no page, no login, no socket-only call there.
	req := newRequest(t, "GET", rc.base+"/api/v1/instances", nil)
	req.AddCookie(&http.Cookie{Name: "esm_session", Value: "any"})
	resp, err := rc.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), api.TokensOnly) {
		t.Fatalf("cookie: %d %s", resp.StatusCode, b)
	}
	if st, raw := rc.call("GET", "/", "", nil, nil); st != http.StatusNotFound || !strings.Contains(raw, "serves no web page") {
		t.Fatalf("page: %d %s", st, raw)
	}
	if st, raw := rc.call("POST", "/api/v1/login", "", api.LoginRequest{Username: "admin", Password: "a long password 1"}, nil); st != http.StatusForbidden ||
		!strings.Contains(raw, api.TokensOnly) {
		t.Fatalf("login: %d %s", st, raw)
	}
	if st, raw := rc.call("PUT", "/api/v1/users/admin/password", h.admin, api.PasswordResetRequest{Password: "another long password"}, nil); st != http.StatusForbidden ||
		!strings.Contains(raw, "socket_only") {
		t.Fatalf("socket-only call: %d %s", st, raw)
	}

	// The pins, in the box, on the command line and in the start log.
	var box api.Box
	if st, raw := rc.call("GET", "/api/v1/box", read.Token, nil, &box); st != http.StatusOK || box.APIListener == nil ||
		box.APIListener.Listen != info.Listen || box.APIListener.Certificate.SPKIPin != info.Certificate.SPKIPin ||
		box.APIListener.Certificate.SHA256 != info.Certificate.SHA256 || box.APIListener.Page {
		t.Fatalf("box: %d %s", st, raw)
	}
	code, out, errOut := h.runCLI("tls")
	line := oneLine(t, "tls", out)
	if code != 0 || !strings.Contains(line, info.Listen) || !strings.Contains(line, info.Certificate.SHA256) ||
		!strings.Contains(line, info.Certificate.SPKIPin) || !strings.Contains(line, "API tokens only") {
		t.Fatalf("esm tls: %d %q %q", code, out, errOut)
	}
	code, out, _ = h.runCLI("--json", "tls")
	var tr cli.TLSResult
	if err := json.Unmarshal([]byte(oneLine(t, "tls --json", out)), &tr); code != 0 || err != nil || tr.APIListener == nil ||
		tr.APIListener.Certificate.SPKIPin != info.Certificate.SPKIPin {
		t.Fatalf("esm --json tls: %d %v %q", code, err, out)
	}
	if code, out, _ := h.runCLI("status"); code != 0 || !strings.Contains(out, "API listener on "+info.Listen) {
		t.Fatalf("esm status: %d %q", code, out)
	}
	for _, want := range []string{"API listener started", info.Certificate.SPKIPin, info.Certificate.SHA256} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("the start log does not say %q", want)
		}
	}

	// Everything this process listens on, the API listener included, is
	// loopback.
	testkit.CheckLoopback(t, os.Getpid())

	// The operator stops the server; then the service ends with an event
	// stream open on the API listener.
	if st, raw := rc.call("POST", "/api/v1/instances/alpha/stop", oper.Token, struct{}{}, nil); st != http.StatusOK {
		t.Fatalf("operator stop: %d %s", st, raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", rc.base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+read.Token)
	resp, err = rc.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %v %v", resp, err)
	}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
		}
		resp.Body.Close()
	}()
	start := time.Now()
	h.stopService()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("shutdown with the API listener took %s", d)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream on the API listener did not end with the service")
	}
	if c, err := net.DialTimeout("tcp", info.Listen, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("the API listener still takes connections after shutdown")
	}
}

// Without api_listener there is none: the box says null, esm tls says so,
// and nothing more listens than before.
func TestNoAPIListenerByDefault(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	if h.svc.APIListener() != nil {
		t.Fatalf("an API listener without api_listener: %+v", h.svc.APIListener())
	}
	var raw json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/box", nil, &raw)
	if !strings.Contains(string(raw), `"api_listener":null`) {
		t.Fatalf("box: %s", raw)
	}
	code, out, _ := h.runCLI("tls")
	if code != 0 || !strings.Contains(oneLine(t, "tls", out), "no API listener") {
		t.Fatalf("esm tls: %d %q", code, out)
	}
	if code, out, _ := h.runCLI("status"); code != 0 || strings.Contains(out, "API listener") {
		t.Fatalf("esm status: %d %q", code, out)
	}
}
