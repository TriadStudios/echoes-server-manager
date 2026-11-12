// Package testkit holds helpers shared by ESM's tests: it builds the fake
// game server once per test process, makes build folders and archives with
// correct manifests, and waits for conditions.
//
// Packages that use FakeServer should clean up with:
//
//	func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }
package testkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
)

// FakeServerEnv names a prebuilt fake server binary to use instead of
// building one (for running cross-compiled test binaries on a machine
// without Go).
const FakeServerEnv = "ESM_TEST_FAKESERVER"

// FakePackage is the import path of the fake server command.
const FakePackage = "github.com/TriadStudios/echoes-server-manager/cmd/esm-fakeserver"

// Test-only request files understood by the fake in <data>/control/.
const (
	ControlStop  = "stop"
	ControlCrash = "crash"
	ControlHang  = "hang"
	// ControlUnready keeps the fake running but stops it reporting ready.
	ControlUnready = "unready"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
	fakeTemp string
)

// ExeSuffix is ".exe" on Windows and "" elsewhere.
func ExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// FakeServer returns the path of the fake server binary for this OS,
// building it on first use.
func FakeServer(t testing.TB) string {
	t.Helper()
	fakeOnce.Do(buildFake)
	if fakeErr != nil {
		t.Fatalf("fake server: %v", fakeErr)
	}
	return fakePath
}

func buildFake() {
	if p := os.Getenv(FakeServerEnv); p != "" {
		abs, err := filepath.Abs(p)
		if err == nil {
			_, err = os.Stat(abs)
		}
		fakePath, fakeErr = abs, err
		return
	}
	dir, err := os.MkdirTemp("", "esm-fake-")
	if err != nil {
		fakeErr = err
		return
	}
	fakeTemp = dir
	out := filepath.Join(dir, "esm-fakeserver"+ExeSuffix())
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, FakePackage)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		fakeErr = fmt.Errorf("go build %s: %v\n%s", FakePackage, err, b)
		return
	}
	fakePath = out
}

// Run runs the tests and removes the fake server built for them.
func Run(m *testing.M) int {
	code := m.Run()
	if fakeTemp != "" {
		os.RemoveAll(fakeTemp)
	}
	return code
}

// Executable paths inside a build made by MakeBuild.
const (
	LinuxExe   = "linux/esm-fakeserver"
	WindowsExe = "windows/esm-fakeserver.exe"
)

// BuildOptions describe a build folder.
type BuildOptions struct {
	Version string            // required
	Commit  string            // default "fake-<version>"
	Channel string            // default "live"
	Fake    map[string]any    // when set, written as fake.json next to each executable
	Extra   map[string][]byte // more files, by slash path
	// ThisOSOnly lists only this OS's executable in the manifest. Otherwise
	// both linux and windows are listed and the other one is a placeholder.
	ThisOSOnly bool
}

// MakeBuild creates <parent>/<version>/ holding the fake server for this OS,
// a placeholder for the other OS, optional fake.json files, extra files and
// a manifest.json with the sha256 of every other file. It returns the folder.
func MakeBuild(t testing.TB, parent string, o BuildOptions) string {
	t.Helper()
	if o.Version == "" {
		t.Fatal("MakeBuild: Version is required")
	}
	if o.Commit == "" {
		o.Commit = "fake-" + o.Version
	}
	if o.Channel == "" {
		o.Channel = "live"
	}
	dir := filepath.Join(parent, o.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := FakeServer(t)
	m := builds.Manifest{Version: o.Version, Commit: o.Commit, Channel: o.Channel,
		Executables: map[string]string{}, Files: map[string]string{}}
	for goos, rel := range map[string]string{"linux": LinuxExe, "windows": WindowsExe} {
		if goos != runtime.GOOS && o.ThisOSOnly {
			continue
		}
		m.Executables[goos] = rel
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if goos == runtime.GOOS {
			copyFile(t, fake, dst, 0o755)
		} else {
			writeFile(t, dst, []byte("placeholder: the "+goos+" server is not built in tests\n"), 0o755)
		}
		if o.Fake != nil {
			b, err := json.Marshal(o.Fake)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(filepath.Dir(dst), "fake.json"), b, 0o644)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(o.Extra)) {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), o.Extra[rel], 0o644)
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "manifest.json" {
			return nil
		}
		sum, err := FileSHA256(p)
		if err != nil {
			return err
		}
		m.Files[rel] = sum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	writeFile(t, filepath.Join(dir, "manifest.json"), b, 0o644)
	return dir
}

// FileSHA256 returns the hex sha256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFile(t testing.TB, path string, b []byte, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t testing.TB, src, dst string, mode fs.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, b, mode)
}

// WriteControl drops a request file (ControlStop, ControlCrash or
// ControlHang, ControlUnready) into <dataDir>/control/.
func WriteControl(dataDir, name string) error {
	dir := filepath.Join(dataDir, "control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600)
}

// Eventually polls cond every interval until it is true or timeout passes,
// then fails the test with msg.
func Eventually(t testing.TB, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: "+msg, append([]any{timeout}, args...)...)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
