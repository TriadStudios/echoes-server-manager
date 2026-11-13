package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/cli"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// runCLI runs the esm command line against the harness root (so it talks
// over the unix socket) and returns its exit code, stdout and stderr.
func (h *harness) runCLI(args ...string) (int, string, string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{"--root", h.root}, args...), &out, &errOut, "test")
	return code, out.String(), errOut.String()
}

func oneLine(t *testing.T, what, s string) string {
	t.Helper()
	if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
		t.Fatalf("%s printed %d lines, want exactly one: %q", what, strings.Count(s, "\n"), s)
	}
	return strings.TrimSuffix(s, "\n")
}

// 12: the command line over the socket prints one line per verb.
func TestCLIOverSocket(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	b := testkit.MakeBuild(t, h.src, testkit.BuildOptions{Version: "1.0"})

	code, out, errOut := h.runCLI("build", "install", b)
	if code != 0 || !strings.Contains(oneLine(t, "build install", out), "installed build 1.0") {
		t.Fatalf("build install: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("create", "alpha", "--bind", "127.0.0.1", "--set", "FakeReadyDelay=100ms",
		"--set", "FakeSaveInterval=200ms", "--arg", "-FakeExitCode=7", "--max-players", "16")
	if code != 0 || !strings.HasPrefix(oneLine(t, "create", out), "created alpha: build 1.0 (live)") {
		t.Fatalf("create: %d %q %q", code, out, errOut)
	}

	code, out, errOut = h.runCLI("list", "--json")
	if code != 0 {
		t.Fatalf("list --json: %d %q", code, errOut)
	}
	var list api.InstanceList
	if err := json.Unmarshal([]byte(oneLine(t, "list --json", out)), &list); err != nil || len(list.Instances) != 1 ||
		list.Instances[0].MaxPlayers != 16 || list.Instances[0].ExtraArgs[0] != "-FakeExitCode=7" {
		t.Fatalf("list --json: %v %q", err, out)
	}

	code, out, errOut = h.runCLI("start", "alpha")
	if code != 0 || !strings.HasPrefix(oneLine(t, "start", out), "alpha running") {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("status", "alpha")
	if code != 0 || !strings.Contains(oneLine(t, "status", out), "ready via") {
		t.Fatalf("status: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("backup", "alpha")
	if code != 0 || !strings.HasPrefix(oneLine(t, "backup", out), "backup ") {
		t.Fatalf("backup: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("logs", "alpha", "-n", "3")
	if code != 0 || strings.Count(out, "\n") != 3 {
		t.Fatalf("logs -n 3: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("stop", "alpha")
	if code != 0 || !strings.HasPrefix(oneLine(t, "stop", out), "alpha stopped") {
		t.Fatalf("stop: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("status")
	if code != 0 || !strings.Contains(oneLine(t, "status", out), "process supervisor (limits not enforced)") {
		t.Fatalf("box status: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("build", "list")
	if code != 0 || oneLine(t, "build list", out) != "1 build: 1.0 (live, used by alpha)" {
		t.Fatalf("build list: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("token", "create", "--role", "read", "--name", "page")
	if code != 0 || !strings.HasPrefix(oneLine(t, "token create", out), "esm_") {
		t.Fatalf("token create: %d %q %q", code, out, errOut)
	}
	tok := strings.Fields(out)[0]
	// The same API over TCP with --url and the new token.
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "list")
	if code != 0 || !strings.HasPrefix(oneLine(t, "list --url", out), "1 instance: alpha stopped") {
		t.Fatalf("list over --url: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "start", "alpha")
	if code != 1 || out != "" || !strings.Contains(errOut, "operator") {
		t.Fatalf("start with a read token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("start", "ghost")
	if code != 1 || out != "" || oneLine(t, "error", errOut) != `esm: instance "ghost" not found` {
		t.Fatalf("start of a missing instance: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--json", "start", "ghost")
	var eb api.ErrorBody
	if code != 1 || json.Unmarshal([]byte(oneLine(t, "--json error", out)), &eb) != nil || eb.Error.Code != "not_found" {
		t.Fatalf("--json error: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("version")
	if code != 0 || oneLine(t, "version", out) != "esm test" {
		t.Fatalf("version: %d %q", code, out)
	}
	// --bind keeps the port checks on loopback, as every test must.
	code, out, errOut = h.runCLI("doctor", "--bind", "127.0.0.1")
	if !strings.HasPrefix(oneLine(t, "doctor", out), "doctor: ") || !strings.Contains(out, "service checked too") {
		t.Fatalf("doctor: %d %q", code, out)
	}
	code, _, errOut = h.runCLI("doctor", "--bind", "nowhere")
	if code != 1 || !strings.Contains(errOut, `--bind "nowhere" is not an IP address`) {
		t.Fatalf("doctor with a bad --bind: %d %q", code, errOut)
	}
	code, _, errOut = h.runCLI("frobnicate")
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("unknown verb: %d %q", code, errOut)
	}
	if st := h.call("GET", "/api/v1/instances/alpha", tok, nil, nil); st != http.StatusOK {
		t.Fatalf("read token over TCP: %d", st)
	}
}

// The command line says UNSAFE wherever it shows an instance that passes
// its passwords on the server command line: create, list, status, the box
// line and doctor. A read token gets no log of such an instance.
func TestCLIShowsUnsafeDelivery(t *testing.T) {
	t.Parallel()
	h := newESM(t)
	h.install(testkit.BuildOptions{Version: "1.0"})
	const long = "UNSAFE on a shared box: passwords and tokens are on the server command line"
	const short = "UNSAFE: passwords on the command line"

	code, out, errOut := h.runCLI("create", "safe", "--bind", "127.0.0.1", "--set", "FakeReadyDelay=100ms")
	if code != 0 || strings.Contains(out, "UNSAFE") || strings.Contains(out, "warning") {
		t.Fatalf("create with the default delivery: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("create", "risky", "--bind", "127.0.0.1", "--set", "FakeReadyDelay=100ms", "--secret-delivery", "command_line")
	if code != 0 || !strings.Contains(oneLine(t, "create", out), "; warning: "+long) {
		t.Fatalf("create with command-line delivery: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("create", "odd", "--bind", "127.0.0.1", "--secret-delivery", "environment")
	if code != 1 || out != "" || !strings.Contains(errOut, "secret_delivery") {
		t.Fatalf("create with an unknown delivery: %d %q %q", code, out, errOut)
	}

	code, out, errOut = h.runCLI("list")
	line := oneLine(t, "list", out)
	if code != 0 || !strings.Contains(line, "risky stopped (") || strings.Count(line, short) != 1 ||
		!strings.Contains(line[strings.Index(line, "risky"):strings.Index(line, "safe")], short) {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("--json", "list")
	var list api.InstanceList
	if err := json.Unmarshal([]byte(oneLine(t, "list --json", out)), &list); code != 0 || err != nil || len(list.Instances) != 2 ||
		len(list.Instances[0].Warnings) != 1 || list.Instances[0].Warnings[0] != "secrets_on_command_line" || len(list.Instances[1].Warnings) != 0 ||
		list.Instances[0].SecretDelivery != "command_line" || list.Instances[1].SecretDelivery != "file" {
		t.Fatalf("list --json: %d %v %q", code, err, out)
	}
	code, out, errOut = h.runCLI("status", "risky")
	if code != 0 || !strings.HasPrefix(oneLine(t, "status", out), "risky stopped") || !strings.Contains(out, "; warning: "+long) {
		t.Fatalf("status of the unsafe instance: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("status", "safe")
	if code != 0 || !strings.HasPrefix(oneLine(t, "status", out), "safe stopped") || strings.Contains(out, "UNSAFE") {
		t.Fatalf("status of the other instance: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("--json", "status", "risky")
	var view struct {
		Name     string   `json:"name"`
		Warnings []string `json:"warnings"`
		Status   struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(oneLine(t, "status --json", out)), &view); code != 0 || err != nil || view.Name != "risky" ||
		view.Status.State != "stopped" || len(view.Warnings) != 1 {
		t.Fatalf("status --json: %d %v %q", code, err, out)
	}
	code, out, errOut = h.runCLI("status")
	if code != 0 || !strings.Contains(oneLine(t, "box status", out), "; "+short+" (risky)") {
		t.Fatalf("box status: %d %q %q", code, out, errOut)
	}
	_, out, _ = h.runCLI("doctor", "--bind", "127.0.0.1")
	if line := oneLine(t, "doctor", out); !strings.Contains(line, "; secret_delivery: UNSAFE on a shared box: 1 instance (risky)") ||
		!strings.Contains(line, "; service secret_delivery: UNSAFE on a shared box: 1 instance (risky)") {
		t.Fatalf("doctor: %q", out)
	}

	// Started, the unsafe instance shows the warning with its state, and a
	// read token is refused its log.
	code, out, errOut = h.runCLI("start", "risky")
	if code != 0 || !strings.HasPrefix(oneLine(t, "start", out), "risky running") {
		t.Fatalf("start: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("logs", "risky", "-n", "3")
	if code != 0 || strings.Count(out, "\n") != 3 {
		t.Fatalf("logs over the socket: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("token", "create", "--role", "read")
	if code != 0 {
		t.Fatalf("token create: %d %q %q", code, out, errOut)
	}
	tok := strings.Fields(out)[0]
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "logs", "risky")
	if code != 1 || out != "" || !strings.Contains(errOut, "hidden from the read role") {
		t.Fatalf("logs with a read token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "--json", "logs", "risky", "--console")
	var eb api.ErrorBody
	if code != 1 || json.Unmarshal([]byte(oneLine(t, "--json error", out)), &eb) != nil || eb.Error.Code != "hidden_unsafe_secrets" {
		t.Fatalf("logs --json with a read token: %d %q %q", code, out, errOut)
	}
	code, out, errOut = h.runCLI("--url", h.base, "--token", tok, "logs", "safe")
	if code != 0 {
		t.Fatalf("logs of the other instance with a read token: %d %q", code, out)
	}
	code, out, errOut = h.runCLI("stop", "risky")
	if code != 0 || !strings.HasPrefix(oneLine(t, "stop", out), "risky stopped") {
		t.Fatalf("stop: %d %q %q", code, out, errOut)
	}
}
