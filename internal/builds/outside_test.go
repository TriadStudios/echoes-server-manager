package builds_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

// A local source is read from the import folder only. What is outside it is
// refused as such (source_not_allowed), by the check and by the install alike,
// and never answered as a name that is not there (invalid): on every system,
// with the paths that system writes. On Windows more is outside than "an
// absolute path elsewhere": "/etc" and "\etc" are no absolute paths there
// (filepath.IsAbs says no) but name the root of the current drive, "C:etc"
// names a place on a drive, and a UNC path names a share; none of them is a
// name below the import folder.
func TestWhatIsOutsideTheImportFolderIsNotAllowedOnEverySystem(t *testing.T) {
	r := newRig(t, builds.Options{})
	imports := r.src
	vol := filepath.VolumeName(imports)
	sep := string(filepath.Separator)
	cases := []struct{ name, source string }{
		{"an absolute path elsewhere", filepath.Join(t.TempDir(), "outside")},
		{"the root of the drive, or of the system", vol + sep},
		{"the folder that holds the import folder", filepath.Dir(imports)},
		{"a path that goes up", filepath.Join("..", "outside")},
		{"a path that goes up, with a slash", "../outside"},
		{"a path that goes up from below", filepath.Join("below", "..", "..", "outside")},
		{"a path that starts with a slash", "/outside-of-everything"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, []struct{ name, source string }{
			{"a path that starts with a backslash", `\outside-of-everything`},
			{"a path that goes up, with a backslash", `..\outside`},
			{"a place on the current drive", vol + "outside"},
			{"a place on a drive that is another", `Z:\outside`},
			{"a UNC path", `\\server\share\outside`},
			{"the root of the drive with a slash", vol + "/"},
		}...)
	}
	for _, c := range cases {
		check := r.m.CheckInstall(c.source, "")
		_, install := r.m.Install(context.Background(), c.source, "")
		if code(check) != "source_not_allowed" || code(install) != "source_not_allowed" {
			t.Errorf("%s (%q): the check says %v (%s), the install %v (%s); want source_not_allowed from both", c.name, c.source, check, code(check), install, code(install))
		}
	}
	r.noLeftovers(t)
	if list, _ := r.m.List(); len(list) != 0 {
		t.Fatalf("something outside the import folder was installed: %+v", list)
	}

	// What is inside it is taken, however it is written, and the check and the
	// install agree: the folder as an absolute path, with slashes, as a name
	// below the import folder, and, where it names the same place, as a path
	// that starts at the root of its drive. That is the same path on Linux; on
	// Windows it is taken for the root of the drive that the process is on, so
	// it is only a path inside the import folder when that is the drive of the
	// import folder.
	folder := testkit.MakeBuild(t, imports, testkit.BuildOptions{Version: "1.0.0"})
	inside := []struct{ name, source string }{
		{"an absolute path", folder},
		{"an absolute path with slashes", filepath.ToSlash(folder)},
		{"a name below the import folder", "1.0.0"},
		{"a name that goes up and back in", filepath.Join("below", "..", "1.0.0")},
	}
	if wd, err := os.Getwd(); err == nil && strings.EqualFold(filepath.VolumeName(wd), vol) {
		inside = append(inside, struct{ name, source string }{"a path from the root of its drive", strings.TrimPrefix(folder, vol)})
	}
	for _, c := range inside {
		if err := r.m.CheckInstall(c.source, ""); err != nil {
			t.Errorf("%s (%q): the check says %v", c.name, c.source, err)
		}
	}
	if _, err := r.m.Install(context.Background(), inside[0].source, ""); err != nil {
		t.Fatalf("install of %q (%s): %v", inside[0].source, inside[0].name, err)
	}
	for _, c := range inside {
		if _, err := r.m.Install(context.Background(), c.source, ""); code(err) != "build_exists" {
			t.Errorf("%s (%q): the install says %v, want build_exists (the source was taken, and the build is there)", c.name, c.source, err)
		}
	}
	r.noLeftovers(t)
}
