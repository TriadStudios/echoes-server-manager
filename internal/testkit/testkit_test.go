package testkit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

func TestMakeBuildManifest(t *testing.T) {
	parent := t.TempDir()
	dir := MakeBuild(t, parent, BuildOptions{
		Version: "1.2.3",
		Channel: "test",
		Fake:    map[string]any{"FakeFailStart": true},
		Extra:   map[string][]byte{"Content/readme.txt": []byte("hello")},
	})
	if dir != filepath.Join(parent, "1.2.3") {
		t.Fatalf("dir %q", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "1.2.3" || m.Channel != "test" || m.Commit != "fake-1.2.3" {
		t.Fatalf("manifest %+v", m)
	}
	if m.Executables["linux"] != LinuxExe || m.Executables["windows"] != WindowsExe {
		t.Fatalf("executables %v", m.Executables)
	}
	want := []string{"Content/readme.txt", "linux/esm-fakeserver", "linux/fake.json", "windows/esm-fakeserver.exe", "windows/fake.json"}
	var got []string
	for rel, sum := range m.Files {
		got = append(got, rel)
		real, err := FileSHA256(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || real != sum {
			t.Errorf("%s: hash %s, manifest %s (%v)", rel, real, sum, err)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("files %v, want %v", got, want)
	}
	exe := filepath.Join(dir, filepath.FromSlash(m.Executables[runtime.GOOS]))
	a, _ := FileSHA256(exe)
	b2, _ := FileSHA256(FakeServer(t))
	if a != b2 {
		t.Fatal("this OS's executable is not the fake server")
	}

	only := MakeBuild(t, parent, BuildOptions{Version: "only", ThisOSOnly: true})
	b, _ = os.ReadFile(filepath.Join(only, "manifest.json"))
	m = Manifest{}
	json.Unmarshal(b, &m)
	if len(m.Executables) != 1 || m.Executables[runtime.GOOS] == "" {
		t.Fatalf("ThisOSOnly executables %v", m.Executables)
	}
}

func TestArchives(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("data"), 0o644)
	os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644)
	out := t.TempDir()

	zp := filepath.Join(out, "x.zip")
	MakeZip(t, src, zp)
	zr, err := zip.OpenReader(zp)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	zr.Close()
	if names["a/b/f.txt"] != "data" || names["top.txt"] != "top" {
		t.Fatalf("zip content %v", names)
	}

	tp := filepath.Join(out, "x.tar.gz")
	WriteTarGz(t, tp, append(FolderEntries(t, src),
		Entry{Name: "../evil", Body: []byte("x")},
		Entry{Name: "link", Symlink: "/etc/passwd"},
		Entry{Name: "hard", Hardlink: "top.txt"},
		Entry{Name: "dev", Device: true},
	))
	f, _ := os.Open(tp)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	types := map[string]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types[h.Name] = h.Typeflag
	}
	if types["../evil"] != tar.TypeReg || types["link"] != tar.TypeSymlink || types["hard"] != tar.TypeLink ||
		types["dev"] != tar.TypeChar || types["a/b/f.txt"] != tar.TypeReg {
		t.Fatalf("tar types %v", types)
	}
}

func TestWriteControl(t *testing.T) {
	d := t.TempDir()
	if err := WriteControl(d, ControlCrash); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, "control", "crash")); err != nil {
		t.Fatal(err)
	}
}
