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

	e1, err := b.Publish(TypeState, "alpha", map[string]string{"state": "running"})
	if err != nil {
		t.Fatal(err)
	}
	e2, _ := b.Publish(TypeLog, "beta", "line")
	if e1.ID != 1 || e2.ID != 2 || e1.Time.IsZero() {
		t.Fatalf("ids %d %d", e1.ID, e2.ID)
	}
	if string(e1.Data) != `{"state":"running"}` || string(e2.Data) != `"line"` {
		t.Fatalf("data %s %s", e1.Data, e2.Data)
	}
	if _, err := b.Publish(TypeLog, "x", func() {}); err == nil {
		t.Fatal("unencodable data accepted")
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

// A restricted event reaches a public subscriber in its public form, or
// not at all when it has none: as it is published and when remembered
// events are asked for later. Other subscribers get it whole.
func TestRestrictedEvents(t *testing.T) {
	b := NewBus(16)
	full := b.Subscribe(Filter{}, 8)
	public := b.Subscribe(Filter{Public: true}, 8)

	open, _ := b.Publish(TypeState, "alpha", map[string]string{"reason": "plain"})
	swapped, err := b.PublishRestricted(TypeState, "alpha", map[string]string{"reason": "private words"}, map[string]string{"reason": "notice"})
	if err != nil || !swapped.Restricted() || open.Restricted() || string(swapped.Data) != `{"reason":"private words"}` {
		t.Fatalf("published %+v %v", swapped, err)
	}
	withheld, _ := b.PublishRestricted(TypeLog, "alpha", "private line", nil)
	last, _ := b.Publish(TypeLog, "alpha", "plain line")
	if _, err := b.PublishRestricted(TypeLog, "alpha", "x", func() {}); err == nil {
		t.Fatal("unencodable public data accepted")
	}

	for _, want := range []string{`{"reason":"plain"}`, `{"reason":"private words"}`, `"private line"`, `"plain line"`} {
		if got := recv(t, full); string(got.Data) != want {
			t.Fatalf("a full subscriber got %s, want %s", got.Data, want)
		}
	}
	for _, want := range []struct {
		id   uint64
		data string
	}{{open.ID, `{"reason":"plain"}`}, {swapped.ID, `{"reason":"notice"}`}, {last.ID, `"plain line"`}} {
		if got := recv(t, public); got.ID != want.id || string(got.Data) != want.data {
			t.Fatalf("a public subscriber got %d %s, want %d %s", got.ID, got.Data, want.id, want.data)
		}
	}
	select {
	case e := <-public.C:
		t.Fatalf("a public subscriber got one more event: %+v", e)
	default:
	}

	// The same holds for remembered events, with other filters on top.
	evs, complete := b.Since(0, Filter{Public: true})
	if !complete || len(evs) != 3 || evs[1].ID != swapped.ID || string(evs[1].Data) != `{"reason":"notice"}` {
		t.Fatalf("remembered events for a public subscriber: %+v", evs)
	}
	for _, e := range evs {
		if e.ID == withheld.ID {
			t.Fatal("a withheld event was handed out later")
		}
	}
	if evs, _ := b.Since(0, Filter{Public: true, Types: []string{TypeLog}}); len(evs) != 1 || evs[0].ID != last.ID {
		t.Fatalf("remembered log events for a public subscriber: %+v", evs)
	}
	if evs, _ := b.Since(0, Filter{}); len(evs) != 4 || string(evs[1].Data) != `{"reason":"private words"}` {
		t.Fatalf("remembered events for a full subscriber: %+v", evs)
	}
	backlog, sub, _ := b.SubscribeFrom(swapped.ID-1, Filter{Public: true}, 4)
	defer sub.Close()
	if len(backlog) != 2 || string(backlog[0].Data) != `{"reason":"notice"}` || backlog[1].ID != last.ID {
		t.Fatalf("backlog for a public subscriber: %+v", backlog)
	}
}
