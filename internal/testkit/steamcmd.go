package testkit

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// FakeSteamCMDEnv names a prebuilt fake SteamCMD binary to use instead of
// building one (see FakeServerEnv).
const FakeSteamCMDEnv = "ESM_TEST_FAKESTEAMCMD"

// FakeSteamCMDPackage is the import path of the fake SteamCMD command.
const FakeSteamCMDPackage = "github.com/TriadStudios/echoes-server-manager/cmd/esm-fakesteamcmd"

var (
	steamOnce sync.Once
	steamPath string
	steamErr  error
	steamTemp string
)

func buildFakeSteam() {
	if p := os.Getenv(FakeSteamCMDEnv); p != "" {
		abs, err := filepath.Abs(p)
		if err == nil {
			_, err = os.Stat(abs)
		}
		steamPath, steamErr = abs, err
		return
	}
	dir, err := os.MkdirTemp("", "esm-fakesteam-")
	if err != nil {
		steamErr = err
		return
	}
	steamTemp = dir
	out := filepath.Join(dir, "esm-fakesteamcmd"+ExeSuffix())
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", out, FakeSteamCMDPackage)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		steamErr = fmt.Errorf("go build %s: %v\n%s", FakeSteamCMDPackage, err, b)
		return
	}
	steamPath = out
}

// SteamControl is fakesteamcmd.json: what the fake SteamCMD serves.
type SteamControl struct {
	AppID    string `json:"app_id"`
	BuildID  string `json:"buildid"`
	Content  string `json:"content"`
	Branch   string `json:"branch"`
	Password string `json:"password"`
	Record   string `json:"record"`
}

// FakeSteamCMD copies the fake SteamCMD into dir (its own copy, so its
// control file is the test's own) and writes the control file. It returns
// the binary's path, for the steamcmd setting.
func FakeSteamCMD(t testing.TB, dir string, c SteamControl) string {
	t.Helper()
	steamOnce.Do(buildFakeSteam)
	if steamErr != nil {
		t.Fatalf("fake SteamCMD: %v", steamErr)
	}
	exe := filepath.Join(dir, "steamcmd"+ExeSuffix())
	copyFile(t, steamPath, exe, 0o755)
	SetSteamControl(t, dir, c)
	return exe
}

// SetSteamControl rewrites the control file of the fake SteamCMD in dir.
func SetSteamControl(t testing.TB, dir string, c SteamControl) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "fakesteamcmd.json"), b, 0o644)
}

// SteamRecord is one run of the fake SteamCMD as it recorded it.
type SteamRecord struct {
	Args    []string `json:"args"`
	Home    string   `json:"home"`
	Scripts []string `json:"scripts"`
}

// SteamRecords reads the record file of the fake SteamCMD, and whether it
// ever saw the branch password on its command line.
func SteamRecords(t testing.TB, record string) (runs []SteamRecord, leaked bool) {
	t.Helper()
	if _, err := os.Stat(record + ".leak"); err == nil {
		leaked = true
	}
	b, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil, leaked
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r SteamRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		runs = append(runs, r)
	}
	return runs, leaked
}
