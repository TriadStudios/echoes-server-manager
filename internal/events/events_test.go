package events

import (
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case e, ok := <-s.C:
		if !ok {
			t.Fatal("subscription closed")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestPublishSubscribeFilter(t *testing.T) {
	b := NewBus(16)
	all := b.Subscribe(Filter{}, 8)
	alpha := b.Subscribe(Filter{Instance: "alpha"}, 8)
	states := b.Subscribe(Filter{Types: []string{TypeState}}, 8)

	e1 := b.Publish(TypeState, "alpha", map[string]string{"state": "running"})
	e2 := b.Publish(TypeLog, "beta", "line")
	if e1.ID != 1 || e2.ID != 2 || e1.Time.IsZero() {
		t.Fatalf("ids %d %d", e1.ID, e2.ID)
	}
	if got := recv(t, all); got.ID != 1 {
		t.Fatalf("all got %d", got.ID)
	}
	if got := recv(t, all); got.ID != 2 {
		t.Fatalf("all got %d", got.ID)
	}
	if got := recv(t, alpha); got.ID != 1 {
		t.Fatalf("alpha got %d", got.ID)
	}
	if got := recv(t, states); got.ID != 1 || got.Type != TypeState {
		t.Fatalf("states got %+v", got)
	}
	select {
	case e := <-alpha.C:
		t.Fatalf("alpha got unexpected %+v", e)
	case e := <-states.C:
		t.Fatalf("states got unexpected %+v", e)
	default:
	}
	alpha.Close()
	alpha.Close() // idempotent
	if _, ok := <-alpha.C; ok {
		t.Fatal("closed subscription still open")
	}
	b.Publish(TypeState, "alpha", nil) // must not panic on closed sub
}

func TestRingBufferResume(t *testing.T) {
	b := NewBus(5)
	for range 8 {
		b.Publish(TypeLog, "a", nil)
	}
	if b.LastID() != 8 {
		t.Fatalf("last id %d", b.LastID())
	}
	evs, complete := b.Since(5, Filter{})
	if !complete || len(evs) != 3 || evs[0].ID != 6 || evs[2].ID != 8 {
		t.Fatalf("since 5: complete=%v %v", complete, ids(evs))
	}
	evs, complete = b.Since(3, Filter{})
	if !complete || len(evs) != 5 || evs[0].ID != 4 {
		t.Fatalf("since 3: complete=%v %v", complete, ids(evs))
	}
	evs, complete = b.Since(1, Filter{})
	if complete || len(evs) != 5 {
		t.Fatalf("since 1 should be incomplete: complete=%v %v", complete, ids(evs))
	}
	evs, _ = b.Since(8, Filter{})
	if len(evs) != 0 {
		t.Fatalf("since last: %v", ids(evs))
	}
	// An id newer than anything here comes from before a restart.
	evs, complete = b.Since(900, Filter{})
	if complete || len(evs) != 5 {
		t.Fatalf("since unknown id: complete=%v %v", complete, ids(evs))
	}
}

func TestSubscribeFromHasNoGap(t *testing.T) {
	b := NewBus(1000)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			b.Publish(TypeLog, "a", nil)
		}
	}()
	time.Sleep(time.Millisecond)
	backlog, sub, complete := b.SubscribeFrom(0, Filter{}, 1000)
	wg.Wait()
	if !complete {
		t.Fatal("incomplete")
	}
	seen := ids(backlog)
	for len(seen) < 500 {
		seen = append(seen, recv(t, sub).ID)
	}
	for i, id := range seen {
		if id != uint64(i+1) {
			t.Fatalf("gap or duplicate at %d: %v", i, seen[max(0, i-3):i+1])
		}
	}
	sub.Close()
}

func TestSlowSubscriberIsClosed(t *testing.T) {
	b := NewBus(16)
	slow := b.Subscribe(Filter{}, 2)
	fast := b.Subscribe(Filter{}, 100)
	for range 5 {
		b.Publish(TypeLog, "", nil)
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != 2 || !slow.Lagged() {
		t.Fatalf("slow got %d events, lagged=%v", n, slow.Lagged())
	}
	if fast.Lagged() {
		t.Fatal("fast subscriber marked lagged")
	}
	for i := range 5 {
		if e := recv(t, fast); e.ID != uint64(i+1) {
			t.Fatalf("fast got %d", e.ID)
		}
	}
}

func TestCloseBus(t *testing.T) {
	b := NewBus(4)
	s := b.Subscribe(Filter{}, 4)
	b.Close()
	if _, ok := <-s.C; ok {
		t.Fatal("subscription open after bus close")
	}
	b.Publish(TypeAudit, "", nil)
	if evs, _ := b.Since(0, Filter{}); len(evs) != 1 {
		t.Fatal("event not recorded after close")
	}
	_, s2, _ := b.SubscribeFrom(0, Filter{}, 1)
	if _, ok := <-s2.C; ok {
		t.Fatal("subscription on closed bus is open")
	}
}

func ids(evs []Event) []uint64 {
	var out []uint64
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}
