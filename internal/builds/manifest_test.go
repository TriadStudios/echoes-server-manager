package builds

import (
	"runtime"
	"strings"
	"testing"
)

func TestCleanName(t *testing.T) {
	good := map[string]string{
		"a.txt":         "a.txt",
		"./a/b.txt":     "a/b.txt",
		"dir/":          "dir",
		"Content/x.pak": "Content/x.pak",
	}
	for in, want := range good {
		if got, err := CleanName(in); err != nil || got != want {
			t.Errorf("CleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "/", "/abs", "../x", "a/../../x", "a/../b", `a\b`, "C:/x", "a//b", "a/./b", "x\x00y", "line\nbreak"}
	if runtime.GOOS == "windows" {
		bad = append(bad, "NUL", "dir/COM1")
	}
	for _, in := range bad {
		if got, err := CleanName(in); err == nil {
			t.Errorf("CleanName(%q) = %q, want an error", in, got)
		}
	}
}

func TestManifestCheck(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	ok := func() *Manifest {
		return &Manifest{Version: "1.0", Channel: "live",
			Executables: map[string]string{"linux": "bin/s", "windows": "bin/s.exe"},
			Files:       map[string]string{"bin/s": sum, "bin/s.exe": sum}}
	}
	if err := ok().Check("linux"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(m *Manifest){
		"bad version":         func(m *Manifest) { m.Version = "../x" },
		"bad channel":         func(m *Manifest) { m.Channel = "beta" },
		"no files":            func(m *Manifest) { m.Files = nil },
		"unclean file name":   func(m *Manifest) { m.Files["./bin/t"] = sum },
		"escaping file":       func(m *Manifest) { m.Files["../t"] = sum },
		"upper-case hash":     func(m *Manifest) { m.Files["bin/s"] = strings.ToUpper(sum) },
		"short hash":          func(m *Manifest) { m.Files["bin/s"] = "abc" },
		"lists itself":        func(m *Manifest) { m.Files[ManifestName] = sum },
		"unlisted executable": func(m *Manifest) { m.Executables["linux"] = "bin/other" },
		"no linux executable": func(m *Manifest) { delete(m.Executables, "linux") },
	}
	for name, mutate := range cases {
		m := ok()
		mutate(m)
		if err := m.Check("linux"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseManifest([]byte(`{"version":"1","unknown":true}`)); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	for _, v := range []string{"1.0.0", "v2", "2026.11.11-test_1"} {
		if !ValidVersion(v) {
			t.Errorf("version %q refused", v)
		}
	}
	for _, v := range []string{"", ".", "..", "-x", ".hidden", "a/b", strings.Repeat("x", 65)} {
		if ValidVersion(v) {
			t.Errorf("version %q accepted", v)
		}
	}
}
