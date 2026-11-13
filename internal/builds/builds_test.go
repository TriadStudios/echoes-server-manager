package builds_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

type rig struct {
	st  *store.Store
	m   *builds.Manager
	bus *events.Bus
	src string // folder for source builds and archives
}

func newRig(t *testing.T, o builds.Options) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "root"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus(64)
	o.Bus = bus
	src := t.TempDir()
	if o.ImportDir == "" {
		o.ImportDir = src
	}
	return &rig{st: st, m: builds.New(st, o), bus: bus, src: src}
}

func code(err error) string { return fault.CodeOf(err) }

// leftovers fails when an install left anything in tmp/.
func (r *rig) noLeftovers(t *testing.T) {
	t.Helper()
	entries, _ := r.st.ReadDir("tmp")
	if len(entries) != 0 {
		t.Fatalf("tmp/ not empty after install: %v", entries)
	}
}

func TestInstallFolderZipTarGz(t *testing.T) {
	r := newRig(t, builds.Options{})
	ctx := context.Background()
	folder := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.0.0", Channel: "live"})
	b, err := r.m.Install(ctx, folder, "")
	if err != nil {
		t.Fatal(err)
	}
	wantExe := testkit.LinuxExe
	if runtime.GOOS == "windows" {
		wantExe = testkit.WindowsExe
	}
	if b.Version != "1.0.0" || b.Channel != "live" || b.Commit != "fake-1.0.0" || b.Executable != wantExe ||
		b.Files != 3 || b.SizeBytes <= 0 || b.Installed.IsZero() {
		t.Fatalf("build %+v", b)
	}
	exe, err := r.m.ExecutablePath("1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("installed executable is writable: %v", fi.Mode())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed executable is not executable: %v", fi.Mode())
	}
	if _, err := r.m.Install(ctx, folder, ""); code(err) != "build_exists" {
		t.Fatalf("second install: %v", err)
	}

	zipSrc := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.1.0", Channel: "test"})
	zipPath := filepath.Join(r.src, "b.zip")
	testkit.MakeZip(t, zipSrc, zipPath)
	if _, err := r.m.Install(ctx, zipPath, ""); err != nil {
		t.Fatal(err)
	}
	tgzSrc := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "1.2.0"})
	tgzPath := filepath.Join(r.src, "b.tar.gz")
	testkit.MakeTarGz(t, tgzSrc, tgzPath)
	sum, _ := testkit.FileSHA256(tgzPath)
	if _, err := r.m.Install(ctx, tgzPath, strings.Repeat("0", 64)); code(err) != "hash_mismatch" {
		t.Fatalf("wrong archive sha256: %v", err)
	}
	if _, err := r.m.Install(ctx, tgzPath, sum); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Install(ctx, folder, sum); code(err) != "invalid" {
		t.Fatalf("sha256 with a folder: %v", err)
	}
	list, err := r.m.List()
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, b := range list {
		versions = append(versions, b.Version)
	}
	slices.Sort(versions)
	if !slices.Equal(versions, []string{"1.0.0", "1.1.0", "1.2.0"}) {
		t.Fatalf("list %v", versions)
	}
	removed, err := r.m.Prune(func(v string) bool { return v == "1.1.0" })
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"1.0.0", "1.2.0"}) {
		t.Fatalf("pruned %v", removed)
	}
	if _, err := r.m.Get("1.0.0"); !fault.Is(err, fault.NotFound) {
		t.Fatalf("pruned build still there: %v", err)
	}
	if err := r.m.Remove("1.1.0"); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.m.List(); len(list) != 0 {
		t.Fatalf("after remove: %v", list)
	}
	r.noLeftovers(t)
	evs, _ := r.bus.Since(0, events.Filter{Types: []string{events.TypeBuild}})
	if len(evs) != 6 {
		t.Fatalf("want 6 build events (3 installed, 3 removed), got %d", len(evs))
	}
}

func TestInstallTopFolderAccepted(t *testing.T) {
	r := newRig(t, builds.Options{})
	inner := filepath.Join(r.src, "wrap")
	testkit.MakeBuild(t, inner, testkit.BuildOptions{Version: "2.0"})
	zipPath := filepath.Join(r.src, "wrapped.zip")
	testkit.MakeZip(t, inner, zipPath) // entries start with "2.0/"
	if b, err := r.m.Install(context.Background(), zipPath, ""); err != nil || b.Version != "2.0" {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestManifestProblemsRefused(t *testing.T) {
	r := newRig(t, builds.Options{})
	ctx := context.Background()

	tampered := testkit.MakeBuild(t, filepath.Join(r.src, "a"), testkit.BuildOptions{Version: "3.0",
		Extra: map[string][]byte{"Content/data.pak": []byte("original")}})
	if err := os.WriteFile(filepath.Join(tampered, "Content", "data.pak"), []byte("changed!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Install(ctx, tampered, ""); code(err) != "hash_mismatch" {
		t.Fatalf("tampered file: %v", err)
	}

	extra := testkit.MakeBuild(t, filepath.Join(r.src, "b"), testkit.BuildOptions{Version: "3.1"})
	if err := os.WriteFile(filepath.Join(extra, "unlisted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Install(ctx, extra, ""); code(err) != "bad_manifest" {
		t.Fatalf("unlisted file: %v", err)
	}

	missing := testkit.MakeBuild(t, filepath.Join(r.src, "c"), testkit.BuildOptions{Version: "3.2",
		Extra: map[string][]byte{"Content/gone.pak": []byte("x")}})
	if err := os.Remove(filepath.Join(missing, "Content", "gone.pak")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Install(ctx, missing, ""); code(err) != "bad_manifest" {
		t.Fatalf("missing file: %v", err)
	}

	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	noExe := testkit.MakeBuild(t, filepath.Join(r.src, "d"), testkit.BuildOptions{Version: "3.3", ThisOSOnly: true})
	m2 := builds.New(r.st, builds.Options{GOOS: other, ImportDir: r.src})
	if _, err := m2.Install(ctx, noExe, ""); code(err) != "no_executable" {
		t.Fatalf("no executable for this OS: %v", err)
	}
	if _, err := r.m.Install(ctx, filepath.Join(r.src, "nope"), ""); code(err) != "invalid" {
		t.Fatalf("missing source: %v", err)
	}
	if list, _ := r.m.List(); len(list) != 0 {
		t.Fatalf("refused builds were installed: %v", list)
	}
	r.noLeftovers(t)
}

func TestUnsafeArchivesRefused(t *testing.T) {
	r := newRig(t, builds.Options{Limits: builds.Limits{MaxFiles: 50, MaxBytes: 1 << 20}})
	ctx := context.Background()
	body := []byte("x")
	cases := []struct {
		name    string
		entries []testkit.Entry
		tarOnly bool
		want    string
	}{
		{"parent escape", []testkit.Entry{{Name: "../evil.txt", Body: body}}, false, "unsafe_archive"},
		{"nested escape", []testkit.Entry{{Name: "a/../../evil.txt", Body: body}}, false, "unsafe_archive"},
		{"absolute", []testkit.Entry{{Name: "/etc/evil.txt", Body: body}}, false, "unsafe_archive"},
		{"drive letter", []testkit.Entry{{Name: "C:/evil.txt", Body: body}}, false, "unsafe_archive"},
		{"backslash", []testkit.Entry{{Name: `..\evil.txt`, Body: body}}, false, "unsafe_archive"},
		{"symlink", []testkit.Entry{{Name: "link", Symlink: "/etc/passwd"}}, false, "unsafe_archive"},
		{"hard link", []testkit.Entry{{Name: "a.txt", Body: body}, {Name: "b.txt", Hardlink: "a.txt"}}, true, "unsafe_archive"},
		{"device", []testkit.Entry{{Name: "null", Device: true}}, true, "unsafe_archive"},
		{"duplicate", []testkit.Entry{{Name: "a.txt", Body: body}, {Name: "a.txt", Body: body}}, false, "unsafe_archive"},
		{"case duplicate", []testkit.Entry{{Name: "a.txt", Body: body}, {Name: "A.TXT", Body: body}}, false, "unsafe_archive"},
		{"oversized", []testkit.Entry{{Name: "big.bin", Body: make([]byte, 2<<20)}}, false, "too_large"},
	}
	many := make([]testkit.Entry, 60)
	for i := range many {
		many[i] = testkit.Entry{Name: "f" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Body: body}
	}
	cases = append(cases, struct {
		name    string
		entries []testkit.Entry
		tarOnly bool
		want    string
	}{"too many files", many, false, "too_large"})
	for i, c := range cases {
		for _, kind := range []string{"zip", "tar.gz"} {
			if kind == "zip" && c.tarOnly {
				continue
			}
			p := filepath.Join(r.src, "case"+string(rune('a'+i))+"."+kind)
			if kind == "zip" {
				testkit.WriteZip(t, p, c.entries)
			} else {
				testkit.WriteTarGz(t, p, c.entries)
			}
			_, err := r.m.Install(ctx, p, "")
			if code(err) != c.want {
				t.Errorf("%s (%s): got %v (code %s), want %s", c.name, kind, err, code(err), c.want)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.st.Path(), "..", "evil.txt")); err == nil {
		t.Fatal("a file escaped the root")
	}
	r.noLeftovers(t)
}

func TestInstallFromURL(t *testing.T) {
	r := newRig(t, builds.Options{})
	folder := testkit.MakeBuild(t, r.src, testkit.BuildOptions{Version: "4.0"})
	tgz := filepath.Join(r.src, "b.tgz")
	testkit.MakeTarGz(t, folder, tgz)
	data, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/builds/b.tgz" {
			http.NotFound(w, req)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	m := builds.New(r.st, builds.Options{HTTPClient: srv.Client(), ImportDir: r.src})
	ctx := context.Background()
	url := srv.URL + "/builds/b.tgz?sig=secret-value"
	if _, err := m.Install(ctx, url, ""); code(err) != "invalid" {
		t.Fatalf("URL without sha256: %v", err)
	}
	if _, err := m.Install(ctx, strings.Replace(url, "https://", "http://", 1), want); code(err) != "invalid" {
		t.Fatalf("plain http: %v", err)
	}
	if _, err := m.Install(ctx, url, strings.Repeat("a", 64)); code(err) != "hash_mismatch" {
		t.Fatalf("wrong sha256: %v", err)
	}
	if _, err := m.Install(ctx, srv.URL+"/builds/missing.tgz", want); code(err) != "download_failed" {
		t.Fatalf("404: %v", err)
	}
	b, err := m.Install(ctx, url, strings.ToUpper(want))
	if err != nil || b.Version != "4.0" {
		t.Fatalf("%+v %v", b, err)
	}
	if got := builds.RedactSource(url); strings.Contains(got, "secret") {
		t.Fatalf("redacted source still has the query: %s", got)
	}
	r.noLeftovers(t)
}

func TestCleanTemp(t *testing.T) {
	r := newRig(t, builds.Options{})
	if err := r.st.WriteFile("tmp/build-abc/tree/x", []byte("x"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteFile("tmp/other", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.m.CleanTemp(); err != nil {
		t.Fatal(err)
	}
	if r.st.Exists("tmp/build-abc") || !r.st.Exists("tmp/other") {
		t.Fatal("CleanTemp removed the wrong things")
	}
}

// A local source must lie inside the import folder; a relative one is
// taken from there. Anything outside is refused with one answer, whether
// it exists or not, so the answer tells nothing about other folders.
func TestLocalSourcesOnlyFromTheImportFolder(t *testing.T) {
	r := newRig(t, builds.Options{})
	ctx := context.Background()
	outside := t.TempDir()
	folder := testkit.MakeBuild(t, outside, testkit.BuildOptions{Version: "1.0"})
	zipPath := filepath.Join(outside, "b.zip")
	testkit.MakeZip(t, folder, zipPath)
	refused := map[string]string{
		"a folder outside":         folder,
		"an archive outside":       zipPath,
		"a path that is not there": filepath.Join(outside, "nothing-here"),
		"a way out":                filepath.Join("..", filepath.Base(outside)),
		"the parent":               filepath.Dir(r.src),
	}
	var first string
	for what, src := range refused {
		_, err := r.m.Install(ctx, src, "")
		if code(err) != "source_not_allowed" {
			t.Errorf("%s: %v (code %s), want source_not_allowed", what, err, code(err))
			continue
		}
		// The answer may name the import folder, and nothing else: with
		// that taken out, no part of the outside path is left.
		if rest := strings.ReplaceAll(err.Error(), r.src, "<import folder>"); strings.Contains(rest, outside) ||
			strings.Contains(rest, string(filepath.Separator)+filepath.Base(outside)) || strings.Contains(rest, "nothing-here") {
			t.Errorf("%s: the answer names the path: %v", what, err)
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("%s: answer %q differs from %q", what, err, first)
		}
	}
	// A link inside the import folder that leads out is refused too.
	link := filepath.Join(r.src, "link")
	if err := os.Symlink(folder, link); err != nil {
		t.Logf("no symbolic links here: %v", err)
	} else if _, err := r.m.Install(ctx, link, ""); code(err) != "source_not_allowed" {
		t.Errorf("a link out of the import folder: %v (code %s)", err, code(err))
	}
	if list, _ := r.m.List(); len(list) != 0 {
		t.Fatalf("builds from outside were installed: %v", list)
	}
	// Inside, by its full path or relative to the import folder.
	inside := testkit.MakeBuild(t, filepath.Join(r.src, "a"), testkit.BuildOptions{Version: "2.0"})
	if _, err := r.m.Install(ctx, inside, ""); err != nil {
		t.Fatalf("full path inside: %v", err)
	}
	testkit.MakeZip(t, testkit.MakeBuild(t, filepath.Join(r.src, "b"), testkit.BuildOptions{Version: "3.0"}), filepath.Join(r.src, "b.zip"))
	if _, err := r.m.Install(ctx, "b.zip", ""); err != nil {
		t.Fatalf("relative path inside: %v", err)
	}
	r.noLeftovers(t)
}

// A failed download says no more than the class of the server's answer.
func TestDownloadErrorsSayLittle(t *testing.T) {
	r := newRig(t, builds.Options{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "internal detail 1234", http.StatusTeapot)
	}))
	defer srv.Close()
	m := builds.New(r.st, builds.Options{HTTPClient: srv.Client(), ImportDir: r.src})
	_, err := m.Install(context.Background(), srv.URL+"/b.zip", strings.Repeat("a", 64))
	if code(err) != "download_failed" || strings.Contains(err.Error(), "418") || strings.Contains(err.Error(), "Teapot") || !strings.Contains(err.Error(), "4xx") {
		t.Fatalf("got %v", err)
	}
}
