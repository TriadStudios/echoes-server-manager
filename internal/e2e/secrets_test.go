package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

const (
	leakServerPW = "open sesame 42"
	leakAdminPW  = "adm1n-secret"
)

// brokenBuild makes a build whose executables are text files, so starting
// it fails inside the supervisor.
func brokenBuild(t *testing.T, parent, version string) string {
	t.Helper()
	dir := filepath.Join(parent, version)
	files := map[string]string{"bin/server": "not a program\n", "bin/server.exe": "not a program\n"}
	sums := map[string]string{}
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		sums[rel] = hex.EncodeToString(sum[:])
	}
	m := map[string]any{"version": version, "commit": "broken", "channel": "live",
		"executables": map[string]string{"linux": "bin/server", "windows": "bin/server.exe"}, "files": sums}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeSystemdRun puts a systemd-run on PATH that fails and repeats its
// whole command line, the worst case for leaking passwords.
func fakeSystemdRun(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"Failed to start transient service unit: $*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A start that fails inside the supervisor never shows the passwords: not
// in the answer, the status, the events or the audit log.
func TestStartFailureHidesPasswords(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	dir := brokenBuild(t, h.src, "0.9-broken")
	h.must(http.StatusCreated, "POST", "/api/v1/builds", api.InstallRequest{Source: dir}, nil)
	startFailureHidesPasswords(t, h, "alpha", "0.9-broken")
}

func TestStartFailureHidesPasswordsSystemd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd is Linux only")
	}
	h := newSystemdESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	fakeSystemdRun(t)
	startFailureHidesPasswords(t, h, unitSafeName("sdpw"), "1.0")
}

func startFailureHidesPasswords(t *testing.T, h *harness, name, build string) {
	h.create(name, build, nil)
	h.must(http.StatusOK, "PUT", "/api/v1/instances/"+name+"/secrets",
		map[string]string{"server_password": leakServerPW, "admin_password": leakAdminPW}, nil)
	s := h.openEvents("", h.admin, "")
	var raw json.RawMessage
	st := h.call("POST", "/api/v1/instances/"+name+"/start?wait=true", h.admin, struct{}{}, &raw)
	var eb api.ErrorBody
	json.Unmarshal(raw, &eb)
	if st != http.StatusUnprocessableEntity || eb.Error.Code != "start_failed" {
		t.Fatalf("start of a build that cannot run: %d %s", st, raw)
	}
	var status json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/instances/"+name+"/status", nil, &status)
	var seen []events.Event
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case e := <-s.events:
			<-s.names
			seen = append(seen, e)
		case <-deadline:
			break collect
		}
	}
	evRaw, _ := json.Marshal(seen)
	auditLog, err := os.ReadFile(filepath.Join(h.root, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	for what, text := range map[string]string{
		"error answer": string(raw), "status": string(status), "events": string(evRaw),
		"audit log": string(auditLog), "service log": h.stderr.String(),
	} {
		for _, secret := range []string{leakServerPW, leakAdminPW, "sesame"} {
			if strings.Contains(text, secret) {
				t.Errorf("the %s shows a password:\n%s", what, text)
			}
		}
	}
	if !strings.Contains(string(auditLog), `"action":"instance.start"`) {
		t.Fatalf("the failed start was not audited:\n%s", auditLog)
	}
}
