package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

// Each harness gets its own ports so parallel tests never collide.
var portBlock atomic.Int32

// lockedBuffer is a bytes.Buffer safe for concurrent use.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	t          *testing.T
	root       string
	src        string
	supervisor string
	svc        *service.Service
	stderr     *lockedBuffer
	base       string
	admin      string // admin bearer token
	http       *http.Client
	sock       *http.Client
	gameLo     int
	queryLo    int
}

type option func(cfg map[string]any)

// newESM starts a service on a fresh root with short timings.
func newESM(t *testing.T, opts ...option) *harness {
	t.Helper()
	return newESMWith(t, "process", opts...)
}

func newESMWith(t *testing.T, supervisorKind string, opts ...option) *harness {
	t.Helper()
	// A short root keeps the socket path under the platform limit.
	root, err := os.MkdirTemp("", "esm")
	if err != nil {
		t.Fatal(err)
	}
	blk := int(portBlock.Add(1))
	h := &harness{t: t, root: root, src: t.TempDir(), supervisor: supervisorKind, http: &http.Client{},
		gameLo: 30000 + blk*20, queryLo: 31000 + blk*20}
	cfg := map[string]any{
		"listen":           "127.0.0.1:0",
		"game_ports":       fmt.Sprintf("%d-%d", h.gameLo, h.gameLo+9),
		"query_ports":      fmt.Sprintf("%d-%d", h.queryLo, h.queryLo+9),
		"supervisor":       supervisorKind,
		"backoff":          map[string]any{"base": "50ms", "max": "200ms", "window": "10m", "max_crashes": 5},
		"stop_timeout":     "5s",
		"ready_timeout":    "10s",
		"backup_save_wait": "3s",
		"log_level":        "debug",
		"build_import_dir": h.src,
	}
	for _, o := range opts {
		o(cfg)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(root+string(os.PathSeparator)+"esm.json", b, 0o600); err != nil {
		t.Fatal(err)
	}
	h.sock = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", root+string(os.PathSeparator)+"esm.sock")
	}}}
	h.startService()
	var tok api.NewToken
	if st := h.callSock("POST", "/api/v1/tokens", api.TokenRequest{Name: "tests", Role: "admin"}, &tok); st != http.StatusCreated {
		t.Fatalf("admin token over the socket: %d", st)
	}
	h.admin = tok.Token
	t.Cleanup(h.cleanup)
	return h
}

func (h *harness) startService() {
	h.t.Helper()
	h.stderr = &lockedBuffer{}
	svc, err := service.Start(service.Options{
		Root: h.root, Version: "test", Stderr: h.stderr,
		WatchInterval: 50 * time.Millisecond, SchedulerInterval: time.Hour, KeepAlive: 200 * time.Millisecond,
		PortHost: "127.0.0.1", AuthOptions: auth.Options{Iterations: 1000}, FailDelay: 10 * time.Millisecond,
	})
	if err != nil {
		h.t.Fatalf("start service: %v", err)
	}
	h.svc = svc
	h.base = "http://" + svc.Addr()
}

func (h *harness) stopService() {
	h.t.Helper()
	if h.svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.svc.Shutdown(ctx); err != nil {
		h.t.Errorf("shutdown: %v", err)
	}
	h.svc = nil
}

func (h *harness) cleanup() {
	if h.svc != nil {
		// This test process and every server still running listen on
		// loopback only.
		testkit.CheckLoopback(h.t, os.Getpid())
		var list api.InstanceList
		if h.call("GET", "/api/v1/instances", h.admin, nil, &list) == http.StatusOK {
			for _, v := range list.Instances {
				testkit.CheckLoopback(h.t, v.Status.PID)
				if h.call("POST", "/api/v1/instances/"+v.Name+"/stop", h.admin, struct{}{}, nil) != http.StatusOK {
					// ESM leaves a server that does not stop in time
					// running; the test does not.
					h.kill(v.Name)
				}
				if h.supervisor == "systemd" {
					exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", "esm-"+v.Name+".service").Run()
					exec.Command("systemctl", "--user", "stop", "esm-"+v.Name+".service").Run()
				}
			}
		}
		h.stopService()
	}
	os.RemoveAll(h.root)
}

// kill ends the server of an instance with a kill, as a test may and ESM
// never does on Linux.
func (h *harness) kill(name string) {
	var st instances.Status
	if h.call("GET", "/api/v1/instances/"+name+"/status", h.admin, nil, &st) != http.StatusOK || st.PID <= 0 {
		return
	}
	if h.supervisor == "systemd" {
		exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", "esm-"+name+".service").Run()
		return
	}
	if p, err := os.FindProcess(st.PID); err == nil {
		p.Kill()
	}
}

func (h *harness) send(c *http.Client, req *http.Request, out any) int {
	h.t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("%s %s: decode %q: %v", req.Method, req.URL.Path, b, err)
		}
	}
	if resp.StatusCode >= 400 && !json.Valid(b) {
		h.t.Fatalf("%s %s: error answer is not JSON: %q", req.Method, req.URL.Path, b)
	}
	return resp.StatusCode
}

func newRequest(t *testing.T, method, url string, body any) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// call sends a request over TCP with a bearer token (none when empty) and
// decodes the answer (success or error) into out.
func (h *harness) call(method, path, token string, body, out any) int {
	h.t.Helper()
	req := newRequest(h.t, method, h.base+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return h.send(h.http, req, out)
}

// callSock sends a request over the unix socket.
func (h *harness) callSock(method, path string, body, out any) int {
	h.t.Helper()
	return h.send(h.sock, newRequest(h.t, method, "http://esm"+path, body), out)
}

// must calls and fails the test on an unexpected status.
func (h *harness) must(want int, method, path string, body, out any) {
	h.t.Helper()
	var raw json.RawMessage
	if st := h.call(method, path, h.admin, body, &raw); st != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, st, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
}

// install makes a build folder and installs it through the API.
func (h *harness) install(o testkit.BuildOptions) api.BuildView {
	h.t.Helper()
	dir := testkit.MakeBuild(h.t, h.src, o)
	var b api.BuildView
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, &b)
	return b
}

func fastRules(extra map[string]string) map[string]string {
	m := map[string]string{"FakeReadyDelay": "100ms", "FakeSaveInterval": "200ms"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// create makes an instance bound to 127.0.0.1 with a quick fake.
func (h *harness) create(name, build string, rules map[string]string) api.InstanceChange {
	h.t.Helper()
	body := map[string]any{"name": name, "bind_ip": "127.0.0.1", "rules": fastRules(rules)}
	if build != "" {
		body["build"] = build
	}
	var v api.InstanceChange
	h.must(http.StatusCreated, "POST", "/api/v1/instances", body, &v)
	return v
}

func (h *harness) status(name string) instances.Status {
	h.t.Helper()
	var st instances.Status
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/status", nil, &st)
	return st
}

func (h *harness) waitState(name string, want instances.State, timeout time.Duration) instances.Status {
	h.t.Helper()
	var st instances.Status
	testkit.Eventually(h.t, timeout, func() bool {
		st = h.status(name)
		return st.State == want
	}, "instance %s never reached %s (last: %+v)", name, want, &st)
	if want == instances.Running {
		testkit.CheckLoopback(h.t, st.PID)
	}
	return st
}

// dataDir is the absolute data folder of an instance.
func (h *harness) dataDir(name string) string {
	return strings.Join([]string{h.root, "instances", name, "data"}, string(os.PathSeparator))
}

// instDir is the absolute folder of an instance.
func (h *harness) instDir(name string) string {
	return strings.Join([]string{h.root, "instances", name}, string(os.PathSeparator))
}

// serverLog is the server log of an instance.
func (h *harness) serverLog(name string) string { return gameserver.ServerLogPath(h.instDir(name)) }

// saveCounter reads the fake's save counter from a save file's content (-1
// when unreadable).
func saveCounter(b []byte) int {
	var f struct {
		Counter *int `json:"counter"`
	}
	if json.Unmarshal(b, &f) != nil || f.Counter == nil {
		return -1
	}
	return *f.Counter
}

// fakeSave is the slash path of the fake's save of a world, inside a data
// folder or a backup.
func fakeSave(world string) string {
	return "Saved/SaveGames/Fake/" + testkit.DefaultMap + "__" + world + ".json"
}

func (h *harness) liveCounter(name, world string) int {
	b, err := os.ReadFile(filepath.Join(h.dataDir(name), filepath.FromSlash(fakeSave(world))))
	if err != nil {
		return -1
	}
	return saveCounter(b)
}

// sse opens an event stream and delivers parsed events.
type sse struct {
	events chan events.Event
	names  chan string
	resp   *http.Response
	cancel context.CancelFunc
}

func (h *harness) openEvents(query, token, lastID string) *sse {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", h.base+"/api/v1/events"+query, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		h.t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	s := &sse{events: make(chan events.Event, 256), names: make(chan string, 256), resp: resp, cancel: cancel}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var id, name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var e events.Event
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) == nil && strconv.FormatUint(e.ID, 10) == id {
					s.names <- name
					s.events <- e
				}
			}
		}
	}()
	h.t.Cleanup(s.close)
	return s
}

func (s *sse) close() {
	s.cancel()
	s.resp.Body.Close()
}

// next waits for the next event matching ok.
func (s *sse) next(t *testing.T, timeout time.Duration, ok func(events.Event) bool) events.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e, open := <-s.events:
			if !open {
				t.Fatal("event stream closed")
			}
			<-s.names
			if ok(e) {
				return e
			}
		case <-deadline:
			t.Fatal("no matching event in time")
		}
	}
}

func stateOf(e events.Event) instances.Status {
	var st instances.Status
	json.Unmarshal(e.Data, &st)
	return st
}
