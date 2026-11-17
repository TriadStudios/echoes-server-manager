package svcunit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func abs(p string) string {
	if runtime.GOOS == "windows" {
		return "C:" + p // a unit is Linux-only; forward slashes keep it plain here
	}
	return p
}

func TestUnit(t *testing.T) {
	u, err := Unit(abs("/home/ops/bin/esm"), abs("/home/ops/durinn 50%"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(u)
	for _, want := range []string{
		"ExecStart=\"" + abs("/home/ops/bin/esm") + "\" serve --root \"" + abs("/home/ops/durinn 50%%") + "\"\n",
		"Restart=always\n",
		"RestartSec=5\n",
		"KillMode=process\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unit lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"esm", abs(`/a"b`), abs("/a\nb"), abs(`/a\b`)} {
		if _, err := Unit(bad, abs("/root")); err == nil {
			t.Errorf("binary %q accepted", bad)
		}
	}
	if _, err := Unit(abs("/bin/esm"), abs("/a$HOME")); err != nil {
		t.Fatal(err)
	} else if u, _ := Unit(abs("/bin/esm"), abs("/a$HOME")); !strings.Contains(string(u), "$$HOME") {
		t.Errorf("$ not doubled: %s", u)
	}
}

// The unit runs the rollback script before every start, ignoring its failure
// (the "-"), and never lets systemd give up on a start that keeps failing: the
// rollback is at the 3rd start.
func TestUnitRunsTheRollbackScriptBeforeEveryStart(t *testing.T) {
	u, err := Unit(abs("/home/ops/bin 50%/esm"), abs("/home/ops/durinn"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(u)
	pre := "ExecStartPre=-/bin/sh \"" + abs("/home/ops/bin 50%%/esm") + ".update.sh\" \"" + abs("/home/ops/bin 50%%/esm") + "\"\n"
	if !strings.Contains(s, pre) {
		t.Fatalf("unit lacks %q:\n%s", pre, s)
	}
	at := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("unit lacks %q:\n%s", sub, s)
		}
		return i
	}
	if !(at("[Unit]") < at("StartLimitIntervalSec=0\n") && at("StartLimitIntervalSec=0\n") < at("[Service]")) {
		t.Errorf("StartLimitIntervalSec=0 is not in [Unit]:\n%s", s)
	}
	if !(at("[Service]") < at("ExecStartPre=") && at("ExecStartPre=") < at("ExecStart=\"")) {
		t.Errorf("ExecStartPre is not in [Service] before ExecStart:\n%s", s)
	}
	if strings.Count(s, "ExecStartPre=") != 1 {
		t.Errorf("more than one ExecStartPre:\n%s", s)
	}
	// A unit of an old ESM, written before the rollback, differs: it is
	// replaced only with force.
	dir := t.TempDir()
	old := "[Unit]\nDescription=Echoes Server Manager\n\n[Service]\nExecStart=\"" + abs("/opt/esm/esm") + "\" serve --root \"" + abs("/opt/durinn") + "\"\nRestart=always\n"
	os.WriteFile(filepath.Join(dir, Name), []byte(old), 0o644)
	if _, err := Write(dir, abs("/opt/esm/esm"), abs("/opt/durinn"), false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("an old unit without force: %v", err)
	}
	if _, err := Write(dir, abs("/opt/esm/esm"), abs("/opt/durinn"), true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, Name)); !strings.Contains(string(b), "ExecStartPre=") {
		t.Fatalf("the replaced unit has no rollback:\n%s", b)
	}
}

// The script is a file next to the binary; a script that is there and is the
// same is left alone, and one that differs (an older ESM's) is replaced.
func TestWriteScript(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "esm")
	path, err := WriteScript(exe)
	if err != nil {
		t.Fatal(err)
	}
	if path != exe+".update.sh" || path != ScriptPath(exe) {
		t.Fatalf("path %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(Script()) || !strings.HasPrefix(string(got), "#!/bin/sh\n") {
		t.Fatalf("script: %v\n%s", err, got)
	}
	if strings.Contains(string(got), "\r") {
		t.Error("the script has carriage returns: /bin/sh would not run it")
	}
	if _, err := WriteScript(exe); err != nil {
		t.Fatalf("same script again: %v", err)
	}
	os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o644)
	if _, err := WriteScript(exe); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(Script()) {
		t.Fatalf("an older script was not replaced:\n%s", got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".esm-script-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// A folder that cannot be written to is an error the caller can report
	// while the unit goes on working.
	if _, err := WriteScript(filepath.Join(t.TempDir(), "missing", "esm")); err == nil {
		t.Fatal("the script was written into a folder that is not there")
	}
	// Script hands out a copy.
	s := Script()
	s[0] = 'X'
	if Script()[0] != '#' {
		t.Fatal("Script shares its bytes")
	}
}

// ESM brings the script next to its binary up to date when it starts: one
// that differs from the built-in one is replaced (atomically, 0644), one that
// is the same is left alone, and one that is not there is never made, so a
// box that does not run as the unit stays as clean as it was.
func TestRefreshScript(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "esm")
	if changed, err := RefreshScript(exe); changed || err != nil {
		t.Fatalf("no script: %v, %v", changed, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("RefreshScript made files: %v", entries)
	}

	old := []byte("#!/bin/sh\n# an older script\nexit 0\n")
	os.WriteFile(ScriptPath(exe), old, 0o600)
	changed, err := RefreshScript(exe)
	if !changed || err != nil {
		t.Fatalf("an older script: %v, %v", changed, err)
	}
	if got, _ := os.ReadFile(ScriptPath(exe)); string(got) != string(Script()) {
		t.Fatalf("the older script was not replaced:\n%s", got)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(ScriptPath(exe)); err != nil || fi.Mode().Perm() != 0o644 {
			t.Fatalf("mode of the new script: %v, %v", fi, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("files next to the binary after the refresh: %v", entries)
	}

	// The same script: nothing is written (it keeps its time).
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(ScriptPath(exe), stamp, stamp)
	if changed, err := RefreshScript(exe); changed || err != nil {
		t.Fatalf("the same script: %v, %v", changed, err)
	}
	if fi, _ := os.Stat(ScriptPath(exe)); !fi.ModTime().Equal(stamp) {
		t.Fatal("the same script was written again")
	}

	// Something that cannot be read as a script is an error, not a rewrite.
	odd := filepath.Join(t.TempDir(), "esm")
	os.MkdirAll(ScriptPath(odd), 0o755)
	if changed, err := RefreshScript(odd); changed || err == nil {
		t.Fatalf("a folder in the script's place: %v, %v", changed, err)
	}
}

func TestWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "systemd", "user")
	exe, root := abs("/opt/esm/esm"), abs("/opt/durinn")
	path, err := Write(dir, exe, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Write(dir, exe, root, false); err != nil {
		t.Fatalf("same unit again: %v", err)
	}
	if _, err := Write(dir, exe, abs("/srv/other"), false); err == nil {
		t.Fatal("a different unit replaced the existing one without --force")
	}
	if _, err := Write(dir, exe, abs("/srv/other"), true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "other") {
		t.Fatalf("not replaced: %s", b)
	}
}

// With ESM_TEST_SYSTEMD=1 on Linux, systemd itself checks the unit.
func TestUnitVerifiesWithSystemd(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("ESM_TEST_SYSTEMD") != "1" {
		t.Skip("needs Linux and ESM_TEST_SYSTEMD=1")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("no systemd-analyze")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path, err := Write(dir, exe, t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("systemd-analyze", "--user", "verify", path).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Logf("systemd-analyze says:\n%s", out)
	}
}
