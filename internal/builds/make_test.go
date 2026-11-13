package builds_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
)

func makeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMakeManifest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	makeFile(t, dir, "bin/server", "server")
	makeFile(t, dir, "Data/a.txt", "a")
	makeFile(t, dir, "manifest.json", "an old one")
	makeFile(t, dir, "steamapps/appmanifest_1.acf", "skipped")
	spec := builds.ManifestSpec{Version: "1.0", Channel: "test", Executables: map[string]string{"linux": "bin/server"}, Skip: []string{"steamapps"}}
	man, err := builds.MakeManifest(ctx, dir, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != 2 || man.Files["Data/a.txt"] != "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb" {
		t.Fatalf("files %v", man.Files)
	}
	if _, err := builds.ParseManifest(man.Encode()); err != nil {
		t.Fatal(err)
	}

	bad := func(name string, spec builds.ManifestSpec, max int, code string) {
		t.Helper()
		_, err := builds.MakeManifest(ctx, dir, spec, max)
		if fault.CodeOf(err) != code {
			t.Errorf("%s: %v, want %s", name, err, code)
		}
	}
	bad("binary not there", builds.ManifestSpec{Version: "1.0", Channel: "test", Executables: map[string]string{"linux": "bin/nope"}}, 0, "bad_manifest")
	bad("no binary", builds.ManifestSpec{Version: "1.0", Channel: "test"}, 0, "invalid")
	bad("bad version", builds.ManifestSpec{Version: "../x", Channel: "test", Executables: map[string]string{"linux": "bin/server"}}, 0, "bad_manifest")
	bad("bad channel", builds.ManifestSpec{Version: "1.0", Channel: "beta", Executables: map[string]string{"linux": "bin/server"}}, 0, "bad_manifest")
	bad("too many files", spec, 1, "too_large")

	if err := os.Symlink(filepath.Join(dir, "Data"), filepath.Join(dir, "link")); err == nil {
		bad("a link", spec, 0, "unsafe_archive")
		os.Remove(filepath.Join(dir, "link"))
	} else {
		t.Logf("no symbolic link check here: %v", err)
	}
	makeFile(t, dir, "data/a.txt", "a")
	if _, err := os.Stat(filepath.Join(dir, "Data", "A.TXT")); err != nil {
		// A case-sensitive file system holds both names.
		bad("names that differ in case", spec, 0, "unsafe_archive")
	}
}
