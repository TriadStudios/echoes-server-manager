package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/auth"
	"github.com/TriadStudios/echoes-server-manager/internal/cli"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/instances"
	"github.com/TriadStudios/echoes-server-manager/internal/service"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// serveStopWithin is how soon esm serve must end after its stop signal
// with no request open: well inside the 30 seconds it gives open requests,
// and far inside the TimeoutStopSec=60 of its unit.
const serveStopWithin = 10 * time.Second

// buildESM builds the esm binary into a temporary folder.
func buildESM(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "esm"+testkit.ExeSuffix())
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, "github.com/TriadStudios/echoes-server-manager/cmd/esm")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/esm: %v\n%s", err, b)
	}
	return out
}

// fileText shows a file's content when it is printed (for messages made
// before the file is complete).
type fileText string

func (f fileText) String() string {
	b, err := os.ReadFile(string(f))
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// esm serve, the binary, ends within seconds of its stop signal while a
// server it started is running, and leaves that server running; the next
// ESM adopts it. On Linux the stop is a SIGINT. On Windows it is a Ctrl+C
// on the console of esm serve, which runs in a process group of its own:
// Windows starts such a process with Ctrl+C turned off, as it does a
// background job of Git Bash or one started with start /b.
func TestServeEndsAtItsStopSignal(t *testing.T) {
	t.Parallel()
	exe := buildESM(t)
	// A short root keeps the socket path under the platform limit. Removed
	// last, after every process in it has ended.
	root, err := os.MkdirTemp("", "esm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	src := t.TempDir()
	blk := int(portBlock.Add(1))
	cfg := map[string]any{
		"listen":           "127.0.0.1:0",
		"game_ports":       fmt.Sprintf("%d-%d", 30000+blk*20, 30000+blk*20+9),
		"query_ports":      fmt.Sprintf("%d-%d", 31000+blk*20, 31000+blk*20+9),
		"supervisor":       "process",
		"stop_timeout":     "5s",
		"ready_timeout":    "10s",
		"log_level":        "debug",
		"build_import_dir": src,
		// The binary is no test binary: it would look up releases.
		"update_check": false,
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(root, "esm.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "serve.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	serveLog := fileText(logPath)

	// ESM leaves its server running; the test does not, whatever failed.
	// This runs after esm serve has ended (so nothing restarts the server)
	// and before the root goes. The pid comes from the status once it is
	// known, else from the supervisor's pid record, which is written when
	// the server starts.
	var pid int
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		if pid <= 0 {
			var rec struct {
				PID int `json:"pid"`
			}
			if b, err := os.ReadFile(filepath.Join(root, "instances", "alpha", "run", "pid.json")); err == nil && json.Unmarshal(b, &rec) == nil {
				pid = rec.PID
			}
		}
		if pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	})

	cmd := exec.Command(exe, "--root", root, "serve")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	serveAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			cmd.Process.Kill()
			<-exited
		}
		logFile.Close()
	})

	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := cli.Main(append([]string{"--root", root}, args...), &out, &errOut, "test")
		return code, out.String(), errOut.String()
	}
	must := func(args ...string) string {
		t.Helper()
		code, out, errOut := run(args...)
		if code != 0 {
			t.Fatalf("esm %s: %d %s %s\nesm serve:\n%s", strings.Join(args, " "), code, out, errOut, serveLog)
		}
		return out
	}
	status := func() instances.View {
		t.Helper()
		var v instances.View
		if err := json.Unmarshal([]byte(must("--json", "status", "alpha")), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Wait until esm serve answers on its socket, and stop waiting when it
	// ends first.
	for deadline := time.Now().Add(30 * time.Second); ; {
		if code, _, _ := run("status"); code == 0 {
			break
		}
		select {
		case <-exited:
			t.Fatalf("esm serve ended (exit code %d) before it answered on its socket\n%s", cmd.ProcessState.ExitCode(), serveLog)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("esm serve does not answer on its socket\n%s", serveLog)
		}
	}

	build := testkit.MakeBuild(t, src, testkit.BuildOptions{Version: "1.0"})
	must("build", "install", build)
	must("create", "alpha", "--bind", "127.0.0.1", "--set", "FakeReadyDelay=100ms", "--set", "FakeSaveInterval=200ms")
	must("start", "alpha")
	v := status()
	pid = v.Status.PID
	if v.Status.State != instances.Running || pid <= 0 {
		t.Fatalf("after start: %+v", v.Status)
	}
	testkit.CheckLoopback(t, cmd.Process.Pid, pid)

	sent := time.Now()
	stopServe(t, cmd)
	select {
	case <-exited:
	case <-time.After(serveStopWithin):
		t.Fatalf("esm serve did not end within %s of its stop signal\n%s", serveStopWithin, serveLog)
	}
	t.Logf("esm serve ended %s after its stop signal", time.Since(sent).Round(time.Millisecond))
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("esm serve ended with exit code %d\n%s", code, serveLog)
	}
	if !strings.Contains(serveLog.String(), "leaving 1 game servers running") {
		t.Fatalf("esm serve did not report the running server:\n%s", serveLog)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gameserver.Probe(ctx, "127.0.0.1", v.Ports.Query); err != nil {
		t.Fatalf("the server did not survive the stop of esm serve: %v", err)
	}

	// The next ESM adopts the server, and stops it as ESM stops servers.
	svc, err := service.Start(service.Options{
		Root: root, Version: "test", Stderr: &lockedBuffer{},
		WatchInterval: 50 * time.Millisecond, SchedulerInterval: time.Hour, PortHost: "127.0.0.1",
		AuthOptions: auth.Options{Iterations: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		svc.Shutdown(ctx)
	})
	if v := status(); v.Status.State != instances.Running || v.Status.PID != pid || v.Status.ReadySource != instances.ReadyAdopted {
		t.Fatalf("after the restart: %+v, want pid %d adopted", v.Status, pid)
	}
	must("stop", "alpha")
	stopped = true
	if v := status(); v.Status.State != instances.Stopped {
		t.Fatalf("after the stop: %+v", v.Status)
	}
}
