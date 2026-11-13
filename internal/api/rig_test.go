package api

import (
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
)

// rig is an API server with real logins, an audit log and an event bus,
// but no instances or builds: enough for the login, token and event
// routes.
type rig struct {
	t     *testing.T
	s     *Server
	st    *store.Store
	auth  *auth.Manager
	audit *audit.Log
	bus   *events.Bus
}

func newRig(t *testing.T, o Options) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	am, err := auth.New(st, auth.Options{Iterations: 1000})
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(256)
	al.SetNotify(func(e audit.Entry) { bus.Publish(events.TypeAudit, "", e) })
	t.Cleanup(func() { bus.Close(); al.Close(); st.Close() })
	if o.Config == nil {
		o.Config = config.Defaults(st.Path())
	}
	o.Auth, o.Audit, o.Bus = am, al, bus
	return &rig{t: t, s: New(o), st: st, auth: am, audit: al, bus: bus}
}

// do sends a request to the TCP handler. The request comes from
// 192.0.2.1:1234 unless remote says otherwise.
func (r *rig) do(method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	r.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	for k, v := range header {
		if k == "remote" {
			req.RemoteAddr = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rec, req)
	return rec
}

var jsonHeader = map[string]string{"Content-Type": "application/json"}

func (r *rig) auditLog() string {
	r.t.Helper()
	b, err := r.st.ReadFile(audit.FileName)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func header(kv ...string) map[string]string {
	h := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		h[kv[i]] = kv[i+1]
	}
	return h
}

func (r *rig) status(rec *httptest.ResponseRecorder, want int) {
	r.t.Helper()
	if rec.Code != want {
		r.t.Fatalf("status %d (%s), want %d", rec.Code, rec.Body, want)
	}
}
