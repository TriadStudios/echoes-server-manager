package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// latestServer answers the release list over TLS on loopback with tag and
// counts the lookups. With a gate, every lookup waits until the gate is
// closed (or the test ends).
type latestServer struct {
	looks atomic.Int32
	gate  chan struct{}
}

func newLatestServer(t *testing.T, tag string, gated bool) (*latestServer, *Client) {
	t.Helper()
	ls := &latestServer{}
	if gated {
		ls.gate = make(chan struct{})
	}
	ended := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.looks.Add(1)
		if ls.gate != nil {
			select {
			case <-ls.gate:
			case <-ended:
				return
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(Release{Tag: tag, URL: "https://example.invalid/" + tag})
	}))
	// Cleanups run last first: the waiting lookups end before the server
	// closes.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(ended) })
	return ls, &Client{API: srv.URL + "/latest", HTTP: srv.Client()}
}

// open lets every waiting lookup answer.
func (ls *latestServer) open() { close(ls.gate) }

// waitFor polls cond until it holds, or fails the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited in vain for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// idle reports whether no check runs.
func (k *Checker) idle() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.running == nil
}

// age moves the times of the last result back by d, as if it were d older.
func (k *Checker) age(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.status.Checked = k.status.Checked.Add(-d)
	k.finished = k.finished.Add(-d)
}

// The service looks for a newer release at most every 5 minutes by
// default: a result younger than that is kept, an older one makes the next
// Status look again, in the background.
func TestCheckerLooksAgainAfterFiveMinutes(t *testing.T) {
	if DefaultCheckEvery != 5*time.Minute {
		t.Fatalf("DefaultCheckEvery is %s, want 5m", DefaultCheckEvery)
	}
	ls, c := newLatestServer(t, "v1.1.0", false)
	k := NewChecker(c, "v1.0.0", 0, true, "")
	if k.every != 5*time.Minute {
		t.Fatalf("the default interval is %s", k.every)
	}
	if st := k.Status(); !st.Enabled || st.Note != "not checked yet" || !st.Checked.IsZero() {
		t.Fatalf("before the first result: %+v", st)
	}
	waitFor(t, "the first check", k.idle)
	if st := k.Status(); st.Latest != "v1.1.0" || !st.Available || st.Reused {
		t.Fatalf("after the first check: %+v", st)
	}
	k.age(4*time.Minute + 50*time.Second)
	k.Status()
	if !k.idle() || ls.looks.Load() != 1 {
		t.Fatalf("a result younger than 5 minutes was looked up again (%d lookups)", ls.looks.Load())
	}
	k.age(20 * time.Second) // now 5m10s old
	k.Status()
	waitFor(t, "a second lookup", func() bool { return ls.looks.Load() == 2 })
	waitFor(t, "the second check", k.idle)
	if st := k.Status(); time.Since(st.Checked) > time.Minute || st.Latest != "v1.1.0" {
		t.Fatalf("after the second check: %+v", st)
	}
}

// While the box is looked at, at least once per interval, the result each
// look gets is never older than the interval plus one check, also when the
// looks come almost an interval apart (a check only when a look finds the
// result due would show one nearly two intervals old). Once nobody looks
// for an interval, nothing more is looked up.
func TestCheckerKeepsTheResultFreshWhileInUse(t *testing.T) {
	const every = 500 * time.Millisecond
	ls, c := newLatestServer(t, "v1.1.0", false)
	k := NewChecker(c, "v1.0.0", every, true, "")
	k.Status()
	waitFor(t, "the first check", k.idle)
	var worst time.Duration
	for range 7 {
		time.Sleep(every * 9 / 10)
		st := k.Status()
		if st.Checked.IsZero() {
			t.Fatalf("no result: %+v", st)
		}
		worst = max(worst, time.Since(st.Checked))
	}
	if worst > every+300*time.Millisecond {
		t.Fatalf("a look got a result %s old; the interval is %s", worst, every)
	}
	if n := ls.looks.Load(); n < 5 {
		t.Fatalf("%d lookups in %s of use, every %s", n, 7*every*9/10, every)
	}
	// Nobody looks any more: at most one more lookup, then none.
	time.Sleep(every*5/2 + 100*time.Millisecond)
	n := ls.looks.Load()
	k.mu.Lock()
	watching := k.watching
	k.mu.Unlock()
	if watching {
		t.Fatal("the watcher still runs after an interval without a look")
	}
	time.Sleep(every * 3 / 2)
	if m := ls.looks.Load(); m != n {
		t.Fatalf("%d lookups after nobody looked any more", m-n)
	}
}

// A check by hand while a check runs waits for that one, and so does a
// second one: the release server is asked once.
func TestCheckNowWaitsForTheRunningCheck(t *testing.T) {
	ls, c := newLatestServer(t, "v1.1.0", true)
	k := NewChecker(c, "v1.0.0", 0, true, "")
	k.Status() // starts a check in the background, which waits at the gate
	waitFor(t, "the background lookup", func() bool { return ls.looks.Load() == 1 })
	got := make(chan Status, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got <- k.CheckNow(context.Background())
		}()
	}
	select {
	case st := <-got:
		t.Fatalf("a check by hand returned while the check ran: %+v", st)
	case <-time.After(200 * time.Millisecond):
	}
	ls.open()
	wg.Wait()
	close(got)
	for st := range got {
		if st.Latest != "v1.1.0" || !st.Available || st.Reused || st.Checked.IsZero() {
			t.Fatalf("a check by hand: %+v", st)
		}
	}
	if n := ls.looks.Load(); n != 1 {
		t.Fatalf("%d lookups, want 1", n)
	}
}

// A check by hand looks the release up at once. Another within 30 seconds
// of its end gets the same result without a lookup, and says so; one
// later looks again.
func TestCheckNowReusesAResultYoungerThan30s(t *testing.T) {
	if ReuseWithin != 30*time.Second {
		t.Fatalf("ReuseWithin is %s, want 30s", ReuseWithin)
	}
	ls, c := newLatestServer(t, "v1.1.0", false)
	k := NewChecker(c, "v1.0.0", time.Hour, true, "")
	first := k.CheckNow(context.Background())
	if first.Latest != "v1.1.0" || !first.Available || first.Reused || first.Note != "" || ls.looks.Load() != 1 {
		t.Fatalf("the first check by hand: %+v (%d lookups)", first, ls.looks.Load())
	}
	again := k.CheckNow(context.Background())
	if !again.Reused || again.Latest != "v1.1.0" || !again.Checked.Equal(first.Checked) ||
		!strings.Contains(again.Note, "without asking the release server") || ls.looks.Load() != 1 {
		t.Fatalf("a check by hand right after: %+v (%d lookups)", again, ls.looks.Load())
	}
	// The status of the box carries no reuse.
	if st := k.Status(); st.Reused || st.Note != "" {
		t.Fatalf("status after a reuse: %+v", st)
	}
	k.age(25 * time.Second)
	if st := k.CheckNow(context.Background()); !st.Reused || ls.looks.Load() != 1 {
		t.Fatalf("a check by hand 25s after: %+v (%d lookups)", st, ls.looks.Load())
	}
	k.age(6 * time.Second) // the last check ended 31 s ago
	if st := k.CheckNow(context.Background()); st.Reused || st.Note != "" || ls.looks.Load() != 2 {
		t.Fatalf("a check by hand 31s after: %+v (%d lookups)", st, ls.looks.Load())
	}
}

// A check by hand that gets no answer ends at the check's time limit with
// what failed; it does not wait for the release server.
func TestCheckNowIsBounded(t *testing.T) {
	if CheckTimeout != 15*time.Second {
		t.Fatalf("CheckTimeout is %s, want 15s", CheckTimeout)
	}
	ls, c := newLatestServer(t, "v1.1.0", true) // the gate stays shut
	k := NewChecker(c, "v1.0.0", 0, true, "")
	if k.timeout != CheckTimeout {
		t.Fatalf("the checker's time limit is %s", k.timeout)
	}
	k.timeout = 300 * time.Millisecond
	start := time.Now()
	st := k.CheckNow(context.Background())
	took := time.Since(start)
	if took < 250*time.Millisecond || took > 5*time.Second {
		t.Fatalf("the check took %s, want about 300ms", took)
	}
	if st.Latest != "" || st.Available || st.Checked.IsZero() || !strings.Contains(st.Note, "look up the newest release") {
		t.Fatalf("after no answer: %+v", st)
	}
	if ls.looks.Load() != 1 || !k.idle() {
		t.Fatalf("%d lookups, idle %v", ls.looks.Load(), k.idle())
	}
	// The caller that goes away is not held either.
	k.age(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	k.timeout = 2 * time.Second
	k.CheckNow(ctx)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a check by hand outlived its caller by %s", took)
	}
}
