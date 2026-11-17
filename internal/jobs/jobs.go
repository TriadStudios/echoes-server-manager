// Package jobs runs the long calls of the API (an update, a backup, a
// restore, the install of a build, ESM's own update) as jobs that a client
// can look at while they run: a job has a state, the step it is in, the
// steps it went through with their times, and in the end its result or its
// error. A job runs with a context of its own, never the request's, so a
// client that goes away does not stop it; ESM's shutdown still ends it (see
// Runner.Close).
//
// The work of a job tells its steps through the context it is given:
// Step, Progress, Commit and the other functions of this package look the
// job up in the context, and do nothing in a context that has none, so the
// code that does the work serves a plain request and a job alike.
//
// Jobs live in memory only: they are lost when ESM ends, and a job that ran
// then is not resumed.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/TriadStudios/echoes-server-manager/internal/audit"
)

// Kinds of jobs.
const (
	KindUpdate     = "update"
	KindBackup     = "backup"
	KindRestore    = "restore"
	KindInstall    = "install"
	KindSelfUpdate = "self_update"
)

// States of a job. A job is running until its work returns, and then is done
// (it worked), failed (it did not) or cancelled (a cancel was asked for
// before it could not be stopped any more, and it stopped).
const (
	StateRunning   = "running"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// ValidState reports whether s is a state of a job.
func ValidState(s string) bool {
	switch s {
	case StateRunning, StateDone, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Retention and limits. A finished job is kept while it is among the newest
// DefaultKeep jobs or ended less than DefaultMaxAge ago, but never more than
// hardCap finished jobs, so that memory stays bounded however many jobs are
// started. A running job is never dropped.
const (
	DefaultKeep       = 100
	DefaultMaxAge     = 24 * time.Hour
	DefaultMaxRunning = 32
	hardCap           = 1000
	// progressEvery is the most often a job tells about the progress of a
	// step (bytes done): every start and end of a step is told at once.
	progressEvery = 250 * time.Millisecond
	// shutdownGrace is how long Close waits for jobs to end after it asked
	// them to.
	shutdownGrace = 3 * time.Second
)

// Errors of the runner.
var (
	// ErrClosed: the runner no longer starts jobs (ESM is shutting down).
	ErrClosed = errors.New("ESM is shutting down: no job is started")
	// ErrBusy: as many jobs run as the runner allows.
	ErrBusy = errors.New("too many jobs are running")
	// ErrNotFound: there is no job with that id (or it was dropped).
	ErrNotFound = errors.New("no such job")
	// ErrCancelled is what Commit answers once a cancel was asked for.
	ErrCancelled = errors.New("the job was cancelled")
	// ErrEnded is what Commit answers for a job that has ended.
	ErrEnded = errors.New("the job has ended")
)

// NotCancellableError says why a job cannot be cancelled.
type NotCancellableError struct{ Reason string }

func (e *NotCancellableError) Error() string { return e.Reason }

// StepRecord is one step of a job.
type StepRecord struct {
	Name    string    `json:"name"`
	Started time.Time `json:"started"`
	// Ended is when the step ended: when the next one began, or the job did.
	Ended time.Time `json:"ended,omitzero"`
	// Bytes and Total are how far a step that moves bytes (a download) has
	// come and how many there are, when they are known; Percent is how far it
	// has come, from 0 to 100, when that is known.
	Bytes   int64    `json:"bytes,omitzero"`
	Total   int64    `json:"total,omitzero"`
	Percent *float64 `json:"percent,omitempty"`
}

// Error is how a failed job says what went wrong, in the shape of an error
// answer of the API.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Job is a job as it is shown: a copy, which later changes do not touch.
type Job struct {
	ID string `json:"id"`
	// Kind is what the job does (update, backup, restore, install or
	// self_update); Target is what it does it to: an instance, a build
	// source, or "esm".
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Actor is who started the job, as the audit log records the call.
	Actor audit.Actor `json:"actor"`
	State string      `json:"state"`
	// Step is the name of the step the job is in; of a finished job, the one
	// it ended in. It is empty before the first step.
	Step  string       `json:"step"`
	Steps []StepRecord `json:"steps"`
	// Started and Ended are when the job began and when its work returned.
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
	// Result is what the call of a done job answers when it is made without
	// async; Error says what went wrong in a failed one.
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
	// Cancellable says that a cancel would be taken now: the job is an
	// install that has not begun to put its build in place. Cancelling says
	// that a cancel was asked for and the job has not ended yet.
	Cancellable bool `json:"cancellable"`
	Cancelling  bool `json:"cancelling,omitempty"`
}

// Options configure a Runner. Everything is optional.
type Options struct {
	// Keep and MaxAge say which finished jobs are kept (see DefaultKeep and
	// DefaultMaxAge); MaxRunning is how many jobs may run at once.
	Keep       int
	MaxAge     time.Duration
	MaxRunning int
	Now        func() time.Time
	Logger     *slog.Logger
	// Notify is told of every change of a job, in the order the changes were
	// made, with the lock of the runner held: it must be quick and must not
	// call the runner. restricted is what Restrict said of the job.
	Notify func(j Job, restricted bool)
	// Restrict says whether what a job shows of its result and error is for
	// callers who see everything only; it is asked before each notification,
	// outside the lock of the runner.
	Restrict func(j Job) bool
	// Ended is called once for every job, after it ended, outside the lock.
	Ended func(j Job)
	// Classify turns the error of a failed job into the code and message of
	// an error answer of the API (default: "internal" and the text).
	Classify func(err error) (code, message string)
}

// Runner runs jobs and remembers them.
type Runner struct {
	opts Options
	log  *slog.Logger
	now  func() time.Time

	root       context.Context
	cancelRoot context.CancelFunc
	wg         sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	byID    map[string]*job
	ordered []*job // by start, oldest first
	running int
}

type job struct {
	// ident holds what never changes: id, kind, target and actor.
	ident Job
	pub   Job
	// cancel ends the job's context.
	cancel context.CancelFunc
	// asked is set by Cancel; committed by Commit. Cancel is not taken once
	// committed, and Commit is not taken once asked: of the two, one wins.
	asked, committed bool
	done             chan struct{}
	lastTold         time.Time
}

// New makes a runner.
func New(o Options) *Runner {
	if o.Keep <= 0 {
		o.Keep = DefaultKeep
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.MaxRunning <= 0 {
		o.MaxRunning = DefaultMaxRunning
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	root, cancel := context.WithCancel(context.Background())
	return &Runner{opts: o, log: o.Logger, now: o.Now, root: root, cancelRoot: cancel, byID: map[string]*job{}}
}

// Spec says what a job is.
type Spec struct {
	Kind, Target string
	Actor        audit.Actor
}

// Work is what a job does. It gets the job's own context. It returns what a
// call that is not a job answers (the result), or the error.
type Work func(ctx context.Context) (result any, err error)

// newID makes a random id: "job_" and 24 hex digits. It is URL-safe, and never
// starts with a hyphen, which a command line would take for a flag.
func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}

// Start begins a job that runs work, and returns the job as it is now. It
// answers ErrClosed after Close, and ErrBusy while the most jobs the runner
// allows are running.
func (r *Runner) Start(spec Spec, work Work) (Job, error) {
	id := newID()
	ident := Job{ID: id, Kind: spec.Kind, Target: spec.Target, Actor: spec.Actor}
	restricted := r.restricted(ident)
	ctx, cancel := context.WithCancel(r.root)
	j := &job{cancel: cancel, done: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return Job{}, ErrClosed
	}
	if r.running >= r.opts.MaxRunning {
		r.mu.Unlock()
		cancel()
		return Job{}, ErrBusy
	}
	now := r.now()
	j.ident, j.pub = ident, ident
	j.pub.State, j.pub.Started, j.pub.Steps = StateRunning, now, []StepRecord{}
	j.pub.Cancellable = j.cancellableLocked()
	r.byID[id] = j
	r.ordered = append(r.ordered, j)
	r.running++
	r.wg.Add(1)
	r.pruneLocked(now)
	r.tellLocked(j, restricted, now)
	snap := j.snapshot()
	r.mu.Unlock()
	go r.run(withTracker(ctx, &tracker{r: r, j: j}), j, work)
	return snap, nil
}

func (r *Runner) run(ctx context.Context, j *job, work Work) {
	defer r.wg.Done()
	res, err := safely(ctx, work)
	restricted := r.restricted(j.ident)
	r.mu.Lock()
	now := r.now()
	r.endStepLocked(j, now)
	switch {
	case err == nil:
		j.pub.State, j.pub.Result = StateDone, res
	case j.asked:
		j.pub.State = StateCancelled
	case r.root.Err() != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
		j.pub.State = StateFailed
		j.pub.Error = &Error{Code: "shutting_down", Message: "ESM was shutting down; the job was stopped"}
	default:
		code, msg := "internal", err.Error()
		if r.opts.Classify != nil {
			code, msg = r.opts.Classify(err)
		}
		j.pub.State, j.pub.Error = StateFailed, &Error{Code: code, Message: msg}
	}
	j.pub.Ended = now
	j.pub.Cancellable = false
	j.pub.Cancelling = false
	r.running--
	j.cancel()
	close(j.done)
	r.pruneLocked(now)
	r.tellLocked(j, restricted, now)
	snap := j.snapshot()
	r.mu.Unlock()
	if r.opts.Ended != nil {
		r.opts.Ended(snap)
	}
}

// safely runs work, and turns a panic into an error: a job that panics
// fails, ESM goes on.
func safely(ctx context.Context, work Work) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = nil, fmt.Errorf("the job panicked: %v", p)
		}
	}()
	return work(ctx)
}

func (r *Runner) restricted(j Job) bool {
	return r.opts.Restrict != nil && r.opts.Restrict(j)
}

func (j *job) snapshot() Job {
	c := j.pub
	c.Steps = slices.Clone(j.pub.Steps)
	return c
}

func (j *job) cancellableLocked() bool {
	return j.pub.Kind == KindInstall && j.pub.State == StateRunning && !j.committed
}

// tellLocked tells Notify of the job as it is now.
func (r *Runner) tellLocked(j *job, restricted bool, now time.Time) {
	j.lastTold = now
	if r.opts.Notify != nil {
		r.opts.Notify(j.snapshot(), restricted)
	}
}

// endStepLocked ends the step that runs.
func (r *Runner) endStepLocked(j *job, now time.Time) {
	if n := len(j.pub.Steps); n > 0 && j.pub.Steps[n-1].Ended.IsZero() {
		j.pub.Steps[n-1].Ended = now
	}
}

// pruneLocked drops the finished jobs that are no longer kept.
func (r *Runner) pruneLocked(now time.Time) {
	finished := 0
	var drop []string
	for i := len(r.ordered) - 1; i >= 0; i-- { // newest first
		j := r.ordered[i]
		if j.pub.State == StateRunning {
			continue
		}
		finished++
		pos := len(r.ordered) - i
		if pos <= r.opts.Keep && finished <= hardCap {
			continue
		}
		if finished <= hardCap && now.Sub(j.pub.Ended) < r.opts.MaxAge {
			continue
		}
		drop = append(drop, j.pub.ID)
	}
	if len(drop) == 0 {
		return
	}
	for _, id := range drop {
		delete(r.byID, id)
	}
	r.ordered = slices.DeleteFunc(r.ordered, func(j *job) bool { _, ok := r.byID[j.pub.ID]; return !ok })
}

// Get returns the job with this id.
func (r *Runner) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	j := r.byID[id]
	if j == nil {
		return Job{}, false
	}
	return j.snapshot(), true
}

// Filter selects jobs; an empty field matches every job.
type Filter struct {
	State  string
	Target string
}

// List returns the jobs that match f, newest first.
func (r *Runner) List(f Filter) []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	out := []Job{}
	for i := len(r.ordered) - 1; i >= 0; i-- {
		j := r.ordered[i]
		if f.State != "" && j.pub.State != f.State || f.Target != "" && j.pub.Target != f.Target {
			continue
		}
		out = append(out, j.snapshot())
	}
	return out
}

// Cancel asks the job to stop. Only an install can be cancelled, and only
// until it begins to put its build in place (see Commit): anything else
// answers a *NotCancellableError. The job is then stopped by cancelling its
// context; it ends as cancelled once its work returns (see Wait). Cancelling
// a job that is being cancelled changes nothing.
func (r *Runner) Cancel(id string) (Job, error) {
	r.mu.Lock()
	j := r.byID[id]
	r.mu.Unlock()
	if j == nil {
		return Job{}, ErrNotFound
	}
	restricted := r.restricted(j.ident)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case j.pub.State != StateRunning:
		return Job{}, &NotCancellableError{Reason: "the job has ended already (it is " + j.pub.State + ")"}
	case j.pub.Kind != KindInstall:
		return Job{}, &NotCancellableError{Reason: "only the install of a build can be cancelled; a job of kind " + j.pub.Kind + " runs to its end"}
	case j.committed:
		return Job{}, &NotCancellableError{Reason: "the build is being put in place; that is not stopped half way"}
	}
	if !j.asked {
		j.asked = true
		j.pub.Cancelling = true
		j.cancel()
		r.tellLocked(j, restricted, r.now())
	}
	return j.snapshot(), nil
}

// Wait waits up to d for the job to end, and returns it as it is then.
func (r *Runner) Wait(id string, d time.Duration) (Job, bool) {
	r.mu.Lock()
	j := r.byID[id]
	r.mu.Unlock()
	if j == nil {
		return Job{}, false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-j.done:
	case <-t.C:
	}
	return r.Get(id)
}

// Running is how many jobs run.
func (r *Runner) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Close stops the runner taking jobs, and waits for the jobs that run to
// end, as the end of ESM waits for the requests that run: until ctx is done.
// What still runs then is asked to stop (its context ends), and given a few
// seconds to do so; a job that does not is left. It returns ctx's error when
// jobs had to be asked to stop.
func (r *Runner) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	ended := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(ended)
	}()
	select {
	case <-ended:
		r.cancelRoot()
		return nil
	case <-ctx.Done():
	}
	r.cancelRoot()
	t := time.NewTimer(shutdownGrace)
	defer t.Stop()
	select {
	case <-ended:
	case <-t.C:
		r.log.Warn("jobs did not stop when ESM shut down", "running", r.Running())
	}
	return ctx.Err()
}

// --- what the work tells ---

type ctxKey struct{}

// tracker ties a context to its job.
type tracker struct {
	r *Runner
	j *job
}

func withTracker(ctx context.Context, t *tracker) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

func from(ctx context.Context) *tracker {
	t, _ := ctx.Value(ctxKey{}).(*tracker)
	return t
}

// Quiet returns ctx with the job taken out of it: Step, Progress and the
// others then do nothing, so that a part of the work that has steps of its
// own can be run as one step of the job (the context is cancelled with ctx
// as before).
func Quiet(ctx context.Context) context.Context {
	if from(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, (*tracker)(nil))
}

// InJob reports whether ctx belongs to a job.
func InJob(ctx context.Context) bool { return from(ctx) != nil }

// Step ends the step the job of ctx is in and begins one named name. It does
// nothing in a context without a job.
func Step(ctx context.Context, name string) {
	if t := from(ctx); t != nil {
		t.r.step(t.j, name, false)
	}
}

// Commit is Step for the step from which on the job cannot be cancelled any
// more (an install putting its build in place). It answers ErrCancelled, and
// begins nothing, when a cancel was asked for before, and ctx's error when
// ctx has ended; the work then stops. In a context without a job it does
// nothing and answers nil.
func Commit(ctx context.Context, name string) error {
	t := from(ctx)
	if t == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.r.step(t.j, name, true)
}

// Interrupted answers why the work of a job should stop: its context ended,
// because a cancel was asked for or ESM is shutting down. In a context
// without a job it answers nil: the end of a request does not stop work that
// a request was always run to its end for.
func Interrupted(ctx context.Context) error {
	if from(ctx) == nil {
		return nil
	}
	return ctx.Err()
}

// Progress tells how far the step that runs has come: done of total bytes (a
// total that is not known is 0). Progress is told at most a few times a
// second.
func Progress(ctx context.Context, done, total int64) {
	if t := from(ctx); t != nil {
		t.r.progress(t.j, done, total, -1)
	}
}

// Percent tells how far the step that runs has come, from 0 to 100, when
// that is all that is known.
func Percent(ctx context.Context, p float64) {
	if t := from(ctx); t != nil {
		t.r.progress(t.j, 0, 0, p)
	}
}

func (r *Runner) step(j *job, name string, commit bool) error {
	restricted := r.restricted(j.ident)
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.pub.State != StateRunning {
		return ErrEnded
	}
	if commit {
		if j.asked {
			return ErrCancelled
		}
		j.committed = true
	}
	now := r.now()
	r.endStepLocked(j, now)
	j.pub.Steps = append(j.pub.Steps, StepRecord{Name: name, Started: now})
	j.pub.Step = name
	j.pub.Cancellable = j.cancellableLocked()
	r.tellLocked(j, restricted, now)
	return nil
}

// progress records how far the step that runs has come: pct, when it is not
// negative, or else done of total bytes.
func (r *Runner) progress(j *job, done, total int64, pct float64) {
	r.mu.Lock()
	n := len(j.pub.Steps)
	if j.pub.State != StateRunning || n == 0 {
		r.mu.Unlock()
		return
	}
	st := &j.pub.Steps[n-1]
	switch {
	case pct >= 0:
		st.Percent = &pct
	default:
		st.Bytes, st.Total, st.Percent = done, total, nil
		if total > 0 {
			p := min(100, float64(done)*100/float64(total))
			p = float64(int(p*10)) / 10
			st.Percent = &p
		}
	}
	due := r.now().Sub(j.lastTold) >= progressEvery
	r.mu.Unlock()
	if !due {
		return
	}
	// Told with the lock taken again, so that what is told is the job as it
	// is by then, and in order with the other notices.
	restricted := r.restricted(j.ident)
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.pub.State == StateRunning {
		r.tellLocked(j, restricted, r.now())
	}
}

// Writer wraps w so that what is written to it counts as progress of the step
// that runs: total is the number of bytes expected, 0 when it is not known.
// In a context without a job it returns w itself.
func Writer(ctx context.Context, total int64, w io.Writer) io.Writer {
	t := from(ctx)
	if t == nil {
		return w
	}
	return &progressWriter{w: w, ctx: ctx, total: total}
}

type progressWriter struct {
	w     io.Writer
	ctx   context.Context
	total int64
	done  int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	Progress(p.ctx, p.done, p.total)
	return n, err
}
