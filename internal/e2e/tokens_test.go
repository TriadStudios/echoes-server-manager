package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

const (
	announceToken = "e2e-ann0unce-7Qx"
	reportToken   = "e2e-rep0rt-9Zw"
)

// The announcements and problem report tokens reach a server the way its
// passwords do. With file delivery they are in the secrets file and on no
// command line, and nothing ESM shows or writes holds them. With
// command-line delivery the server's command line holds them, and what ESM
// shows of it has them masked.
func TestTokensReachTheServerLikeThePasswords(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	pieces := passwordPieces(announceToken, reportToken)
	setTokens := func(name string) api.SecretsResult {
		var sr api.SecretsResult
		h.must(http.StatusOK, "PUT", "/api/v1/instances/"+name+"/secrets",
			map[string]string{"announcements_token": announceToken, "problem_report_token": reportToken}, &sr)
		return sr
	}

	// File delivery.
	h.create("alpha", "", nil)
	if sr := setTokens("alpha"); !sr.Secrets.AnnouncementsTokenSet || !sr.Secrets.ProblemReportTokenSet || sr.Secrets.ServerPasswordSet {
		t.Fatalf("set the tokens: %+v", sr)
	}
	ev := h.recordEvents()
	var st instances.Status
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/start?wait=true", struct{}{}, &st)
	ev.waitCommandLines(t, 1)
	if got, err := h.runSecrets("alpha"); err != nil || got.AnnouncementsToken != announceToken || got.ProblemReportToken != reportToken {
		t.Fatalf("the secrets file while the server runs: %v", err)
	}
	if line := procCommandLine(st.PID); runtime.GOOS == "linux" {
		noPieces(t, "command line of the running server", []byte(line), pieces)
	}
	h.checkNothingToHide(t, "alpha", ev, pieces)
	if strings.Contains(strings.ToLower(h.rawOutput("alpha")), "token=") {
		t.Error("a token switch is in what the file-delivery server wrote")
	}
	h.must(http.StatusOK, "POST", "/api/v1/instances/alpha/stop", struct{}{}, nil)

	// Command-line delivery.
	h.must(http.StatusCreated, "POST", "/api/v1/instances", map[string]any{
		"name": "beta", "bind_ip": "127.0.0.1", "rules": fastRules(nil), "secret_delivery": "command_line"}, nil)
	setTokens("beta")
	h.must(http.StatusOK, "POST", "/api/v1/instances/beta/start?wait=true", struct{}{}, &st)
	raw := h.rawOutput("beta")
	if !strings.Contains(raw, "-AnnouncementsToken="+announceToken+" -ProblemReportToken="+reportToken) {
		t.Fatalf("the server's command line does not hold the tokens:\n%s", raw)
	}
	var logs api.Logs
	if code := h.call("GET", "/api/v1/instances/beta/logs?lines=2000&source=console", h.admin, nil, &logs); code != http.StatusOK {
		t.Fatalf("console log for the admin: %d", code)
	}
	text := strings.Join(logs.Lines, "\n")
	if !strings.Contains(text, "-AnnouncementsToken="+gameserver.Mask+" -ProblemReportToken="+gameserver.Mask) {
		t.Fatalf("the console log for the admin has no command line with masked tokens:\n%s", text)
	}
	noPieces(t, "console log for the admin", []byte(text), pieces)
	for _, path := range []string{"/api/v1/instances/beta", "/api/v1/instances", "/api/v1/audit?limit=200"} {
		var body json.RawMessage
		if code := h.call("GET", path, h.admin, nil, &body); code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, code)
		}
		noPieces(t, "answer of GET "+path, body, pieces)
	}
	noPieces(t, "service log", []byte(h.stderr.String()), pieces)
	auditLog, _ := os.ReadFile(filepath.Join(h.root, "audit.log"))
	noPieces(t, "audit log", auditLog, pieces)
	inst, _ := os.ReadFile(filepath.Join(h.instDir("beta"), "instance.json"))
	noPieces(t, "instance.json", inst, pieces)
	h.must(http.StatusOK, "POST", "/api/v1/instances/beta/stop", struct{}{}, nil)
}
