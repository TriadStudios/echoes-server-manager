package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
)

// clock is a clock that a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// told collects what a runner tells.
type told struct {
	mu   sync.Mutex
	jobs []Job
	flag []bool
}

func (l *told) notify(j Job, restricted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.jobs = append(l.jobs, j)
	l.flag = append(l.flag, restricted)
}

func (l *told) all() []Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Job(nil), l.jobs...)
}

var alice = audit.Actor{Kind: "token", ID: "tok_1", Role: "site", ActingUser: "alice"}

// finish waits for a job to end.
func finish(t *testing.T, r *Runner, id string) Job {
	t.Helper()
	j, ok := r.Wait(id, 10*time.Second)
	if !ok || j.State == StateRunning {
		t.Fatalf("job %s did not end: %+v", id, j)
	}
	return j
}

func TestJobIsDoneWithItsResult(t *testing.T) {
	clk := newClock()
	var tl told
	var ended []Job
	var em sync.Mutex
	r := New(Options{Now: clk.now, Notify: tl.notify, Ended: func(j Job) { em.Lock(); ended = append(ended, j); em.Unlock() }})
	gate := make(chan struct{})
	j, err := r.Start(Spec{Kind: KindBackup, Target: "alpha", Actor: alice}, func(ctx context.Context) (any, error) {
		<-gate
		return map[string]string{"id": "b1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if j.State != StateRunning || j.Kind != KindBackup || j.Target != "alpha" || j.Actor != alice || len(j.ID) != 28 || !strings.HasPrefix(j.ID, "job_") ||
		strings.Trim(j.ID[4:], "0123456789abcdef") != "" || !j.Started.Equal(clk.now()) || j.Steps == nil || len(j.Steps) != 0 || j.Cancellable {
		t.Fatalf("a started job: %+v", j)
	}
	clk.add(3 * time.Second)
	close(gate)
	got := finish(t, r, j.ID)
	if got.State != StateDone || got.Error != nil || got.Ended.Sub(got.Started) != 3*time.Second {
		t.Fatalf("done job: %+v", got)
	}
	if b, _ := json.Marshal(got.Result); string(b) != `{"id":"b1"}` {
		t.Fatalf("result %s", b)
	}
	if g, ok := r.Get(j.ID); !ok || g.State != StateDone {
		t.Fatalf("Get: %+v %v", g, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("a job that was never started")
	}
	// Told: once when it started, once when it ended, the same id each time.
	all := tl.all()
	if len(all) != 2 || all[0].State != StateRunning || all[1].State != StateDone || all[1].ID != j.ID {
		t.Fatalf("told: %+v", all)
	}
	em.Lock()
	defer em.Unlock()
	if len(ended) != 1 || ended[0].State != StateDone {
		t.Fatalf("Ended: %+v", ended)
	}
}

func TestJobFailsWithTheErrorTheClassifierMakes(t *testing.T) {
	r := New(Options{Classify: func(err error) (string, string) { return "boom", "classified: " + err.Error() }})
	j, _ := r.Start(Spec{Kind: KindUpdate, Target: "alpha"}, func(context.Context) (any, error) {
		return "ignored", errors.New("it broke")
	})
	got := finish(t, r, j.ID)
	if got.State != StateFailed || got.Error == nil || got.Error.Code != "boom" || got.Error.Message != "classified: it broke" || got.Result != nil {
		t.Fatalf("failed job: %+v %+v", got, got.Error)
	}
	// Without a classifier: code internal, the text as it is.
	r = New(Options{})
	j, _ = r.Start(Spec{Kind: KindUpdate, Target: "alpha"}, func(context.Context) (any, error) { return nil, errors.New("plain") })
	if got := finish(t, r, j.ID); got.Error == nil || got.Error.Code != "internal" || got.Error.Message != "plain" {
		t.Fatalf("failed job: %+v", got.Error)
	}
}

func TestAPanicFailsTheJobAndNotTheProcess(t *testing.T) {
	r := New(Options{})
	j, _ := r.Start(Spec{Kind: KindBackup, Target: "alpha"}, func(context.Context) (any, error) { panic("oops") })
	got := finish(t, r, j.ID)
	if got.State != StateFailed || got.Error == nil || !strings.Contains(got.Error.Message, "oops") {
		t.Fatalf("panicking job: %+v", got)
	}
	if r.Running() != 0 {
		t.Fatalf("%d running", r.Running())
	}
}

func TestStepsAreRecordedWithTheirTimes(t *testing.T) {
	clk := newClock()
	r := New(Options{Now: clk.now})
	gate := make(chan struct{})
	reached := make(chan struct{})
	j, _ := r.Start(Spec{Kind: KindUpdate, Target: "alpha"}, func(ctx context.Context) (any, error) {
		Step(ctx, "backup")
		clk.add(2 * time.Second)
		Step(ctx, "stop")
		clk.add(5 * time.Second)
		Step(ctx, "start")
		close(reached)
		<-gate
		clk.add(time.Second)
		return nil, nil
	})
	<-reached
	got, _ := r.Get(j.ID)
	if got.Step != "start" || len(got.Steps) != 3 {
		t.Fatalf("while running: %+v", got)
	}
	if s := got.Steps[0]; s.Name != "backup" || s.Ended.Sub(s.Started) != 2*time.Second {
		t.Fatalf("first step: %+v", s)
	}
	if s := got.Steps[1]; s.Name != "stop" || s.Ended.Sub(s.Started) != 5*time.Second || !s.Started.Equal(got.Steps[0].Ended) {
		t.Fatalf("second step: %+v", s)
	}
	if s := got.Steps[2]; s.Name != "start" || !s.Ended.IsZero() {
		t.Fatalf("the step that runs has no end yet: %+v", s)
	}
	close(gate)
	got = finish(t, r, j.ID)
	// The last step ends with the job, and a finished job still names the
	// step it ended in.
	if got.Step != "start" || !got.Steps[2].Ended.Equal(got.Ended) || got.Steps[2].Ended.Sub(got.Steps[2].Started) != time.Second {
		t.Fatalf("finished: %+v", got)
	}
	// A copy: what the caller does to it does not reach the job.
	got.Steps[0].Name = "changed"
	if again, _ := r.Get(j.ID); again.Steps[0].Name != "backup" {
		t.Fatal("a snapshot shares its steps with the job")
	}
}

func TestProgressOfAStep(t *testing.T) {
	clk := newClock()
	var tl told
	r := New(Options{Now: clk.now, Notify: tl.notify})
	gate, reached := make(chan struct{}), make(chan struct{})
	j, _ := r.Start(Spec{Kind: KindInstall, Target: "steam:main"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		Progress(ctx, 250, 1000)
		clk.add(time.Second)
		Progress(ctx, 500, 0) // total not known
		close(reached)
		<-gate
		Percent(ctx, 61.5)
		Step(ctx, "verify")
		return nil, nil
	})
	<-reached
	got, _ := r.Get(j.ID)
	s := got.Steps[0]
	// The second call did not know the total: bytes are what it said, and
	// the percent of the call before is not kept.
	if s.Bytes != 500 || s.Total != 0 || s.Percent != nil {
		t.Fatalf("step without a total: %+v", s)
	}
	// With the total known the percent follows.
	r2 := New(Options{Now: clk.now})
	gate2, reached2 := make(chan struct{}), make(chan struct{})
	j2, _ := r2.Start(Spec{Kind: KindInstall, Target: "x"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		Progress(ctx, 333, 1000)
		close(reached2)
		<-gate2
		Progress(ctx, 5000, 1000) // more than the total: 100, not more
		Step(ctx, "verify")
		return nil, nil
	})
	<-reached2
	g2, _ := r2.Get(j2.ID)
	if s := g2.Steps[0]; s.Bytes != 333 || s.Total != 1000 || s.Percent == nil || *s.Percent != 33.3 {
		t.Fatalf("step with a total: %+v %v", s, s.Percent)
	}
	close(gate2)
	g2 = finish(t, r2, j2.ID)
	if s := g2.Steps[0]; s.Percent == nil || *s.Percent != 100 {
		t.Fatalf("step past its total: %+v", s)
	}
	close(gate)
	got = finish(t, r, j.ID)
	if s := got.Steps[0]; s.Percent == nil || *s.Percent != 61.5 {
		t.Fatalf("percent alone: %+v", s)
	}
	if got.Steps[1].Percent != nil || got.Steps[1].Bytes != 0 {
		t.Fatalf("the next step starts with no progress: %+v", got.Steps[1])
	}
	b, _ := json.Marshal(got.Steps[1])
	if strings.Contains(string(b), "bytes") || strings.Contains(string(b), "percent") || strings.Contains(string(b), "total") {
		t.Fatalf("a step with no progress shows none: %s", b)
	}
}

func TestProgressIsToldAFewTimesASecond(t *testing.T) {
	clk := newClock()
	var tl told
	r := New(Options{Now: clk.now, Notify: tl.notify})
	j, _ := r.Start(Spec{Kind: KindInstall, Target: "x"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		for i := range 1000 {
			Progress(ctx, int64(i), 1000)
			clk.add(time.Millisecond)
		}
		return nil, nil
	})
	finish(t, r, j.ID)
	n := len(tl.all())
	// start, step, about 4 progress notices a second for one second, end.
	if n < 4 || n > 12 {
		t.Fatalf("%d notices for 1000 updates in one second", n)
	}
}

func TestOnlyAnInstallCancelsAndOnlyBeforeItPlacesItsBuild(t *testing.T) {
	r := New(Options{})
	for _, kind := range []string{KindUpdate, KindBackup, KindRestore, KindSelfUpdate} {
		gate := make(chan struct{})
		j, _ := r.Start(Spec{Kind: kind, Target: "t"}, func(ctx context.Context) (any, error) { <-gate; return nil, nil })
		_, err := r.Cancel(j.ID)
		var nc *NotCancellableError
		if !errors.As(err, &nc) || !strings.Contains(nc.Reason, kind) {
			t.Errorf("cancel of a %s job: %v", kind, err)
		}
		if g, _ := r.Get(j.ID); g.Cancellable || g.Cancelling || g.State != StateRunning {
			t.Errorf("a %s job: %+v", kind, g)
		}
		close(gate)
		if g := finish(t, r, j.ID); g.State != StateDone {
			t.Errorf("a %s job that was asked to cancel: %+v", kind, g)
		}
	}

	// An install in its first steps: cancelled, and its context ends.
	started := make(chan struct{})
	j, _ := r.Start(Spec{Kind: KindInstall, Target: "steam:main"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		close(started)
		<-ctx.Done()
		if err := Commit(ctx, "place"); err == nil {
			return nil, errors.New("committed after a cancel")
		}
		return nil, ctx.Err()
	})
	<-started
	if g, _ := r.Get(j.ID); !g.Cancellable || g.Cancelling {
		t.Fatalf("an install that downloads: %+v", g)
	}
	snap, err := r.Cancel(j.ID)
	if err != nil || !snap.Cancelling || snap.State != StateRunning {
		t.Fatalf("cancel: %+v %v", snap, err)
	}
	// A second cancel of a job that is being cancelled changes nothing; it
	// may also find the job ended already.
	if _, err := r.Cancel(j.ID); err != nil {
		var nc *NotCancellableError
		if !errors.As(err, &nc) {
			t.Fatalf("second cancel: %v", err)
		}
	}
	got := finish(t, r, j.ID)
	if got.State != StateCancelled || got.Error != nil || got.Result != nil || got.Cancellable || got.Cancelling {
		t.Fatalf("cancelled install: %+v", got)
	}
	// A finished job cannot be cancelled.
	var nc *NotCancellableError
	if _, err := r.Cancel(j.ID); !errors.As(err, &nc) || !strings.Contains(nc.Reason, "ended") {
		t.Fatalf("cancel of a cancelled job: %v", err)
	}
	if _, err := r.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel of no job: %v", err)
	}

	// Once the install puts its build in place it cannot be cancelled: the
	// two do not both win.
	placing, release := make(chan struct{}), make(chan struct{})
	j, _ = r.Start(Spec{Kind: KindInstall, Target: "steam:main"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		Step(ctx, "verify")
		if err := Commit(ctx, "place"); err != nil {
			return nil, err
		}
		close(placing)
		<-release
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("stopped half way: %w", err)
		}
		return "placed", nil
	})
	<-placing
	if g, _ := r.Get(j.ID); g.Cancellable || g.Step != "place" {
		t.Fatalf("an install that places: %+v", g)
	}
	if _, err := r.Cancel(j.ID); !errors.As(err, &nc) || !strings.Contains(nc.Reason, "put in place") {
		t.Fatalf("cancel while placing: %v", err)
	}
	close(release)
	if got := finish(t, r, j.ID); got.State != StateDone || got.Result != "placed" {
		t.Fatalf("the install that was not cancelled: %+v", got)
	}
}

// Of a cancel and a commit that come at once, one wins and the other is told:
// never a build that is placed after the cancel was taken.
func TestCancelAndCommitNeverBothWin(t *testing.T) {
	for range 200 {
		r := New(Options{})
		atStep, goOn := make(chan struct{}), make(chan struct{})
		var committed bool
		j, _ := r.Start(Spec{Kind: KindInstall, Target: "x"}, func(ctx context.Context) (any, error) {
			Step(ctx, "verify")
			close(atStep)
			<-goOn
			if err := Commit(ctx, "place"); err != nil {
				return nil, err
			}
			committed = true
			return "placed", nil
		})
		<-atStep
		var cancelErr error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancelErr = r.Cancel(j.ID)
		}()
		close(goOn)
		wg.Wait()
		got := finish(t, r, j.ID)
		switch {
		case cancelErr == nil && (committed || got.State != StateCancelled):
			t.Fatalf("cancel was taken and the job went on: committed %v, %+v", committed, got)
		case cancelErr != nil && (!committed || got.State != StateDone):
			t.Fatalf("cancel was refused (%v) and the job did not place: %+v", cancelErr, got)
		}
	}
}

// The work gets a context of its own: a client that went away, and the
// context of its request, do not reach it.
func TestAJobGoesOnWhenTheRequestIsGone(t *testing.T) {
	r := New(Options{})
	type key struct{}
	_, cancelRequest := context.WithCancel(context.WithValue(context.Background(), key{}, "from the request"))
	proceed, saw := make(chan struct{}), make(chan error, 1)
	j, err := r.Start(Spec{Kind: KindUpdate, Target: "alpha"}, func(ctx context.Context) (any, error) {
		<-proceed
		if ctx.Value(key{}) != nil {
			saw <- errors.New("the job's context carries a value of the request")
		}
		saw <- ctx.Err()
		return "finished", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest() // the client goes away
	time.Sleep(20 * time.Millisecond)
	if g, _ := r.Get(j.ID); g.State != StateRunning {
		t.Fatalf("the job ended with the request: %+v", g)
	}
	close(proceed)
	if err := <-saw; err != nil {
		t.Fatalf("the job's context: %v", err)
	}
	if got := finish(t, r, j.ID); got.State != StateDone || got.Result != "finished" {
		t.Fatalf("job: %+v", got)
	}
}

func TestRetentionKeepsTheNewestHundredAndWhatIsYoungerThanADay(t *testing.T) {
	clk := newClock()
	r := New(Options{Now: clk.now})
	run := func(n int) []string {
		var ids []string
		for range n {
			j, err := r.Start(Spec{Kind: KindBackup, Target: "alpha"}, func(context.Context) (any, error) { return nil, nil })
			if err != nil {
				t.Fatal(err)
			}
			finish(t, r, j.ID)
			ids = append(ids, j.ID)
			clk.add(time.Second)
		}
		return ids
	}
	// 150 jobs within a few minutes: all of them are younger than a day.
	first := run(150)
	if got := len(r.List(Filter{})); got != 150 {
		t.Fatalf("%d jobs of the last day, want all 150", got)
	}
	// A day and an hour later nothing is young any more: the newest 100 stay.
	clk.add(25 * time.Hour)
	list := r.List(Filter{})
	if len(list) != 100 {
		t.Fatalf("%d jobs after a day, want the newest 100", len(list))
	}
	if list[0].ID != first[149] || list[99].ID != first[50] {
		t.Fatalf("not the newest 100: first %s last %s", list[0].ID, list[99].ID)
	}
	if _, ok := r.Get(first[49]); ok {
		t.Fatal("the 101st job is still there")
	}
	if _, ok := r.Get(first[50]); !ok {
		t.Fatal("the 100th job is gone")
	}
	// New jobs push the old ones out of the newest 100, and the old ones, a
	// day old, go with them.
	run(30)
	if got := len(r.List(Filter{})); got != 100 {
		t.Fatalf("%d jobs, want 30 new and the 70 newest of the old", got)
	}
	// Jobs younger than a day are kept beyond the 100.
	run(90)
	if got := len(r.List(Filter{})); got != 120 {
		t.Fatalf("%d jobs, want the 120 of the last day", got)
	}
	// A job that runs is never dropped, however old.
	gate := make(chan struct{})
	slow, _ := r.Start(Spec{Kind: KindUpdate, Target: "alpha"}, func(context.Context) (any, error) { <-gate; return nil, nil })
	clk.add(100 * time.Hour)
	run(120)
	if g, ok := r.Get(slow.ID); !ok || g.State != StateRunning {
		t.Fatalf("a running job was dropped: %+v %v", g, ok)
	}
	close(gate)
	finish(t, r, slow.ID)
}

func TestFinishedJobsAreBoundedWhateverTheirAge(t *testing.T) {
	clk := newClock()
	r := New(Options{Now: clk.now})
	for range hardCap + 50 {
		j, _ := r.Start(Spec{Kind: KindBackup, Target: "a"}, func(context.Context) (any, error) { return nil, nil })
		finish(t, r, j.ID)
	}
	if n := len(r.List(Filter{})); n != hardCap {
		t.Fatalf("%d jobs, want at most %d", n, hardCap)
	}
}

func TestListIsNewestFirstAndFilters(t *testing.T) {
	clk := newClock()
	r := New(Options{Now: clk.now})
	gate := make(chan struct{})
	mk := func(target string, work Work) Job {
		j, err := r.Start(Spec{Kind: KindBackup, Target: target}, work)
		if err != nil {
			t.Fatal(err)
		}
		clk.add(time.Second)
		return j
	}
	ok := func(context.Context) (any, error) { return nil, nil }
	bad := func(context.Context) (any, error) { return nil, errors.New("no") }
	a := mk("alpha", ok)
	b := mk("beta", bad)
	finish(t, r, a.ID)
	finish(t, r, b.ID)
	c := mk("alpha", func(context.Context) (any, error) { <-gate; return nil, nil })
	ids := func(f Filter) []string {
		var out []string
		for _, j := range r.List(f) {
			out = append(out, j.ID)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	eq(ids(Filter{}), c.ID, b.ID, a.ID)
	eq(ids(Filter{State: StateRunning}), c.ID)
	eq(ids(Filter{State: StateFailed}), b.ID)
	eq(ids(Filter{State: StateDone}), a.ID)
	eq(ids(Filter{State: StateCancelled}))
	eq(ids(Filter{Target: "alpha"}), c.ID, a.ID)
	eq(ids(Filter{Target: "alpha", State: StateDone}), a.ID)
	eq(ids(Filter{Target: "ghost"}))
	if got := r.List(Filter{Target: "ghost"}); got == nil {
		t.Fatal("an empty list is not nil: it is [] in JSON")
	}
	close(gate)
	finish(t, r, c.ID)
	if !ValidState("running") || !ValidState("cancelled") || ValidState("paused") || ValidState("") {
		t.Fatal("ValidState")
	}
}

func TestTooManyRunningJobsAreRefused(t *testing.T) {
	r := New(Options{MaxRunning: 2})
	gate := make(chan struct{})
	work := func(context.Context) (any, error) { <-gate; return nil, nil }
	a, err1 := r.Start(Spec{Kind: KindBackup, Target: "a"}, work)
	b, err2 := r.Start(Spec{Kind: KindBackup, Target: "b"}, work)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := r.Start(Spec{Kind: KindBackup, Target: "c"}, work); !errors.Is(err, ErrBusy) {
		t.Fatalf("third job: %v", err)
	}
	close(gate)
	finish(t, r, a.ID)
	finish(t, r, b.ID)
	if _, err := r.Start(Spec{Kind: KindBackup, Target: "c"}, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("after two ended: %v", err)
	}
}

func TestCloseWaitsForJobsThenStopsThem(t *testing.T) {
	// A job that ends on its own is waited for, and then nothing starts.
	r := New(Options{})
	gate, running := make(chan struct{}), make(chan struct{})
	j, _ := r.Start(Spec{Kind: KindBackup, Target: "a"}, func(ctx context.Context) (any, error) {
		close(running)
		<-gate
		return "done in time", nil
	})
	<-running
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closed <- r.Close(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-closed:
		t.Fatalf("Close did not wait for the job: %v", err)
	default:
	}
	if _, err := r.Start(Spec{Kind: KindBackup, Target: "b"}, func(context.Context) (any, error) { return nil, nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("a job started while closing: %v", err)
	}
	close(gate)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if g, _ := r.Get(j.ID); g.State != StateDone || g.Result != "done in time" {
		t.Fatalf("job: %+v", g)
	}

	// A job that does not end in time is asked to stop, through its context.
	r = New(Options{})
	running = make(chan struct{})
	j, _ = r.Start(Spec{Kind: KindInstall, Target: "steam:main"}, func(ctx context.Context) (any, error) {
		Step(ctx, "download")
		close(running)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-running
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v", err)
	}
	got := finish(t, r, j.ID)
	if got.State != StateFailed || got.Error == nil || got.Error.Code != "shutting_down" {
		t.Fatalf("a job stopped by the shutdown: %+v %+v", got, got.Error)
	}
}

func TestWhatTheWorkTellsIsNothingWithoutAJob(t *testing.T) {
	ctx := context.Background()
	Step(ctx, "x")
	Progress(ctx, 1, 2)
	Percent(ctx, 3)
	if err := Commit(ctx, "place"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// The end of a request is no reason to stop what a request always ran to
	// its end for.
	if err := Interrupted(cancelled); err != nil {
		t.Fatalf("Interrupted without a job: %v", err)
	}
	if err := Commit(cancelled, "place"); err != nil {
		t.Fatalf("Commit without a job: %v", err)
	}
	if InJob(ctx) || Quiet(ctx) != ctx {
		t.Fatal("a context without a job")
	}
}

func TestQuietHidesTheStepsOfAPartOfTheWork(t *testing.T) {
	r := New(Options{})
	j, _ := r.Start(Spec{Kind: KindUpdate, Target: "a"}, func(ctx context.Context) (any, error) {
		Step(ctx, "backup")
		q := Quiet(ctx)
		if InJob(q) || !InJob(ctx) {
			return nil, errors.New("Quiet changed the wrong context")
		}
		Step(q, "wait_save")
		Step(q, "copy")
		Progress(q, 1, 2)
		Step(ctx, "stop")
		return nil, Interrupted(q)
	})
	got := finish(t, r, j.ID)
	if got.State != StateDone || len(got.Steps) != 2 || got.Steps[0].Name != "backup" || got.Steps[1].Name != "stop" || got.Steps[0].Bytes != 0 {
		t.Fatalf("job: %+v", got)
	}
}

func TestInterruptedSaysWhenAJobIsCancelled(t *testing.T) {
	r := New(Options{})
	started, seen := make(chan struct{}), make(chan error, 1)
	j, _ := r.Start(Spec{Kind: KindInstall, Target: "x"}, func(ctx context.Context) (any, error) {
		Step(ctx, "verify")
		if Interrupted(ctx) != nil {
			return nil, errors.New("interrupted before anything")
		}
		close(started)
		for Interrupted(ctx) == nil {
			time.Sleep(time.Millisecond)
		}
		seen <- Interrupted(ctx)
		return nil, Interrupted(ctx)
	})
	<-started
	r.Cancel(j.ID)
	if err := <-seen; !errors.Is(err, context.Canceled) {
		t.Fatalf("Interrupted: %v", err)
	}
	if got := finish(t, r, j.ID); got.State != StateCancelled {
		t.Fatalf("job: %+v", got)
	}
}

func TestRestrictedIsAskedOutsideTheLockAndTold(t *testing.T) {
	var tl told
	var r *Runner
	r = New(Options{
		Notify: tl.notify,
		Restrict: func(j Job) bool {
			// The runner is free to be asked (its lock is not held).
			r.Running()
			return j.Target == "hidden"
		},
	})
	for _, target := range []string{"hidden", "plain"} {
		j, _ := r.Start(Spec{Kind: KindBackup, Target: target}, func(ctx context.Context) (any, error) { Step(ctx, "copy"); return nil, nil })
		finish(t, r, j.ID)
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if len(tl.jobs) != 6 {
		t.Fatalf("%d notices, want 3 for each job", len(tl.jobs))
	}
	for i, j := range tl.jobs {
		if tl.flag[i] != (j.Target == "hidden") {
			t.Errorf("notice %d of %s: restricted %v", i, j.Target, tl.flag[i])
		}
	}
}

func TestEveryNoticeOfAJobIsInOrder(t *testing.T) {
	var tl told
	r := New(Options{Notify: tl.notify})
	j, _ := r.Start(Spec{Kind: KindUpdate, Target: "a"}, func(ctx context.Context) (any, error) {
		for _, s := range []string{"backup", "stop", "switch_build", "start", "wait_ready"} {
			Step(ctx, s)
		}
		return nil, nil
	})
	finish(t, r, j.ID)
	var steps []int
	var last string
	for _, n := range tl.all() {
		steps = append(steps, len(n.Steps))
		last = n.State
	}
	if fmt.Sprint(steps) != "[0 1 2 3 4 5 5]" || last != StateDone {
		t.Fatalf("notices: steps %v, last state %s", steps, last)
	}
}
