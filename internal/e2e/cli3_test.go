package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/api"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/cli"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// runCLIIn is runCLI with standard input.
func (h *harness) runCLIIn(stdin string, args ...string) (int, string, string) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	code := cli.MainIn(append([]string{"--root", h.root}, args...), strings.NewReader(stdin), &out, &errOut, "test")
	return code, out.String(), errOut.String()
}

// build manifest writes the manifest a folder needs, and the folder then
// installs; steam password reads the password from standard input and
// never prints it; service install is for Linux only.
func TestCLIManifestSteamAndService(t *testing.T) {
	h := newESM(t, func(cfg map[string]any) {
		cfg["steam_sources"] = []map[string]any{{"name": "main", "app_id": "1000001", "branch": "testing",
			"executable": map[string]string{"linux": "a/b"}, "channel": "live", "check_every": "0"}}
	})
	dir := filepath.Join(h.src, "made")
	exes := testkit.MakeContent(t, dir, testkit.BuildOptions{Version: "x", Extra: map[string][]byte{"Data/a.txt": []byte("a")}})
	args := []string{"build", "manifest", dir, "--version", "2.0", "--channel", "test", "--default-map", testkit.DefaultMap}
	for goos, rel := range exes {
		args = append(args, "--executable", goos+"="+rel)
	}
	code, out, errOut := h.runCLI(args...)
	if code != 0 || !strings.Contains(oneLine(t, "build manifest", out), "build 2.0 (test)") {
		t.Fatalf("build manifest: %d %q %q", code, out, errOut)
	}
	b, err := os.ReadFile(filepath.Join(dir, builds.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	man, err := builds.ParseManifest(b)
	if err != nil || man.Files["Data/a.txt"] == "" || man.Executables[runtime.GOOS] == "" {
		t.Fatalf("manifest %s: %v", b, err)
	}
	code, out, errOut = h.runCLI("build", "install", dir)
	if code != 0 || !strings.Contains(out, "installed build 2.0") {
		t.Fatalf("install the made folder: %d %q %q", code, out, errOut)
	}
	if code, _, errOut = h.runCLI("build", "manifest", dir, "--version", "2.1", "--executable", "linux=missing/bin"); code == 0 {
		t.Fatal("a manifest with a binary that is not there was written")
	} else if !strings.Contains(errOut, "missing/bin") {
		t.Fatalf("error %q", errOut)
	}

	code, out, errOut = h.runCLIIn("Cli-Pw123\n", "steam", "password", "main")
	if code != 0 || strings.Contains(out+errOut, "Cli-Pw123") || !strings.Contains(oneLine(t, "steam password", out), "set the branch password of main") {
		t.Fatalf("steam password: %d %q %q", code, out, errOut)
	}
	if code, _, errOut = h.runCLIIn("", "steam", "password", "main"); code == 0 || !strings.Contains(errOut, "standard input") {
		t.Fatalf("no password on stdin: %d %q", code, errOut)
	}
	code, out, _ = h.runCLI("steam", "list", "--json")
	var sl api.SteamList
	if code != 0 || json.Unmarshal([]byte(oneLine(t, "steam list", out)), &sl) != nil || len(sl.Sources) != 1 || !sl.Sources[0].PasswordSet ||
		strings.Contains(out, "Cli-Pw123") {
		t.Fatalf("steam list: %d %q", code, out)
	}
	if code, out, _ = h.runCLIIn("", "steam", "password", "main", "--clear"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("clear: %d %q", code, out)
	}
	var raw json.RawMessage
	h.must(http.StatusOK, "GET", "/api/v1/steam", nil, &raw)
	if !strings.Contains(string(raw), `"password_set":false`) {
		t.Fatalf("after clear: %s", raw)
	}

	if runtime.GOOS != "linux" {
		if code, _, errOut = h.runCLI("service", "install"); code == 0 || !strings.Contains(errOut, "Linux only") {
			t.Fatalf("service install off Linux: %d %q", code, errOut)
		}
		return
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	code, out, errOut = h.runCLI("service", "install")
	if code != 0 || !strings.Contains(oneLine(t, "service install", out), "loginctl enable-linger") {
		t.Fatalf("service install: %d %q %q", code, out, errOut)
	}
	unit, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "esm.service"))
	if err != nil || !strings.Contains(string(unit), "serve --root \""+h.root+"\"") || !strings.Contains(string(unit), "Restart=always") {
		t.Fatalf("unit: %v\n%s", err, unit)
	}
}
