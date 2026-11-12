package instances

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/builds"
	"github.com/TriadStudios/echoes-server-manager/internal/config"
	"github.com/TriadStudios/echoes-server-manager/internal/events"
	"github.com/TriadStudios/echoes-server-manager/internal/fault"
	"github.com/TriadStudios/echoes-server-manager/internal/gameserver"
	"github.com/TriadStudios/echoes-server-manager/internal/store"
	"github.com/TriadStudios/echoes-server-manager/internal/supervisor"
	"github.com/TriadStudios/echoes-server-manager/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.Run(m)) }

// Each test gets its own slice of ports so leftovers of one test never
// collide with the next.
var portBlock atomic.Int32

type rig struct {
	t   *testing.T
	st  *store.Store
	cfg *config.Config
	b   *builds.Manager
	m   *Manager
	bus *events.Bus
	src string
	log *syncBuffer // everything the managers of this rig log
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newRig(t *testing.T, tune func(*config.Config)) *rig {
	t.Helper()
	root, err := os.MkdirTemp("", "esmi")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	blk := int(portBlock.Add(1))
	cfg := config.Defaults(st.Path())
	cfg.GamePorts = strconv.Itoa(24000+blk*20) + "-" + strconv.Itoa(24000+blk*20+9)
	cfg.QueryPorts = strconv.Itoa(25000+blk*20) + "-" + strconv.Itoa(25000+blk*20+9)
	cfg.Backoff = config.Backoff{Base: config.Duration(50 * time.Millisecond), Max: config.Duration(200 * time.Millisecond),
		Window: config.Duration(10 * time.Minute), MaxCrashes: 5}
	cfg.StopTimeout = config.Duration(5 * time.Second)
	cfg.ReadyTimeout = config.Duration(10 * time.Second)
	cfg.BackupSaveWait = config.Duration(3 * time.Second)
	if tune != nil {
		tune(cfg)
	}
	sup, err := supervisor.NewProcess(st, supervisor.Options{PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(4096)
	b := builds.New(st, builds.Options{Bus: bus})
	r := &rig{t: t, st: st, cfg: cfg, b: b, bus: bus, src: t.TempDir(), log: &syncBuffer{}}
	r.m = r.newManager(sup)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, v := range r.m.List() {
			r.m.Stop(ctx, v.Name)
		}
		r.m.Close()
		sup.Close()
		st.Close()
		os.RemoveAll(root)
	})
	return r
}

func (r *rig) newManager(sup supervisor.Supervisor) *Manager {
	r.t.Helper()
	m, err := New(Options{Store: r.st, Config: r.cfg, Supervisor: sup, Builds: r.b, Bus: r.bus,
		Logger:        slog.New(slog.NewJSONHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		WatchInterval: 50 * time.Millisecond, SchedulerInterval: time.Hour})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := m.Resume(); err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *rig) install(o testkit.BuildOptions) {
	r.t.Helper()
	dir := testkit.MakeBuild(r.t, r.src, o)
	if _, err := r.b.Install(context.Background(), dir, ""); err != nil {
		r.t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// fastRules makes the fake server quick.
func fastRules(extra map[string]string) *map[string]string {
	m := map[string]string{"FakeReadyDelay": "100ms", "FakeSaveInterval": "200ms"}
	for k, v := range extra {
		m[k] = v
	}
	return &m
}

func (r *rig) create(name string, rules map[string]string) View {
	r.t.Helper()
	v, err := r.m.Create(CreateRequest{Name: name, Settings: Settings{BindIP: ptr("127.0.0.1"), Rules: fastRules(rules)}})
	if err != nil {
		r.t.Fatal(err)
	}
	return v
}

func (r *rig) waitState(name string, want State, timeout time.Duration) Status {
	r.t.Helper()
	var st Status
	testkit.Eventually(r.t, timeout, func() bool {
		st, _ = r.m.Status(name)
		return st.State == want
	}, "instance %s never reached state %s", name, want)
	return st
}

func TestPortAllocation(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	g, _ := config.ParsePortRange(r.cfg.GamePorts)
	q, _ := config.ParsePortRange(r.cfg.QueryPorts)
	a := r.create("alpha", nil)
	if a.Ports.Game != g.Lo || a.Ports.Query != q.Lo {
		t.Fatalf("first instance got %+v, want %d/%d", a.Ports, g.Lo, q.Lo)
	}
	// Something else on the box holds the next game port.
	busy, err := net.ListenPacket("udp", "127.0.0.1:"+strconv.Itoa(g.Lo+1))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	b := r.create("beta", nil)
	if b.Ports.Game != g.Lo+2 || b.Ports.Query != q.Lo+2 {
		t.Fatalf("second instance got %+v, want %d/%d (skipping a used and a busy pair)", b.Ports, g.Lo+2, q.Lo+2)
	}
	_, err = r.m.Create(CreateRequest{Name: "gamma", Settings: Settings{BindIP: ptr("127.0.0.1"), Ports: &Ports{Game: a.Ports.Game}}})
	if fault.CodeOf(err) != "port_in_use" {
		t.Fatalf("explicit used port: %v", err)
	}
	_, err = r.m.Create(CreateRequest{Name: "gamma", Settings: Settings{BindIP: ptr("127.0.0.1"), Ports: &Ports{Game: g.Lo + 1, Query: q.Lo + 5}}})
	if fault.CodeOf(err) != "port_in_use" {
		t.Fatalf("explicit busy port: %v", err)
	}
	c, err := r.m.Create(CreateRequest{Name: "gamma", Settings: Settings{BindIP: ptr("127.0.0.1"), Ports: &Ports{Game: g.Lo + 7}}})
	if err != nil || c.Ports.Game != g.Lo+7 || c.Ports.Query != q.Lo+1 {
		t.Fatalf("explicit game port: %+v %v", c.Ports, err)
	}
	if _, err := r.m.Create(CreateRequest{Name: "alpha"}); fault.CodeOf(err) != "instance_exists" {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := r.m.Create(CreateRequest{Name: "delta", Build: "9.9"}); fault.CodeOf(err) != "no_build" {
		t.Fatalf("missing build: %v", err)
	}
	if _, err := r.m.Create(CreateRequest{Name: "delta", Settings: Settings{Channel: ptr("test")}}); fault.CodeOf(err) != "no_build" {
		t.Fatalf("no test build: %v", err)
	}
	if _, _, err := r.m.Patch("alpha", PatchRequest{Build: ptr("2.0")}); !fault.Is(err, fault.Invalid) {
		t.Fatalf("patch build: %v", err)
	}
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{Ports: &Ports{Game: b.Ports.Game, Query: q.Lo + 9}}}); fault.CodeOf(err) != "port_in_use" {
		t.Fatalf("patch to another instance's port: %v", err)
	}
}

func TestLifecycleCrashAndGiveUp(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	st, err := r.m.Start(ctx, "alpha", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Running || st.ReadySource == "" || st.PID == 0 {
		t.Fatalf("after start: %+v", st)
	}
	if _, err := r.m.Start(ctx, "alpha", false); fault.CodeOf(err) != "already_running" {
		t.Fatalf("second start: %v", err)
	}
	if err := r.m.Delete(ctx, "alpha", false); fault.CodeOf(err) != "instance_running" {
		t.Fatalf("delete while running: %v", err)
	}
	pid := st.PID
	data, _ := r.st.Abs(dataDir("alpha"))
	if err := testkit.WriteControl(data, testkit.ControlCrash); err != nil {
		t.Fatal(err)
	}
	testkit.Eventually(t, 10*time.Second, func() bool {
		st, _ = r.m.Status("alpha")
		return st.State == Running && st.PID != pid
	}, "no restart after the crash")
	if st.Crashes != 1 || st.GaveUp {
		t.Fatalf("after one crash: %+v", st)
	}
	lines, err := r.m.Logs("alpha", SourceServer, 50, true)
	if err != nil || len(lines) == 0 {
		t.Fatalf("logs: %v %v", lines, err)
	}
	if st, err = r.m.Stop(ctx, "alpha"); err != nil || st.State != Stopped {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if b, err := r.st.ReadFile(dataDir("alpha") + "/" + gameserver.SaveFileRel("alpha")); err != nil || !strings.Contains(string(b), "counter=") {
		t.Fatalf("world file after stop: %q %v", b, err)
	}

	// A server that always crashes is given up on.
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{Rules: fastRules(map[string]string{"FakeCrashAfter": "200ms"})}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Start(ctx, "alpha", false); err != nil {
		t.Fatal(err)
	}
	st = r.waitState("alpha", Failed, 20*time.Second)
	if !st.GaveUp || st.Crashes != 5 {
		t.Fatalf("after giving up: %+v", st)
	}
	time.Sleep(500 * time.Millisecond)
	if st, _ := r.m.Status("alpha"); st.State != Failed {
		t.Fatalf("restarted after giving up: %+v", st)
	}
	// A manual start clears the count.
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{Rules: fastRules(nil)}}); err != nil {
		t.Fatal(err)
	}
	if st, err := r.m.Start(ctx, "alpha", true); err != nil || st.Crashes != 0 || st.GaveUp {
		t.Fatalf("manual start after giving up: %+v %v", st, err)
	}
	if _, err := r.m.Stop(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Delete(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	if r.st.Exists(instDir("alpha")) {
		t.Fatal("instance folder still exists")
	}
}

func TestNeverReadyFails(t *testing.T) {
	r := newRig(t, func(c *config.Config) { c.ReadyTimeout = config.Duration(time.Second) })
	r.install(testkit.BuildOptions{Version: "1.0"})
	// The fake never reports ready: no status file state, no ready line and
	// no query answer.
	r.create("alpha", map[string]string{"FakeNoReady": "true"})
	_, err := r.m.Start(context.Background(), "alpha", true)
	if fault.CodeOf(err) != "not_ready" {
		t.Fatalf("start of a never-ready server: %v", err)
	}
	if st, _ := r.m.Status("alpha"); st.State != Failed {
		t.Fatalf("after ready timeout: %+v", st)
	}
	if r.m.alive("alpha") {
		t.Fatal("server still running after the ready timeout")
	}
}

func TestSchedulerTick(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	every := config.Duration(time.Hour)
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{
		Schedule: &Schedule{RestartAt: []string{"04:00"}}, Backup: &BackupPolicy{Every: every, Keep: 3}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 1, 3, 0, 0, 0, time.Local)
	r.m.schedTick(now)
	in := r.m.lookup("alpha")
	in.mu.Lock()
	next, nb := in.nextRestart, in.nextBackup
	in.mu.Unlock()
	if !next.Equal(time.Date(2026, 5, 1, 4, 0, 0, 0, time.Local)) || !nb.Equal(now.Add(time.Hour)) {
		t.Fatalf("next restart %v, next backup %v", next, nb)
	}
	// Due while stopped: skipped, and the next slot is tomorrow.
	r.m.schedTick(now.Add(time.Hour + time.Minute))
	in.mu.Lock()
	next = in.nextRestart
	in.mu.Unlock()
	if !next.Equal(time.Date(2026, 5, 2, 4, 0, 0, 0, time.Local)) {
		t.Fatalf("next restart after a skipped slot: %v", next)
	}
	if list, _ := r.m.ListBackups("alpha"); len(list) != 0 {
		t.Fatalf("a stopped instance was backed up: %v", list)
	}
}

func TestScheduledWorkAndBackupPruning(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	if _, _, err := r.m.Patch("alpha", PatchRequest{Settings: Settings{
		Schedule: &Schedule{RestartAt: []string{"04:00"}},
		Backup:   &BackupPolicy{Every: config.Duration(time.Hour), Keep: 2}}}); err != nil {
		t.Fatal(err)
	}
	st, err := r.m.Start(ctx, "alpha", true)
	if err != nil {
		t.Fatal(err)
	}
	pid := st.PID
	now := time.Date(2026, 5, 1, 3, 59, 0, 0, time.Local)
	r.m.schedTick(now) // plans 04:00 and a backup an hour from now
	r.m.schedTick(now.Add(time.Hour + time.Second))
	testkit.Eventually(t, 15*time.Second, func() bool {
		st, _ = r.m.Status("alpha")
		list, _ := r.m.ListBackups("alpha")
		return st.State == Running && st.PID != pid && len(list) == 1 && list[0].Reason == ReasonScheduled
	}, "scheduled restart and backup did not both happen")
	if st.Reason != "ready ("+st.ReadySource+")" {
		t.Fatalf("status after the scheduled restart: %+v", st)
	}
	for i := 0; i < 3; i++ {
		if _, err := r.m.Backup(ctx, "alpha", ReasonManual); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.m.ListBackups("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Reason != ReasonManual || list[1].Reason != ReasonManual || list[0].Created.Before(list[1].Created) {
		t.Fatalf("after pruning to 2: %+v", list)
	}
	// Restoring prunes too, but never the backup being restored: it is
	// kept on top of the newest two.
	oldest := list[1].ID
	if _, err := r.m.Restore(ctx, "alpha", oldest); err != nil {
		t.Fatal(err)
	}
	list, _ = r.m.ListBackups("alpha")
	ids := []string{}
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	if len(list) != 3 || list[0].Reason != ReasonPreRestore || list[2].ID != oldest {
		t.Fatalf("after restore with keep 2: %v", ids)
	}
}

func TestRestoreRefusesDamagedBackups(t *testing.T) {
	r := newRig(t, nil)
	r.install(testkit.BuildOptions{Version: "1.0"})
	r.create("alpha", nil)
	ctx := context.Background()
	if _, err := r.m.Start(ctx, "alpha", true); err != nil {
		t.Fatal(err)
	}
	b, err := r.m.Backup(ctx, "alpha", ReasonManual)
	if err != nil || len(b.Files) != 1 {
		t.Fatalf("backup %+v %v", b, err)
	}
	if err := r.st.WriteFile("outside.sav", []byte("not a save"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteFile(backupDir("alpha", b.ID)+"/notes.txt", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, files := range [][]string{
		{"../../../outside.sav"},
		{"SaveGames/../../../outside.sav"},
		{"/SaveGames/alpha.sav"},
		{`SaveGames\alpha.sav`},
		{"notes.txt"},
		{"SaveGames/missing.sav"},
	} {
		meta := b
		meta.Files = files
		if err := r.st.WriteJSON(backupDir("alpha", b.ID)+"/meta.json", meta, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.m.Restore(ctx, "alpha", b.ID); fault.CodeOf(err) != "backup_damaged" {
			t.Fatalf("restore with files %q: %v", files, err)
		}
		if st, _ := r.m.Status("alpha"); st.State != Running {
			t.Fatalf("a refused restore stopped the server: %+v", st)
		}
	}
	if list, _ := r.m.ListBackups("alpha"); len(list) != 1 {
		t.Fatalf("a refused restore made backups: %+v", list)
	}
}
