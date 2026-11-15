package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- a clock the tests step ---

// fakeClock is a clock that moves only when a test moves it. after
// registers a wait that fires when the clock reaches its time.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []fakeWait
}

type fakeWait struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.t
		return ch
	}
	c.waits = append(c.waits, fakeWait{c.t.Add(d), ch})
	return ch
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waits)
}

// fireDue fires every wait whose time has come.
func (c *fakeClock) fireDue() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = slices.DeleteFunc(c.waits, func(w fakeWait) bool {
		if w.at.After(c.t) {
			return false
		}
		w.ch <- c.t
		return true
	})
}

// stepToNext moves the clock to the earliest wait at or before until and
// reports whether there was one.
func (c *fakeClock) stepToNext(until time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var next time.Time
	for _, w := range c.waits {
		if !w.at.After(until) && (next.IsZero() || w.at.Before(next)) {
			next = w.at
		}
	}
	if next.IsZero() {
		return false
	}
	c.t = next
	return true
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// clocked is a checker on a fake clock.
type clocked struct {
	t   *testing.T
	k   *Checker
	clk *fakeClock
}

func newClocked(t *testing.T, c *Client, every time.Duration) *clocked {
	t.Helper()
	k := NewChecker(c, "v1.0.0", every, true, "")
	clk := newFakeClock()
	k.now, k.after = clk.now, clk.after
	return &clocked{t: t, k: k, clk: clk}
}

// settle waits until no lookup runs and the watcher, if it runs, waits for
// the clock. Only this waits in real time: for the lookup over loopback.
func (c *clocked) settle() {
	c.t.Helper()
	waitFor(c.t, "the checker to settle", func() bool {
		c.k.mu.Lock()
		idle, watching := c.k.running == nil, c.k.watching
		c.k.mu.Unlock()
		return idle && (!watching || c.clk.pending() > 0)
	})
}

// advance moves the clock on by d, waking every wait at its own time and
// letting the checker settle after each.
func (c *clocked) advance(d time.Duration) {
	c.t.Helper()
	c.settle()
	until := c.clk.now().Add(d)
	for c.clk.stepToNext(until) {
		c.clk.fireDue()
		c.settle()
	}
	c.clk.set(until)
}

// jump moves the clock on by d at once and only then wakes what is due: a
// watcher that wakes up late.
func (c *clocked) jump(d time.Duration) {
	c.t.Helper()
	c.settle()
	c.clk.set(c.clk.now().Add(d))
	c.clk.fireDue()
	c.settle()
}

func (c *clocked) watching() bool {
	c.k.mu.Lock()
	defer c.k.mu.Unlock()
	return c.k.watching
}

func (c *clocked) age(st Status) time.Duration { return c.clk.now().Sub(st.Checked) }

// --- a release server on loopback ---

// listServer answers the release list over TLS on loopback, like the
// release server: with tag and, when etag is set, an ETag, and 304 to a
// request whose If-None-Match is that ETag. refuse, when set, answers
// instead. With a gate, every request waits until the gate is closed (or
// the test ends).
type listServer struct {
	mu          sync.Mutex
	tag, etag   string
	refuse      func(w http.ResponseWriter)
	looks       int      // requests
	notModified int      // of them answered 304
	ifNoneMatch []string // If-None-Match of each request
	gate        chan struct{}
}

func newListServer(t *testing.T, tag string, gated bool) (*listServer, *Client) {
	t.Helper()
	ls := &listServer{tag: tag}
	if gated {
		ls.gate = make(chan struct{})
	}
	ended := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.mu.Lock()
		ls.looks++
		ls.ifNoneMatch = append(ls.ifNoneMatch, r.Header.Get("If-None-Match"))
		gate := ls.gate
		ls.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ended:
				return
			case <-r.Context().Done():
				return
			}
		}
		ls.mu.Lock()
		defer ls.mu.Unlock()
		switch {
		case ls.refuse != nil:
			ls.refuse(w)
		case ls.etag != "" && r.Header.Get("If-None-Match") == ls.etag:
			ls.notModified++
			w.WriteHeader(http.StatusNotModified)
		default:
			if ls.etag != "" {
				w.Header().Set("ETag", ls.etag)
			}
			json.NewEncoder(w).Encode(Release{Tag: ls.tag, URL: "https://example.invalid/" + ls.tag})
		}
	}))
	// Cleanups run last first: the waiting requests end before the server
	// closes.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(ended) })
	return ls, &Client{API: srv.URL + "/latest", HTTP: srv.Client()}
}

func (ls *listServer) count() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.looks
}

func (ls *listServer) set(f func(ls *listServer)) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	f(ls)
}

// open lets every waiting request answer.
func (ls *listServer) open() { close(ls.gate) }

// waitFor polls cond until it holds, or fails the test after a long while
// (it waits only for work that runs at once, never for time to pass).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited in vain for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// --- the tests ---

// The service looks for a newer release at most every 5 minutes by
// default. A look at the box before the result is 5 minutes old makes the
// watcher look again when it is, and once more after the last look at the
// box at most; after a pause the first look at the box gets the old
// result, with its time, and starts a lookup.
func TestCheckerLooksAgainAfterFiveMinutes(t *testing.T) {
	if DefaultCheckEvery != 5*time.Minute {
		t.Fatalf("DefaultCheckEvery is %s, want 5m", DefaultCheckEvery)
	}
	ls, c := newListServer(t, "v1.1.0", false)
	ck := newClocked(t, c, 0)
	if ck.k.every != 5*time.Minute {
		t.Fatalf("the default interval is %s", ck.k.every)
	}
	t0 := ck.clk.now()
	if st := ck.k.Status(); !st.Enabled || st.Note != "not checked yet" || !st.Checked.IsZero() {
		t.Fatalf("before the first result: %+v", st)
	}
	ck.settle()
	if st := ck.k.Status(); st.Latest != "v1.1.0" || !st.Available || st.Reused || !st.Checked.Equal(t0) || ls.count() != 1 {
		t.Fatalf("after the first lookup: %+v (%d lookups)", st, ls.count())
	}
	ck.advance(4*time.Minute + 50*time.Second)
	ck.k.Status()
	ck.settle()
	if n := ls.count(); n != 1 {
		t.Fatalf("a result younger than 5 minutes was looked up again (%d lookups)", n)
	}
	ck.advance(20 * time.Second) // 5m10s: the watcher looked at 5m
	if st := ck.k.Status(); ls.count() != 2 || !st.Checked.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("at 5m10s: %+v (%d lookups)", st, ls.count())
	}
	// Nobody looks any more: one more lookup at 10m (a look at the box
	// came after the one at 5m), then the watcher ends.
	ck.advance(30 * time.Minute)
	if n := ls.count(); n != 3 || ck.watching() {
		t.Fatalf("after 30 minutes without a look at the box: %d lookups, watching %v", n, ck.watching())
	}
	st := ck.k.Status()
	if !st.Checked.Equal(t0.Add(10*time.Minute)) || st.Latest != "v1.1.0" {
		t.Fatalf("the first look after the pause: %+v", st)
	}
	ck.settle()
	if st := ck.k.Status(); ls.count() != 4 || !st.Checked.Equal(t0.Add(35*time.Minute+10*time.Second)) {
		t.Fatalf("the look after that: %+v (%d lookups)", st, ls.count())
	}
}

// While the box is looked at, at least once per interval, the result each
// look gets is never older than the interval, also when the looks come
// almost an interval apart (a lookup only when a look at the box finds
// the result due would show one nearly two intervals old). A watcher that
// wakes up late does not end for that. Once nobody looks, at most one more
// lookup is made, then none.
func TestCheckerKeepsTheResultFreshWhileInUse(t *testing.T) {
	const every = 5 * time.Minute
	ls, c := newListServer(t, "v1.1.0", false)
	ck := newClocked(t, c, every)
	ck.k.Status()
	ck.settle()
	for i := range 12 {
		ck.advance(every * 9 / 10)
		st := ck.k.Status()
		if age := ck.age(st); st.Checked.IsZero() || age > every {
			t.Fatalf("look %d: a result %s old, the interval is %s: %+v", i+1, age, every, st)
		}
	}
	// Lookups at 0, 5m, ..., 50m; the 12 looks at the box ended at 54m.
	if n := ls.count(); n != 11 {
		t.Fatalf("%d lookups in 54 minutes of use, want 11", n)
	}
	// The watcher waits for 55m and wakes up 4m50s late: a look at the box
	// came since the last lookup, so it looks and goes on.
	ck.jump(time.Minute + 4*time.Minute + 50*time.Second)
	if n := ls.count(); n != 12 || !ck.watching() {
		t.Fatalf("after a late wake: %d lookups, watching %v", n, ck.watching())
	}
	if st := ck.k.Status(); ck.age(st) != 0 {
		t.Fatalf("after a late wake the result is %s old", ck.age(st))
	}
	// Nobody looks any more. The last look at the box came when that lookup
	// started, so none follows (one would after a look between two
	// lookups), and the watcher ends at the next due time.
	ck.advance(3 * every)
	n := ls.count()
	if n != 12 || ck.watching() {
		t.Fatalf("after 3 intervals without a look at the box: %d lookups, watching %v", n, ck.watching())
	}
	ck.advance(10 * every)
	if m := ls.count(); m != n {
		t.Fatalf("%d lookups after the watcher ended", m-n)
	}
}

// A check by hand while a lookup runs waits for that one, and so does a
// second one: the release server is asked once.
func TestCheckNowWaitsForTheRunningCheck(t *testing.T) {
	ls, c := newListServer(t, "v1.1.0", true)
	ck := newClocked(t, c, 0)
	ck.k.Status() // starts a lookup, which waits at the gate
	waitFor(t, "the lookup", func() bool { return ls.count() == 1 })
	got := make(chan Status, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got <- ck.k.CheckNow(context.Background())
		}()
	}
	waitFor(t, "two checks by hand waiting", func() bool {
		ck.k.mu.Lock()
		defer ck.k.mu.Unlock()
		return ck.k.waiting == 2
	})
	select {
	case st := <-got:
		t.Fatalf("a check by hand returned while the lookup ran: %+v", st)
	default:
	}
	ls.open()
	wg.Wait()
	close(got)
	for st := range got {
		if st.Latest != "v1.1.0" || !st.Available || st.Reused || st.Checked.IsZero() {
			t.Fatalf("a check by hand: %+v", st)
		}
	}
	if n := ls.count(); n != 1 {
		t.Fatalf("%d lookups, want 1", n)
	}
}

// A check by hand looks the release up at once. Another within 60 seconds
// of its end gets the same result without a lookup, and says so; one
// later looks again.
func TestCheckNowReusesAResultYoungerThan60s(t *testing.T) {
	if ReuseWithin != 60*time.Second {
		t.Fatalf("ReuseWithin is %s, want 60s", ReuseWithin)
	}
	ls, c := newListServer(t, "v1.1.0", false)
	ck := newClocked(t, c, time.Hour)
	first := ck.k.CheckNow(context.Background())
	if first.Latest != "v1.1.0" || !first.Available || first.Reused || first.Note != "" || ls.count() != 1 {
		t.Fatalf("the first check by hand: %+v (%d lookups)", first, ls.count())
	}
	again := ck.k.CheckNow(context.Background())
	if !again.Reused || again.Latest != "v1.1.0" || !again.Checked.Equal(first.Checked) ||
		!strings.Contains(again.Note, "without asking the release server") || ls.count() != 1 {
		t.Fatalf("a check by hand right after: %+v (%d lookups)", again, ls.count())
	}
	// The status of the box carries no reuse.
	if st := ck.k.Status(); st.Reused || st.Note != "" {
		t.Fatalf("status after a reuse: %+v", st)
	}
	ck.advance(59 * time.Second)
	if st := ck.k.CheckNow(context.Background()); !st.Reused || !strings.Contains(st.Note, "59 seconds ago") || ls.count() != 1 {
		t.Fatalf("a check by hand 59s after: %+v (%d lookups)", st, ls.count())
	}
	ck.advance(time.Second) // the last lookup ended 60 s ago
	if st := ck.k.CheckNow(context.Background()); st.Reused || st.Note != "" || ls.count() != 2 {
		t.Fatalf("a check by hand 60s after: %+v (%d lookups)", st, ls.count())
	}
}

// A check by hand that gets no answer ends at the lookup's time limit with
// what failed; it does not wait for the release server. This one runs in
// real time, with a short limit and wide margins.
func TestCheckNowIsBounded(t *testing.T) {
	if CheckTimeout != 15*time.Second {
		t.Fatalf("CheckTimeout is %s, want 15s", CheckTimeout)
	}
	ls, c := newListServer(t, "v1.1.0", true) // the gate stays shut
	ck := newClocked(t, c, 0)
	if ck.k.timeout != CheckTimeout {
		t.Fatalf("the checker's time limit is %s", ck.k.timeout)
	}
	ck.k.timeout = 300 * time.Millisecond
	start := time.Now()
	st := ck.k.CheckNow(context.Background())
	if took := time.Since(start); took < 250*time.Millisecond || took > 10*time.Second {
		t.Fatalf("the check took %s, want about 300ms", took)
	}
	if st.Latest != "" || st.Available || st.Checked.IsZero() || !strings.Contains(st.Note, "look up the newest release") {
		t.Fatalf("after no answer: %+v", st)
	}
	if ls.count() != 1 {
		t.Fatalf("%d lookups", ls.count())
	}
	// The caller that goes away is not held either.
	ck.advance(2 * ReuseWithin)
	ck.k.timeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	ck.k.CheckNow(ctx)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("a check by hand outlived its caller by %s", took)
	}
}

// Each lookup after the first sends the ETag of the last answer, and an
// answer 304 Not Modified keeps the release that answer named, with a new
// time. A changed list comes whole, with a new ETag; without an ETag
// nothing is sent.
func TestCheckerAsksWithTheETag(t *testing.T) {
	ls, c := newListServer(t, "v1.1.0", false)
	ls.set(func(ls *listServer) { ls.etag = `"a1"` })
	ck := newClocked(t, c, time.Hour)
	check := func(want string) Status {
		t.Helper()
		ck.advance(2 * ReuseWithin)
		st := ck.k.CheckNow(context.Background())
		if st.Reused || st.Latest != want || !st.Checked.Equal(ck.clk.now()) || st.Note != "" {
			t.Fatalf("a check: %+v, want %s", st, want)
		}
		return st
	}
	check("v1.1.0")
	if st := check("v1.1.0"); !st.Available {
		t.Fatalf("after 304: %+v", st)
	}
	ls.set(func(ls *listServer) { ls.tag, ls.etag = "v1.2.0", `"b2"` })
	check("v1.2.0")
	check("v1.2.0")
	ls.set(func(ls *listServer) { ls.tag, ls.etag = "v1.3.0", "" })
	check("v1.3.0")
	check("v1.3.0")
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if want := []string{"", `"a1"`, `"a1"`, `"b2"`, `"b2"`, ""}; !slices.Equal(ls.ifNoneMatch, want) || ls.notModified != 2 {
		t.Fatalf("If-None-Match sent %q (want %q), %d answered 304", ls.ifNoneMatch, want, ls.notModified)
	}
}

// When the release server refuses a lookup as asked too often (403 with
// X-RateLimit-Remaining 0, or 429 with Retry-After), nothing is looked up
// before the time it names: neither by Status, nor by the watcher, nor by
// a check by hand, which gives the last result and says why. The status
// keeps the release found before and says until when it waits.
func TestCheckerWaitsWhenTheServerSaysSo(t *testing.T) {
	ls, c := newListServer(t, "v1.1.0", false)
	ck := newClocked(t, c, 5*time.Minute)
	ck.k.Status()
	ck.settle()
	reset := ck.clk.now().Add(20 * time.Minute)
	ls.set(func(ls *listServer) {
		ls.refuse = func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		}
	})
	ck.advance(4 * time.Minute)
	ck.k.Status()
	ck.advance(time.Minute) // the watcher looks at 5m and is refused
	if ls.count() != 2 {
		t.Fatalf("%d lookups", ls.count())
	}
	st := ck.k.Status()
	if !st.RetryAfter.Equal(reset) || st.Latest != "v1.1.0" || !st.Available ||
		!strings.Contains(st.Note, "asked to wait until "+reset.Format(time.RFC3339)) || !strings.Contains(st.Note, "403") {
		t.Fatalf("after the refusal: %+v", st)
	}
	// Until the reset: looks at the box and checks by hand, and no lookup.
	for range 7 {
		ck.advance(2 * time.Minute)
		ck.k.Status()
		byHand := ck.k.CheckNow(context.Background())
		if !byHand.Reused || !byHand.RetryAfter.Equal(reset) || !strings.Contains(byHand.Note, "asked to wait") {
			t.Fatalf("a check by hand while waiting: %+v", byHand)
		}
	}
	if n := ls.count(); n != 2 {
		t.Fatalf("%d lookups while the release server asked to wait", n)
	}
	// At the reset the watcher looks again (a look at the box came since).
	ls.set(func(ls *listServer) { ls.refuse = nil })
	ck.advance(reset.Sub(ck.clk.now()))
	if st := ck.k.Status(); ls.count() != 3 || !st.RetryAfter.IsZero() || st.Note != "" || !st.Checked.Equal(reset) {
		t.Fatalf("after the wait: %+v (%d lookups)", st, ls.count())
	}

	// 429 with Retry-After, to a check by hand.
	ls.set(func(ls *listServer) {
		ls.refuse = func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "90")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	ck.advance(2 * ReuseWithin)
	at := ck.clk.now()
	if st := ck.k.CheckNow(context.Background()); st.Reused || !st.RetryAfter.Equal(at.Add(90*time.Second)) || !strings.Contains(st.Note, "429") {
		t.Fatalf("a check by hand refused with 429: %+v", st)
	}
	ck.advance(89 * time.Second)
	if st := ck.k.CheckNow(context.Background()); !st.Reused || ls.count() != 4 {
		t.Fatalf("89s later: %+v (%d lookups)", st, ls.count())
	}
	ls.set(func(ls *listServer) { ls.refuse = nil })
	ck.advance(time.Second)
	if st := ck.k.CheckNow(context.Background()); st.Reused || !st.RetryAfter.IsZero() || ls.count() != 5 {
		t.Fatalf("90s later: %+v (%d lookups)", st, ls.count())
	}
}

// How long a refusal makes ESM wait: what the headers name, at least a
// minute, at most an hour, 15 minutes when they name nothing.
func TestRetryTime(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	for _, c := range []struct {
		name   string
		header map[string]string
		want   time.Duration
	}{
		{"nothing named", nil, 15 * time.Minute},
		{"retry-after seconds", map[string]string{"Retry-After": "120"}, 2 * time.Minute},
		{"retry-after date", map[string]string{"Retry-After": now.Add(10 * time.Minute).Format(http.TimeFormat)}, 10 * time.Minute},
		{"retry-after too short", map[string]string{"Retry-After": "5"}, time.Minute},
		{"retry-after too long", map[string]string{"Retry-After": "7200"}, time.Hour},
		{"retry-after nonsense", map[string]string{"Retry-After": "soon"}, 15 * time.Minute},
		{"reset", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(25 * time.Minute)}, 25 * time.Minute},
		{"reset without remaining", map[string]string{"X-RateLimit-Reset": unix(25 * time.Minute)}, 25 * time.Minute},
		{"reset passed", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(-time.Minute)}, time.Minute},
		{"reset too far", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(3 * time.Hour)}, time.Hour},
		{"reset with requests left", map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": unix(40 * time.Minute)}, 15 * time.Minute},
		{"the later of both", map[string]string{"Retry-After": "300", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": unix(8 * time.Minute)}, 8 * time.Minute},
	} {
		h := http.Header{}
		for k, v := range c.header {
			h.Set(k, v)
		}
		if got := retryTime(h, now); !got.Equal(now.Add(c.want)) {
			t.Errorf("%s: wait %s, want %s", c.name, got.Sub(now), c.want)
		}
	}
	if !rateLimited(http.StatusForbidden) || !rateLimited(http.StatusTooManyRequests) || rateLimited(http.StatusInternalServerError) {
		t.Error("rateLimited")
	}
}
