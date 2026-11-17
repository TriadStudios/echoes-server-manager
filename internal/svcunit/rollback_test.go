package svcunit

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/release"
)

// These tests run the real script with /bin/sh, as the unit does; in the
// test image that is a plain POSIX shell (busybox, or dash in another one).

// rig is a binary with its update files in a folder whose name has a space,
// a $ and a * in it: the script must quote every path.
type rig struct {
	t      *testing.T
	dir    string
	exe    string
	script string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs under /bin/sh")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := filepath.Join(t.TempDir(), "esm dir $HOME *")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, dir: dir, exe: filepath.Join(dir, "esm")}
	// What an update leaves: the old binary kept as a second name of the
	// same file, and the new one moved over the first name.
	os.WriteFile(r.exe, []byte("old binary"), 0o755)
	if err := os.Link(r.exe, r.exe+".previous"); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".esm-update-1")
	os.WriteFile(tmp, []byte("new binary"), 0o755)
	if err := os.Rename(tmp, r.exe); err != nil {
		t.Fatal(err)
	}
	r.script = filepath.Join(dir, "esm.update.sh")
	os.WriteFile(r.script, Script(), 0o644)
	return r
}

// pending marks an update as the self-update does.
func (r *rig) pending() {
	r.t.Helper()
	if err := release.MarkPending(r.exe, release.Marker{From: "v1.0.0", To: "v1.1.0", Time: time.Now().UTC()}); err != nil {
		r.t.Fatal(err)
	}
}

// start runs the script as one start of the unit does, and returns what it
// printed. The script must end with status 0 whatever happens.
func (r *rig) start() string {
	r.t.Helper()
	cmd := exec.Command("/bin/sh", r.script, r.exe)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("the script failed: %v\n%s", err, out.String())
	}
	return out.String()
}

func (r *rig) read(suffix string) string {
	b, err := os.ReadFile(r.exe + suffix)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (r *rig) has(suffix string) bool {
	_, err := os.Lstat(r.exe + suffix)
	return err == nil
}

func (r *rig) starts() int {
	return strings.Count(r.read(release.StartsSuffix), "\n")
}

// A start with no update waiting is nothing: no output, no file made, the
// binary and its .previous untouched, however many times.
func TestRollbackScriptWithoutAPendingUpdate(t *testing.T) {
	r := newRig(t)
	for range 5 {
		if out := r.start(); out != "" {
			t.Fatalf("a normal start printed %q", out)
		}
	}
	if r.read("") != "new binary" || r.read(".previous") != "old binary" {
		t.Fatalf("binaries: %q, %q", r.read(""), r.read(".previous"))
	}
	for _, suffix := range []string{release.StartsSuffix, release.RolledBackSuffix, release.PendingSuffix, release.FailedSuffix} {
		if r.has(suffix) {
			t.Errorf("a normal start made %s", suffix)
		}
	}
	// Called with no argument it does nothing either.
	if err := exec.Command("/bin/sh", r.script).Run(); err != nil {
		t.Fatalf("no argument: %v", err)
	}
}

// The first and second start of an unconfirmed update are counted and change
// nothing.
func TestRollbackScriptCountsTheFirstStarts(t *testing.T) {
	r := newRig(t)
	r.pending()
	for n := 1; n < release.RollbackStarts; n++ {
		out := r.start()
		if r.starts() != n || !strings.Contains(out, "this is start "+strconv.Itoa(n)) || !strings.Contains(out, `"to":"v1.1.0"`) {
			t.Fatalf("start %d: %d start lines, output %q", n, r.starts(), out)
		}
		if r.read("") != "new binary" || r.read(".previous") != "old binary" || !r.has(release.PendingSuffix) {
			t.Fatalf("start %d changed something: binary %q, previous %q, pending %v", n, r.read(""), r.read(".previous"), r.has(release.PendingSuffix))
		}
		if r.has(release.RolledBackSuffix) || r.has(release.FailedSuffix) {
			t.Fatalf("start %d rolled back", n)
		}
	}
	// Each start line is a time.
	for _, line := range strings.Fields(r.read(release.StartsSuffix)) {
		if _, err := time.Parse(time.RFC3339, line); err != nil {
			t.Errorf("start line %q: %v", line, err)
		}
	}
}

// The 3rd start puts the old binary back, keeps the new one as .failed,
// leaves the record for ESM and removes the pending and start files; the start
// after that is a normal one.
func TestRollbackScriptPutsTheOldBinaryBackAtTheThirdStart(t *testing.T) {
	r := newRig(t)
	os.WriteFile(r.exe+release.FailedSuffix, []byte("an older failure"), 0o644)
	r.pending()
	r.start()
	r.start()
	before := time.Now().Add(-2 * time.Second)
	out := r.start()
	if !strings.Contains(out, "putting "+r.exe+".previous back") || !strings.Contains(out, "the old binary is back") {
		t.Fatalf("output %q", out)
	}
	if r.read("") != "old binary" || r.read(release.FailedSuffix) != "new binary" {
		t.Fatalf("binary %q, failed %q", r.read(""), r.read(release.FailedSuffix))
	}
	for _, gone := range []string{".previous", release.PendingSuffix, release.StartsSuffix, release.RolledBackSuffix + ".tmp"} {
		if r.has(gone) {
			t.Errorf("%s is still there", gone)
		}
	}
	if fi, err := os.Stat(r.exe); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the restored binary is not executable: %v, %v", fi, err)
	}
	// The record, as ESM reads it.
	rb, ok, err := release.ReadRolledBack(r.exe)
	if !ok || err != nil || rb.From != "v1.0.0" || rb.To != "v1.1.0" || rb.Time.IsZero() {
		t.Fatalf("record: %+v, %v, %v\n%s", rb, ok, err, r.read(release.RolledBackSuffix))
	}
	if rb.At.Before(before) || rb.At.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("rolled back at %v, started %v", rb.At, before)
	}
	// The start after the rollback is a normal one, and the record stays.
	if out := r.start(); out != "" {
		t.Fatalf("the start after the rollback printed %q", out)
	}
	if r.read("") != "old binary" || !r.has(release.RolledBackSuffix) {
		t.Fatalf("after the rollback: binary %q, record %v", r.read(""), r.has(release.RolledBackSuffix))
	}
}

// Without the old binary there is nothing to put back: the script says so,
// leaves the new binary, stops counting, and still ends with status 0.
func TestRollbackScriptWithoutThePreviousBinary(t *testing.T) {
	r := newRig(t)
	os.Remove(r.exe + ".previous")
	r.pending()
	r.start()
	r.start()
	out := r.start()
	if !strings.Contains(out, "is missing") || !strings.Contains(out, "the new binary stays") {
		t.Fatalf("output %q", out)
	}
	if r.read("") != "new binary" {
		t.Fatalf("binary %q", r.read(""))
	}
	for _, suffix := range []string{release.PendingSuffix, release.StartsSuffix, release.RolledBackSuffix, release.FailedSuffix} {
		if r.has(suffix) {
			t.Errorf("%s is there", suffix)
		}
	}
	if out := r.start(); out != "" {
		t.Fatalf("a later start printed %q", out)
	}
}

// A folder the script cannot write to: it says so, changes nothing and still
// lets ESM start.
func TestRollbackScriptInAFolderItCannotWriteTo(t *testing.T) {
	r := newRig(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	r.pending()
	if err := os.Chmod(r.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(r.dir, 0o755) })
	for range 4 {
		out := r.start()
		if !strings.Contains(out, "cannot write") {
			t.Fatalf("output %q", out)
		}
	}
	if r.read("") != "new binary" || r.read(".previous") != "old binary" {
		t.Fatalf("binaries: %q, %q", r.read(""), r.read(".previous"))
	}
}

// The script is plain POSIX sh: it parses, and uses nothing of bash.
func TestRollbackScriptIsPlainSh(t *testing.T) {
	s := string(Script())
	if !strings.HasPrefix(s, "#!/bin/sh\n") || strings.Contains(s, "\r") {
		t.Fatal("the script does not start with #!/bin/sh, or has carriage returns")
	}
	// sed's character class is no "[[".
	s = strings.ReplaceAll(s, "[[:space:]]", "")
	for _, bad := range []string{"[[", "]]", "function ", "local ", "echo -e", "source ", "$'", "<<<", "<(", ">(", "${#", "${!", "declare ", "set -o pipefail", "==", "=~", "&>", "|&"} {
		if strings.Contains(s, bad) {
			t.Errorf("the script has %q, which is not plain sh", bad)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	if out, err := exec.Command("/bin/sh", "-n", filepath.Join("rollback.sh")).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

// The script and the Go side agree on the names of the files and on the
// number of starts.
func TestRollbackScriptMatchesTheGoSide(t *testing.T) {
	s := string(Script())
	if m := regexp.MustCompile(`(?m)^starts_allowed=(\d+)`).FindStringSubmatch(s); m == nil || m[1] != strconv.Itoa(release.RollbackStarts) {
		t.Fatalf("starts_allowed in the script %v, release.RollbackStarts %d", m, release.RollbackStarts)
	}
	for name, want := range map[string]string{
		"pending=$exe":    release.PendingSuffix,
		"starts=$exe":     release.StartsSuffix,
		"previous=$exe":   release.PreviousSuffix,
		"failed=$exe":     release.FailedSuffix,
		"rolledback=$exe": release.RolledBackSuffix,
	} {
		if !strings.Contains(s, name+want+"\n") {
			t.Errorf("the script lacks %s%s", name, want)
		}
	}
	if ScriptSuffix != ".update.sh" {
		t.Errorf("script suffix %q", ScriptSuffix)
	}
}
