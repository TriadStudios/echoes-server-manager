package builds_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// The app id file goes next to the server binary for this OS, read-only
// like the rest of the build; a build without a configured app id gets
// none, and a build that brings its own keeps it.
func TestSteamAppIDPlaced(t *testing.T) {
	ctx := context.Background()
	exeRel := testkit.LinuxExe
	if runtime.GOOS == "windows" {
		exeRel = testkit.WindowsExe
	}
	idFile := func(r *rig, version string) string {
		p, err := r.m.ExecutablePath(version)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(filepath.Dir(p), gameserver.SteamAppIDFile)
	}

	r := newRig(t, builds.Options{SteamAppID: "1000001"})
	b, err := r.m.Install(ctx, testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.0"}), "")
	if err != nil {
		t.Fatal(err)
	}
	p := idFile(r, "1.0")
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "1000001" {
		t.Fatalf("app id file: %q %v", got, err)
	}
	if filepath.Dir(p) != filepath.Dir(filepath.Join(r.st.Path(), "builds", "1.0", filepath.FromSlash(exeRel))) {
		t.Fatalf("app id file at %s, not next to the server binary", p)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("app id file is writable: %v", fi.Mode())
	}
	if b.DefaultMap != testkit.DefaultMap || b.Files != 3 {
		t.Fatalf("build %+v", b)
	}

	// A build with its own file keeps it.
	own := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.1",
		Extra: map[string][]byte{filepath.ToSlash(filepath.Join(filepath.Dir(exeRel), gameserver.SteamAppIDFile)): []byte("2000002")}})
	if _, err := r.m.Install(ctx, own, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(idFile(r, "1.1")); string(got) != "2000002" {
		t.Fatalf("the build's own app id file was replaced: %q", got)
	}

	// No app id configured: no file.
	r2 := newRig(t, builds.Options{})
	if _, err := r2.m.Install(ctx, testkit.MakeBuild(t, r2.src, testkit.BuildOptions{Version: "1.0"}), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idFile(r2, "1.0")); !os.IsNotExist(err) {
		t.Fatalf("app id file without an app id: %v", err)
	}
}

func TestManifestDefaultMap(t *testing.T) {
	base := `{"version":"1.0","commit":"c","channel":"live","executables":{"linux":"bin/s"},"files":{"bin/s":"` + strings.Repeat("0", 64) + `"}`
	for m, ok := range map[string]bool{
		"":                          true,
		"TestMap":                   true,
		"/Levels/Example/Example":   true,
		"/Levels/Example/Example.X": true,
		"-Port=1":                   false,
		"a b":                       false,
		"/Levels/../x":              false,
		"a;b":                       false,
		strings.Repeat("m", 129):    false,
	} {
		man, err := builds.ParseManifest([]byte(base + `,"default_map":"` + m + `"}`))
		if err != nil {
			t.Fatalf("%q: %v", m, err)
		}
		if err := man.Check("linux"); (err == nil) != ok {
			t.Errorf("default_map %q: %v, want ok=%v", m, err, ok)
		}
		if ok && man.DefaultMap != m {
			t.Errorf("default_map %q read as %q", m, man.DefaultMap)
		}
	}
}
