package svcunit

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
