package steam

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

const sampleACF = `"AppState"
{
	"appid"		"1000001"
	"Universe"		"1"
	"name"		"Some \"Dedicated\" Server"
	"StateFlags"		"4"
	"installdir"		"Some Server"
	"LastUpdated"		"1700000000"
	"SizeOnDisk"		"123456"
	"buildid"		"20021"
	// a comment
	"InstalledDepots"
	{
		"1000002"
		{
			"manifest"		"1234567890123456789"
			"size"		"123456"
		}
	}
	"UserConfig"
	{
		"BetaKey"		"testing"
	}
}
`

func TestParseAppManifest(t *testing.T) {
	am, err := ParseAppManifest([]byte(sampleACF))
	if err != nil {
		t.Fatal(err)
	}
	if am.AppID != "1000001" || am.BuildID != "20021" || !am.FullyInstalled() {
		t.Fatalf("%+v", am)
	}
	bad := map[string]string{
		"no AppState":       `"Other" { "appid" "1" "buildid" "2" "StateFlags" "4" }`,
		"two blocks":        sampleACF + sampleACF,
		"unquoted":          `"AppState" { appid "1" }`,
		"unclosed block":    `"AppState" { "appid" "1"`,
		"stray brace":       `"AppState" { } }`,
		"unclosed string":   `"AppState" { "appid "1" }`,
		"buildid twice":     strings.Replace(sampleACF, `"buildid"		"20021"`, `"buildid" "1" "buildid" "2"`, 1),
		"buildid not digit": strings.Replace(sampleACF, `"20021"`, `"20021x"`, 1),
		"buildid a block":   strings.Replace(sampleACF, `"buildid"		"20021"`, `"buildid" { }`, 1),
		"no buildid":        strings.Replace(sampleACF, `"buildid"		"20021"`, ``, 1),
		"buildid zero":      strings.Replace(sampleACF, `"20021"`, `"000"`, 1),
		"bad escape":        strings.Replace(sampleACF, `\"Dedicated\"`, `\q`, 1),
		"too deep":          `"AppState" ` + strings.Repeat(`{ "a" `, 40) + strings.Repeat("}", 40),
		"too long string":   `"AppState" { "x" "` + strings.Repeat("a", 5000) + `" }`,
		"too big":           `"AppState" { "x" "` + strings.Repeat("a", MaxACFBytes) + `" }`,
	}
	for name, text := range bad {
		if _, err := ParseAppManifest([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	partial := strings.Replace(sampleACF, `"StateFlags"		"4"`, `"StateFlags"		"1026"`, 1)
	if am, err := ParseAppManifest([]byte(partial)); err != nil || am.FullyInstalled() {
		t.Fatalf("partial: %+v %v", am, err)
	}
}

func FuzzParseAppManifest(f *testing.F) {
	f.Add([]byte(sampleACF))
	f.Add([]byte(`"AppState" { "appid" "1" "buildid" "2" "StateFlags" "4" }`))
	f.Add([]byte(`"a" { "b" { "c" "\\\"" } } // x`))
	f.Fuzz(func(t *testing.T, b []byte) {
		am, err := ParseAppManifest(b)
		if err != nil {
			return
		}
		if !digitsRe.MatchString(am.BuildID) || am.AppID == "" {
			t.Fatalf("accepted %+v", am)
		}
	})
}

func TestScript(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app dir")
	b, err := Script(Request{InstallDir: dir, AppID: "1000001", Branch: "testing", Password: "s3cret-Pw", Validate: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "@ShutdownOnFailedCommand 1\n@NoPromptForPassword 1\nforce_install_dir \"" + dir + "\"\nlogin anonymous\n" +
		"app_update 1000001 -beta testing -betapassword s3cret-Pw validate\nquit\n"
	if string(b) != want {
		t.Fatalf("script\n%s\nwant\n%s", b, want)
	}
	b, _ = Script(Request{InstallDir: dir, AppID: "1000001"})
	if !strings.Contains(string(b), "app_update 1000001\n") {
		t.Fatalf("default branch: %s", b)
	}
	for name, r := range map[string]Request{
		"relative dir":      {InstallDir: "app", AppID: "1"},
		"quote in dir":      {InstallDir: filepath.Join(dir, `a"b`), AppID: "1"},
		"newline in dir":    {InstallDir: dir + "\nquit", AppID: "1"},
		"app id":            {InstallDir: dir, AppID: "1 +quit"},
		"branch":            {InstallDir: dir, AppID: "1", Branch: "a b"},
		"password no beta":  {InstallDir: dir, AppID: "1", Password: "x"},
		"password space":    {InstallDir: dir, AppID: "1", Branch: "b", Password: "a b"},
		"password newline":  {InstallDir: dir, AppID: "1", Branch: "b", Password: "a\nquit"},
		"password quote":    {InstallDir: dir, AppID: "1", Branch: "b", Password: `a"b`},
		"password too long": {InstallDir: dir, AppID: "1", Branch: "b", Password: strings.Repeat("a", 129)},
	} {
		if _, err := Script(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSourceOf(t *testing.T) {
	for v, want := range map[string]string{
		"steam-main-123":       "main 123",
		"steam-eu-test-99":     "eu-test 99",
		"steam-main-":          "",
		"steam--1":             "",
		"steam-main-12a":       "",
		"build-1":              "",
		"steam-Main-1":         "",
		"steam-main-x-1-2":     "main-x-1 2",
		"steam-a-123456789012": "a 123456789012",
	} {
		s, id, ok := SourceOf(v)
		got := ""
		if ok {
			got = s + " " + id
		}
		if got != want {
			t.Errorf("SourceOf(%q) = %q, want %q", v, got, want)
		}
	}
}

// rig is a Steam manager on a fresh root with a fake SteamCMD.
type rig struct {
	t       *testing.T
	root    string
	steam   string // folder of the fake SteamCMD
	content string
	record  string
	ctl     testkit.SteamControl
	st      *store.Store
	builds  *builds.Manager
	m       *Manager
	audit   *audit.Log
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, root: t.TempDir(), steam: t.TempDir(), content: filepath.Join(t.TempDir(), "content")}
	r.record = filepath.Join(r.steam, "record.jsonl")
	exes := testkit.MakeContent(t, r.content, testkit.BuildOptions{Version: "x", Extra: map[string][]byte{"Data/world.dat": []byte("data")}})
	r.ctl = testkit.SteamControl{AppID: "1000001", BuildID: "100", Content: r.content, Branch: "testing", Password: "right-Pw1", Record: r.record, Echo: true}
	exe := testkit.FakeSteamCMD(t, r.steam, r.ctl)
	st, err := store.Open(r.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.st = st
	every := config.Duration(0)
	cfg := config.Defaults(r.root)
	cfg.SteamCMD = exe
	cfg.SteamSources = []config.SteamSource{{Name: "main", AppID: "1000001", Branch: "testing", Executable: exes,
		DefaultMap: "TestMap", Channel: "test", CheckEvery: &every}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r.audit, err = audit.Open(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.audit.Close() })
	r.builds = builds.New(st, builds.Options{SteamAppID: "1000009"})
	r.m = New(Options{Store: st, Config: cfg, Builds: r.builds, Audit: r.audit})
	t.Cleanup(r.m.Close)
	return r
}

func (r *rig) noLeak() {
	r.t.Helper()
	runs, leaked := testkit.SteamRecords(r.t, r.record)
	if leaked {
		r.t.Fatal("the branch password was on SteamCMD's command line")
	}
	for _, run := range runs {
		if strings.Contains(strings.Join(run.Args, " "), r.ctl.Password) {
			r.t.Fatalf("password in argv %v", run.Args)
		}
	}
	// Nothing ESM keeps holds the password: not the cache, the builds, the
	// log, the state, the audit log; only the secrets file.
	filepath.WalkDir(r.root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) == "steam.json" {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), r.ctl.Password) {
			r.t.Errorf("%s holds the branch password", p)
		}
		return nil
	})
	if left, _ := filepath.Glob(filepath.Join(r.root, CacheDir, runPrefix+"*")); len(left) != 0 {
		r.t.Errorf("script folders left: %v", left)
	}
}

func TestSteamInstallFlow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	// No password yet: SteamCMD refuses the branch.
	_, _, err := r.m.Install(ctx, "main", true)
	if fault.CodeOf(err) != "wrong_password" {
		t.Fatalf("no password: %v", err)
	}
	if err := r.m.SetPassword("main", "wrong-Pw1"); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.m.Install(ctx, "main", true)
	if fault.CodeOf(err) != "wrong_password" || strings.Contains(err.Error(), "wrong-Pw1") {
		t.Fatalf("wrong password: %v", err)
	}
	if v := r.m.Sources()[0]; v.Error == "" || !v.PasswordSet {
		t.Fatalf("source after a failure: %+v", v)
	}
	if err := r.m.SetPassword("main", "bad pw"); err == nil {
		t.Fatal("a password with a space was accepted")
	}
	if err := r.m.SetPassword("main", r.ctl.Password); err != nil {
		t.Fatal(err)
	}

	// New build id: installed.
	b, installed, err := r.m.Install(ctx, "main", true)
	if err != nil || !installed || b.Version != "steam-main-100" || b.Channel != "test" || b.DefaultMap != "TestMap" {
		t.Fatalf("first install: %+v %v %v", b, installed, err)
	}
	p, _ := r.builds.Path(b.Version)
	if _, err := os.Stat(filepath.Join(p, "steamapps")); err == nil {
		t.Error("steamapps/ was copied into the build")
	}
	if got, _ := os.ReadFile(filepath.Join(p, "Data", "world.dat")); string(got) != "data" {
		t.Errorf("content not copied: %q", got)
	}
	exeDir := filepath.Dir(filepath.Join(p, filepath.FromSlash(b.Executable)))
	if id, _ := os.ReadFile(filepath.Join(exeDir, "steam_appid.txt")); string(id) != "1000009" {
		t.Errorf("steam_appid.txt = %q", id)
	}
	if v, since, ok := r.m.Target("main"); !ok || v != "steam-main-100" || since.IsZero() {
		t.Fatalf("target %q %v %v", v, since, ok)
	}

	// Same build id: nothing new.
	b2, installed, err := r.m.Install(ctx, "main", false)
	if err != nil || installed || b2.Version != b.Version {
		t.Fatalf("unchanged: %+v %v %v", b2, installed, err)
	}

	// A new build id on the branch.
	r.ctl.BuildID = "101"
	testkit.SetSteamControl(t, r.steam, r.ctl)
	b3, installed, err := r.m.Install(ctx, "main", false)
	if err != nil || !installed || b3.Version != "steam-main-101" {
		t.Fatalf("new build id: %+v %v %v", b3, installed, err)
	}
	if v, _, _ := r.m.Target("main"); v != "steam-main-101" {
		t.Fatalf("target %q", v)
	}
	entries, err := r.audit.Recent(50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "steam.install" && e.Actor.Kind == audit.ActorSystem && strings.Contains(e.Target, "steam-main-101") {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit entry for the automatic install: %+v", entries)
	}

	runs, _ := testkit.SteamRecords(t, r.record)
	if len(runs) != 5 {
		t.Fatalf("%d SteamCMD runs, want 5", len(runs))
	}
	for _, run := range runs {
		if len(run.Args) != 2 || run.Args[0] != "+runscript" {
			t.Errorf("SteamCMD ran with %v", run.Args)
		}
		if !strings.HasPrefix(run.Home, filepath.Join(r.root, CacheDir, "main")) {
			t.Errorf("SteamCMD's HOME %q is outside the cache", run.Home)
		}
		if runtime.GOOS == "linux" && (len(run.Scripts) != 1 || run.Scripts[0] != "600 700") {
			t.Errorf("script and folder modes %v, want 600 700", run.Scripts)
		}
	}
	r.noLeak()
	// The SteamCMD log says what happened, with the password masked.
	if log, _ := os.ReadFile(filepath.Join(r.root, CacheDir, "main.log")); !strings.Contains(string(log), "Success!") ||
		!strings.Contains(string(log), "-betapassword "+gameserver.Mask) {
		t.Errorf("log: %s", log)
	}
}

func TestSteamErrors(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, _, err := r.m.Install(ctx, "nope", true); fault.CodeOf(err) != "no_source" {
		t.Fatalf("unknown source: %v", err)
	}
	if err := r.m.SetPassword("nope", "x1234567"); !fault.Is(err, fault.NotFound) {
		t.Fatalf("password of unknown source: %v", err)
	}
	r.m.cfg.SteamCMD = filepath.Join(r.steam, "missing")
	if _, _, err := r.m.Install(ctx, "main", true); fault.CodeOf(err) != "steamcmd_missing" {
		t.Fatalf("missing SteamCMD: %v", err)
	}
}

// contentSize adds up the sizes of the files in dir.
func contentSize(t *testing.T, dir string) uint64 {
	t.Helper()
	var n uint64
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		n += uint64(fi.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A Steam build is copied into builds/ only when the disk that holds the
// builds has at least twice its size free. With less, the install is
// refused with an error the source's state keeps, and nothing is copied;
// with twice its size free it goes ahead.
func TestSteamInstallNeedsTwiceTheBuildSizeFree(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.SetPassword("main", r.ctl.Password); err != nil {
		t.Fatal(err)
	}
	size := contentSize(t, r.content)
	var free uint64
	var asked []string
	r.m.opts.DiskSpace = func(dir string) (uint64, uint64, error) {
		asked = append(asked, dir)
		return 1 << 40, free, nil
	}

	free = 2*size - 1
	_, _, err := r.m.Install(ctx, "main", false)
	if fault.CodeOf(err) != "not_enough_space" || !strings.Contains(err.Error(), "steam-main-100") ||
		!strings.Contains(err.Error(), "nothing was installed") {
		t.Fatalf("install with too little space: %v", err)
	}
	if v := r.m.Sources()[0]; v.Error != err.Error() || v.Version != "" {
		t.Fatalf("source after the refusal: %+v", v)
	}
	if list, err := r.builds.List(); err != nil || len(list) != 0 {
		t.Fatalf("builds after the refusal: %+v %v", list, err)
	}
	for _, dir := range []string{builds.Dir, "tmp"} {
		entries, _ := os.ReadDir(filepath.Join(r.root, dir))
		for _, e := range entries {
			if e.Name() != ".info" {
				t.Errorf("%s/%s left after the refusal", dir, e.Name())
			}
		}
	}
	if len(asked) == 0 || !strings.HasPrefix(asked[0], r.st.Path()) {
		t.Fatalf("free space asked for %v, not for the root's disk", asked)
	}

	free = 2 * size
	b, installed, err := r.m.Install(ctx, "main", false)
	if err != nil || !installed || b.Version != "steam-main-100" {
		t.Fatalf("install with twice the size free: %+v %v %v", b, installed, err)
	}
	if v := r.m.Sources()[0]; v.Error != "" || v.Version != "steam-main-100" {
		t.Fatalf("source after the install: %+v", v)
	}
}

// Where the free space cannot be read, Steam builds are installed all the
// same, and the service log says so once.
func TestSteamInstallWithoutFreeSpaceReading(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.SetPassword("main", r.ctl.Password); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	r.m.log = slog.New(slog.NewTextHandler(&logs, nil))
	r.m.opts.DiskSpace = func(string) (uint64, uint64, error) {
		return 0, 0, errors.New("not read on this system")
	}
	for _, id := range []string{"100", "101"} {
		r.ctl.BuildID = id
		testkit.SetSteamControl(t, r.steam, r.ctl)
		b, installed, err := r.m.Install(ctx, "main", false)
		if err != nil || !installed || b.Version != "steam-main-"+id {
			t.Fatalf("install of %s: %+v %v %v", id, b, installed, err)
		}
	}
	if n := strings.Count(logs.String(), "not read on this system"); n != 1 {
		t.Fatalf("the log says %d times that the free space is unknown:\n%s", n, logs.String())
	}
}

// Surplus orders a source's builds by build id as numbers, keeps the
// newest keep_steam_builds and the build the source installed last, and
// leaves out every other build.
func TestSurplusOrdersByBuildID(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.SetPassword("main", r.ctl.Password); err != nil {
		t.Fatal(err)
	}
	r.m.cfg.KeepSteamBuilds = 2
	for _, id := range []string{"99", "100", "7"} {
		r.ctl.BuildID = id
		testkit.SetSteamControl(t, r.steam, r.ctl)
		if _, _, err := r.m.Install(ctx, "main", false); err != nil {
			t.Fatal(err)
		}
	}
	// Builds not of this source, some with names that start like its own.
	for _, v := range []string{"steam-mainx-1", "steam-main-x-1", "steam-other-1", "1.0.0"} {
		dir := filepath.Join(t.TempDir(), "b")
		exes := testkit.MakeContent(t, dir, testkit.BuildOptions{Version: v})
		if _, err := r.builds.InstallFolder(ctx, dir, builds.ManifestSpec{Version: v, Channel: "live", Executables: exes}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.m.Surplus("main")
	if err != nil {
		t.Fatal(err)
	}
	// 100 and 99 are the newest two; 7 was installed last, so it stays.
	if len(got) != 0 {
		t.Fatalf("surplus %v, want none", got)
	}
	r.ctl.BuildID = "100"
	testkit.SetSteamControl(t, r.steam, r.ctl)
	if _, installed, err := r.m.Install(ctx, "main", false); err != nil || installed {
		t.Fatalf("same build id: %v %v", installed, err)
	}
	if got, err := r.m.Surplus("main"); err != nil || strings.Join(got, " ") != "steam-main-7" {
		t.Fatalf("surplus %v %v, want steam-main-7", got, err)
	}
}

func TestSteamRefusesLinks(t *testing.T) {
	r := newRig(t)
	if err := os.Symlink(filepath.Join(r.content, "Data"), filepath.Join(r.content, "link")); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	r.ctl.Password = ""
	testkit.SetSteamControl(t, r.steam, r.ctl)
	_, _, err := r.m.Install(context.Background(), "main", true)
	if fault.CodeOf(err) != "unsafe_archive" {
		t.Fatalf("link in the app: %v", err)
	}
}
