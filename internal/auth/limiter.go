package auth

import (
	"sync"
	"time"
)

// Limiter counts failures per key (for example "ip:127.0.0.1" or
// "user:admin") inside a sliding window. Once a key has max failures in the
// window, Check refuses with *RateLimitError until the oldest of them ages out.
type Limiter struct {
	window time.Duration
	max    int
	now    func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
}

// NewLimiter makes a limiter allowing max failures per window.
func NewLimiter(window time.Duration, max int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{window: window, max: max, now: now, fails: make(map[string][]time.Time)}
}

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

// Check returns *RateLimitError when any key is over the limit. The
// RetryAfter is the longest wait among the blocked keys.
func (l *Limiter) Check(keys ...string) error {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		ts := l.prune(k, now)
		if len(ts) >= l.max {
			// The attempt becomes possible when enough old failures expire.
			w := ts[len(ts)-l.max].Add(l.window).Sub(now)
			wait = max(wait, w)
		}
	}
	if wait > 0 {
		return &RateLimitError{RetryAfter: wait}
	}
	return nil
}

// Reserve counts one attempt against every key as a failure before the
// attempt is checked, so parallel attempts cannot all slip past the limit
// while a slow password check runs. It refuses with *RateLimitError (and
// counts nothing) when any key is already at the limit. Call release when
// the attempt turns out to succeed or not to count; a failed attempt just
// keeps its reservation.
func (l *Limiter) Reserve(keys ...string) (release func(), err error) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		ts := l.prune(k, now)
		if len(ts) >= l.max {
			wait = max(wait, ts[len(ts)-l.max].Add(l.window).Sub(now))
		}
	}
	if wait > 0 {
		return nil, &RateLimitError{RetryAfter: wait}
	}
	for _, k := range keys {
		l.fails[k] = append(l.fails[k], now)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, k := range keys {
				ts := l.fails[k]
				for i := len(ts) - 1; i >= 0; i-- {
					if ts[i].Equal(now) {
						ts = append(ts[:i], ts[i+1:]...)
						break
					}
				}
				if len(ts) == 0 {
					delete(l.fails, k)
				} else {
					l.fails[k] = ts
				}
			}
		})
	}, nil
}

// Fail records one failure against every key.
func (l *Limiter) Fail(keys ...string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		ts := l.prune(k, now)
		ts = append(ts, now)
		if len(ts) > l.max {
			ts = ts[len(ts)-l.max:]
		}
		l.fails[k] = ts
	}
}

// Reset forgets the failures of every key.
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}
