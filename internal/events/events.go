// Package events is ESM's in-memory publish/subscribe bus.
//
// Every event gets a monotonic id. The newest events are kept in a ring
// buffer so a client that reconnects with Last-Event-ID can catch up.
// Subscribers that fall too far behind are closed (Lagged reports it); they
// reconnect and resume from the ring.
package events

import (
	"slices"
	"sync"
	"time"
)

// Event types.
const (
	TypeState = "state"
	TypeLog   = "log"
	TypeAudit = "audit"
	TypeBuild = "build"
)

// Event is one published event.
type Event struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Type     string    `json:"type"`
	Instance string    `json:"instance,omitempty"`
	Data     any       `json:"data,omitempty"`
}

// Filter selects events. Empty fields match everything.
type Filter struct {
	Instance string
	Types    []string
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if f.Instance != "" && e.Instance != f.Instance {
		return false
	}
	if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
		return false
	}
	return true
}

// Bus is the event bus. The zero value is not usable; call NewBus.
type Bus struct {
	mu     sync.Mutex
	nextID uint64
	ring   []Event // circular, len <= size
	start  int     // index of the oldest event in ring
	size   int
	subs   map[*Subscription]struct{}
	closed bool
	now    func() time.Time
}

// NewBus makes a bus that remembers the last ringSize events.
func NewBus(ringSize int) *Bus {
	if ringSize < 1 {
		ringSize = 1
	}
	return &Bus{nextID: 1, size: ringSize, subs: make(map[*Subscription]struct{}), now: time.Now}
}

// Subscription receives matching events on C until it is closed.
type Subscription struct {
	C <-chan Event

	c      chan Event
	bus    *Bus
	filter Filter
	lagged bool
	once   sync.Once
}

// Close stops the subscription and closes C. Safe to call more than once.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.closeChan()
}

func (s *Subscription) closeChan() { s.once.Do(func() { close(s.c) }) }

// Lagged reports whether the bus closed the subscription because its buffer
// was full.
func (s *Subscription) Lagged() bool {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.lagged
}

// Publish records and delivers an event and returns it with its id and time.
func (b *Bus) Publish(typ, instance string, data any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := Event{ID: b.nextID, Time: b.now().UTC(), Type: typ, Instance: instance, Data: data}
	b.nextID++
	if len(b.ring) < b.size {
		b.ring = append(b.ring, e)
	} else {
		b.ring[b.start] = e
		b.start = (b.start + 1) % b.size
	}
	if b.closed {
		return e
	}
	for s := range b.subs {
		if !s.filter.Match(e) {
			continue
		}
		select {
		case s.c <- e:
		default:
			s.lagged = true
			delete(b.subs, s)
			s.closeChan()
		}
	}
	return e
}

// LastID returns the id of the newest event (0 when none).
func (b *Bus) LastID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID - 1
}

// since returns ring events with id > after that match f, oldest first, and
// whether the ring still holds every event after that id.
func (b *Bus) since(after uint64, f Filter) ([]Event, bool) {
	if after >= b.nextID {
		// An id from before a restart: start from the beginning.
		after = 0
	}
	var out []Event
	complete := true
	if len(b.ring) > 0 {
		oldest := b.ring[b.start].ID
		if after+1 < oldest {
			complete = false
		}
	}
	for i := range b.ring {
		e := b.ring[(b.start+i)%len(b.ring)]
		if e.ID > after && f.Match(e) {
			out = append(out, e)
		}
	}
	return out, complete
}

// Since returns remembered events after id `after` that match f. complete
// is false when some events after that id have already left the ring.
func (b *Bus) Since(after uint64, f Filter) (events []Event, complete bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.since(after, f)
}

// Subscribe delivers future events matching f. buffer is the channel size.
func (b *Bus) Subscribe(f Filter, buffer int) *Subscription {
	_, s, _ := b.SubscribeFrom(b.LastID(), f, buffer)
	return s
}

// SubscribeFrom returns the remembered events after `after` and a
// subscription for everything newer, with no gap and no duplicate between
// the two. complete is as for Since.
func (b *Bus) SubscribeFrom(after uint64, f Filter, buffer int) (backlog []Event, sub *Subscription, complete bool) {
	if buffer < 1 {
		buffer = 1
	}
	c := make(chan Event, buffer)
	s := &Subscription{C: c, c: c, bus: b, filter: f}
	b.mu.Lock()
	defer b.mu.Unlock()
	backlog, complete = b.since(after, f)
	if b.closed {
		s.closeChan()
		return backlog, s, complete
	}
	b.subs[s] = struct{}{}
	return backlog, s, complete
}

// Close closes every subscription; later Publish calls are still recorded
// but delivered to nobody.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		delete(b.subs, s)
		s.closeChan()
	}
}
