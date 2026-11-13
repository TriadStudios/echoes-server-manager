// Package events is ESM's in-memory publish/subscribe bus.
//
// Every event gets a monotonic id. The newest events are kept in a ring
// buffer so a client that reconnects with Last-Event-ID can catch up.
// Subscribers that fall too far behind are closed (Lagged reports it); they
// reconnect and resume from the ring.
package events

import (
	"encoding/json"
	"fmt"
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
	// TypeUpdate is a step of an instance's update to a newer build that
	// ESM found by itself (available, started, updated, failed, overdue).
	TypeUpdate = "update"
)

// Event is one published event. Data is the JSON encoding of the payload,
// made once at publish time, so every subscriber gets the same bytes and
// nothing a publisher changes later can leak into it.
type Event struct {
	ID       uint64          `json:"id"`
	Time     time.Time       `json:"time"`
	Type     string          `json:"type"`
	Instance string          `json:"instance,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`

	// restricted marks an event whose Data not every subscriber may see
	// (see PublishRestricted). public is what the others get in its place;
	// nil means they do not get the event at all. Both are decided when
	// the event is published and stay with it for as long as it is
	// remembered.
	restricted bool
	public     json.RawMessage
}

// Restricted reports whether e was published with PublishRestricted.
func (e Event) Restricted() bool { return e.restricted }

// Filter selects events. Empty fields match everything.
type Filter struct {
	Instance string
	Types    []string
	// Public marks a subscriber that may not see restricted data: it gets
	// the public form of a restricted event, or nothing when there is none.
	Public bool
}

// Match reports whether e passes the filter's instance and types.
func (f Filter) Match(e Event) bool {
	if f.Instance != "" && e.Instance != f.Instance {
		return false
	}
	if len(f.Types) > 0 && !slices.Contains(f.Types, e.Type) {
		return false
	}
	return true
}

// view returns e as a subscriber with this filter gets it, and false when
// it does not get it.
func (f Filter) view(e Event) (Event, bool) {
	if !f.Match(e) {
		return Event{}, false
	}
	if f.Public && e.restricted {
		if e.public == nil {
			return Event{}, false
		}
		e.Data = e.public
	}
	return e, true
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

// Publish encodes data as JSON (nil means no data), records and delivers
// the event, and returns it with its id and time.
func (b *Bus) Publish(typ, instance string, data any) (Event, error) {
	raw, err := encode(typ, data)
	if err != nil {
		return Event{}, err
	}
	return b.publish(Event{Type: typ, Instance: instance, Data: raw}), nil
}

// PublishRestricted is Publish for an event whose data not every
// subscriber may see. Subscribers whose filter has Public set get public in
// its place, or, when public is nil, do not get the event at all: not now
// and not when they ask for remembered events later.
func (b *Bus) PublishRestricted(typ, instance string, data, public any) (Event, error) {
	raw, err := encode(typ, data)
	if err != nil {
		return Event{}, err
	}
	pub, err := encode(typ, public)
	if err != nil {
		return Event{}, err
	}
	return b.publish(Event{Type: typ, Instance: instance, Data: raw, restricted: true, public: pub}), nil
}

func encode(typ string, data any) (json.RawMessage, error) {
	if data == nil {
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("events: encode %s data: %w", typ, err)
	}
	return raw, nil
}

func (b *Bus) publish(e Event) Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.ID, e.Time = b.nextID, b.now().UTC()
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
		seen, ok := s.filter.view(e)
		if !ok {
			continue
		}
		select {
		case s.c <- seen:
		default:
			s.lagged = true
			delete(b.subs, s)
			s.closeChan()
		}
	}
	return e
}

// Subscribers returns how many subscriptions are open.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
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
		if e.ID <= after {
			continue
		}
		if seen, ok := f.view(e); ok {
			out = append(out, seen)
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
