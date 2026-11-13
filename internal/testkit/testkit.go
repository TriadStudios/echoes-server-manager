// Package testkit holds helpers shared by ESM's tests: it builds the fake
// game server once per test process, makes build folders and archives with
// correct manifests, and waits for conditions.
//
// Packages that use FakeServer, or that make ESM bind ports, should end
// with:
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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/netbind"
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
	// ControlReady makes the fake report ready now.
	ControlReady = "ready"
	// ControlUnready keeps the fake running but stops it reporting ready.
	ControlUnready = "unready"
	// ControlCommandLine makes the fake print its command line again.
	ControlCommandLine = "cmdline"
	// ControlSecondSignal is the file the fake writes (and never takes)
	// when it gets a second stop signal: a test that finds it failed to
	// stop with exactly one signal.
	ControlSecondSignal = "second-signal"
)

// SecondSignalExitCode is the fake's exit code after a second stop signal.
const SecondSignalExitCode = 98

// SecondSignal reports whether the fake with this data folder ever got a
// second stop signal.
func SecondSignal(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, "control", ControlSecondSignal))
	return err == nil
}

// FakeSavePath is the save file of the fake with this data folder for a map
// and world.
func FakeSavePath(dataDir, mapName, world string) string {
	return filepath.Join(dataDir, "Saved", "SaveGames", "Fake", mapName+"__"+world+".json")
}

// FakeSaveCounter reads the save counter of the fake's save file (0 when
// there is none).
func FakeSaveCounter(dataDir, mapName, world string) int {
	b, err := os.ReadFile(FakeSavePath(dataDir, mapName, world))
	if err != nil {
		return 0
	}
	var f struct {
		Counter int `json:"counter"`
	}
	json.Unmarshal(b, &f)
	return f.Counter
}

// WriteFakeSave writes a save file with counter for the fake.
func WriteFakeSave(dataDir, mapName, world string, counter int) error {
	p := FakeSavePath(dataDir, mapName, world)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"world": world, "counter": counter})
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

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

// Run runs the tests and removes the fake server built for them. The run
// fails when ESM was asked to bind an address that is not loopback: in a
// test binary netbind refuses those (see netbind.Refused), and Run names
// them.
func Run(m *testing.M) int {
	code := m.Run()
	if fakeTemp != "" {
		os.RemoveAll(fakeTemp)
	}
	if r := netbind.Refused(); len(r) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: tests asked ESM to bind addresses that are not loopback (refused, tests must use 127.0.0.1): %s\n",
			strings.Join(r, ", "))
		code = 1
	}
	return code
}

// DefaultMap is the default map of builds made by MakeBuild.
const DefaultMap = "TestMap"

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
	// DefaultMap is the manifest's default map: DefaultMap when empty,
	// none when "-".
	DefaultMap string
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
	switch o.DefaultMap {
	case "":
		o.DefaultMap = DefaultMap
	case "-":
		o.DefaultMap = ""
	}
	m := builds.Manifest{Version: o.Version, Commit: o.Commit, Channel: o.Channel, DefaultMap: o.DefaultMap,
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

// WriteControl drops a request file (ControlStop, ControlCrash,
// ControlHang, ControlReady, ControlUnready or ControlCommandLine) into
// <dataDir>/control/.
func WriteControl(dataDir, name string) error {
	dir := filepath.Join(dataDir, "control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600)
}

// Questions a fake server answers on its query port (only for tests).
const (
	// JoinPassword asks about the server password.
	JoinPassword = "JOIN"
	// AdminPassword asks about the admin password.
	AdminPassword = "ADMIN"
)

// CheckPassword asks the fake server behind the query port whether password
// is the one it was started with: its server password (kind JoinPassword)
// or its admin password (kind AdminPassword). An empty password is right
// when the server has none. This shows what a server really got, however
// it got it.
func CheckPassword(host string, queryPort int, kind, password string) (bool, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	var d net.Dialer
	if la := gameserver.LoopbackLocalAddr(host); la != nil {
		d.LocalAddr = la
	}
	var lastErr error
	for range 3 {
		c, err := d.Dial("udp", net.JoinHostPort(host, strconv.Itoa(queryPort)))
		if err != nil {
			return false, err
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		var n int
		if _, err = c.Write([]byte(kind + " " + password)); err == nil {
			n, err = c.Read(buf)
		}
		c.Close()
		if err != nil {
			lastErr = err
			continue
		}
		switch string(buf[:n]) {
		case "OK":
			return true, nil
		case "DENIED":
			return false, nil
		}
		return false, fmt.Errorf("testkit: unexpected answer %q", buf[:n])
	}
	return false, lastErr
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
