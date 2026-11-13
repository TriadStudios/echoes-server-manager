package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
)

// Made-up ELF headers: magic, then the class byte (1 = 32-bit, 2 = 64-bit).
var (
	elf64 = append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 57)...)
	elf32 = append([]byte("\x7fELF\x01\x01\x01"), make([]byte, 45)...)
)

func writeFile(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCheckELF64(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		content []byte
		want    string
	}{
		"good":  {elf64, ""},
		"x32":   {elf32, "32-bit"},
		"text":  {[]byte("#!/bin/sh\necho hi\n"), "not an ELF file"},
		"short": {[]byte("\x7fEL"), "too short"},
		"empty": {nil, "too short"},
	} {
		p := filepath.Join(dir, name)
		writeFile(t, p, c.content, 0o600)
		err := checkELF64(p)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestSteamRuntimeCheck(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "steam", "linux64")
	run := func() Check {
		var r Report
		checkSteamRuntime(&r, dir, "linux")
		return r.Checks[0]
	}
	if c := run(); c.Status != Fail || !strings.Contains(c.Message, "does not exist") {
		t.Fatalf("missing folder: %+v", c)
	}
	writeFile(t, filepath.Join(dir, "steamclient.so"), elf32, 0o600)
	if c := run(); c.Status != Fail || !strings.Contains(c.Message, "steamclient.so is a 32-bit ELF file") ||
		!strings.Contains(c.Message, "libsteamwebrtc.so is missing") {
		t.Fatalf("32-bit and missing library: %+v", c)
	}
	writeFile(t, filepath.Join(dir, "steamclient.so"), elf64, 0o600)
	writeFile(t, filepath.Join(dir, "libsteamwebrtc.so"), []byte("not a library"), 0o600)
	if c := run(); c.Status != Fail || !strings.Contains(c.Message, "libsteamwebrtc.so is not an ELF file") {
		t.Fatalf("not an ELF file: %+v", c)
	}
	writeFile(t, filepath.Join(dir, "libsteamwebrtc.so"), elf64, 0o600)
	if c := run(); c.Status != OK || !strings.Contains(c.Message, "LD_LIBRARY_PATH") {
		t.Fatalf("both there: %+v", c)
	}
	var r Report
	checkSteamRuntime(&r, filepath.Join(t.TempDir(), "none"), "windows")
	if c := r.Checks[0]; c.Status != OK || !strings.Contains(c.Message, "not checked") {
		t.Fatalf("on windows: %+v", c)
	}
}

// makeBuild writes builds/<version> with a manifest naming exe as the
// linux server binary.
func makeBuild(t *testing.T, root, version, exe string, content []byte, mode os.FileMode) string {
	t.Helper()
	dir := filepath.Join(root, builds.Dir, version)
	if content != nil {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(exe)), content, mode)
	}
	man, _ := json.Marshal(builds.Manifest{Version: version, Channel: "live", Executables: map[string]string{"linux": exe},
		Files: map[string]string{exe: strings.Repeat("0", 64)}})
	writeFile(t, filepath.Join(dir, builds.ManifestName), man, 0o600)
	return dir
}

func TestBuildsCheck(t *testing.T) {
	root := t.TempDir()
	run := func(appID string) Check {
		var r Report
		checkBuilds(&r, root, "linux", appID)
		return r.Checks[0]
	}
	if c := run(""); c.Status != OK || !strings.Contains(c.Message, "no builds") {
		t.Fatalf("no builds: %+v", c)
	}
	good := makeBuild(t, root, "good-1", "Game/Binaries/Linux/GameServer", elf64, 0o700)
	if c := run(""); c.Status != OK || !strings.Contains(c.Message, "1 build") {
		t.Fatalf("one good build: %+v", c)
	}
	// With an app id configured, a build without the app id file next to
	// its binary is pointed out.
	if c := run("480"); c.Status != Warn || !strings.Contains(c.Message, "good-1 has no "+gameserver.SteamAppIDFile) {
		t.Fatalf("no app id file: %+v", c)
	}
	writeFile(t, filepath.Join(good, "Game", "Binaries", "Linux", gameserver.SteamAppIDFile), []byte("480"), 0o400)
	if c := run("480"); c.Status != OK {
		t.Fatalf("app id file in place: %+v", c)
	}
	if c := run("481"); c.Status != Warn || !strings.Contains(c.Message, "another app id") {
		t.Fatalf("other app id: %+v", c)
	}
	makeBuild(t, root, "x32", "bin/server", elf32, 0o700)
	makeBuild(t, root, "noexec", "bin/server", elf64, 0o600)
	makeBuild(t, root, "missing", "bin/server", nil, 0)
	makeBuild(t, root, "script", "bin/server", []byte("#!/bin/sh\n"), 0o700)
	os.MkdirAll(filepath.Join(root, builds.Dir, ".info"), 0o700)
	c := run("")
	wants := []string{"x32: server binary bin/server is a 32-bit ELF file", "missing: server binary bin/server:",
		"script: server binary bin/server is not an ELF file"}
	if runtime.GOOS != "windows" { // Windows files have no executable bit
		wants = append(wants, "noexec: server binary bin/server is not executable")
	}
	for _, want := range wants {
		if c.Status != Fail || !strings.Contains(c.Message, want) {
			t.Errorf("builds check lacks %q: %+v", want, c)
		}
	}
	if strings.Contains(c.Message, "good-1") {
		t.Errorf("the good build is named: %+v", c)
	}
}

// Run includes the new checks.
func TestRunChecksSteam(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	r := Run(Options{Root: root, PortHost: "127.0.0.1", GOOS: "linux"})
	if c := find(r, "steam_app_id"); c.Status != Warn {
		t.Fatalf("steam_app_id: %+v", c)
	}
	if c := find(r, "steam_runtime"); c.Status != Fail || !strings.Contains(c.Message, filepath.Join(root, "steam", "linux64")) {
		t.Fatalf("steam_runtime: %+v", c)
	}
	if c := find(r, "builds"); c.Status != OK {
		t.Fatalf("builds: %+v", c)
	}
	writeFile(t, filepath.Join(root, "esm.json"), []byte(`{"steam_app_id":"480","steam_runtime_dir":"rt"}`), 0o600)
	writeFile(t, filepath.Join(root, "rt", "steamclient.so"), elf64, 0o600)
	writeFile(t, filepath.Join(root, "rt", "libsteamwebrtc.so"), elf64, 0o600)
	r = Run(Options{Root: root, PortHost: "127.0.0.1", GOOS: "linux"})
	if c := find(r, "steam_app_id"); c.Status != OK {
		t.Fatalf("steam_app_id set: %+v", c)
	}
	if c := find(r, "steam_runtime"); c.Status != OK {
		t.Fatalf("steam_runtime in place: %+v", c)
	}
}
