package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// Bounds of the limiter's memory.
const (
	// maxKeyLen is the longest key kept as it is; a longer key is kept as
	// its SHA-256.
	maxKeyLen = 128
	// defaultMaxKeys is the most keys the limiter holds. When it is full,
	// keys whose failures have aged out go first, then the key whose newest
	// failure is the oldest.
	defaultMaxKeys = 1 << 16
)

// Limiter counts failures per key (for example "ip:192.0.2.1" or a user
// name's key) inside a sliding window. Once a key has max failures in the
// window, Check refuses with *RateLimitError until the oldest of them ages
// out. Keys whose failures have all aged out are forgotten (at the latest
// one window later), long keys are kept as a hash, and the number of keys is
// bounded, so no stream of made-up names makes it grow without end.
type Limiter struct {
	window time.Duration
	max    int
	now    func() time.Time

	mu      sync.Mutex
	fails   map[string][]time.Time
	maxKeys int
	swept   time.Time // when every key was last pruned
}

// NewLimiter makes a limiter allowing max failures per window.
func NewLimiter(window time.Duration, max int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{window: window, max: max, now: now, fails: make(map[string][]time.Time), maxKeys: defaultMaxKeys}
}

// key is the map key for k: k itself, or its hash when k is long.
func key(k string) string {
	if len(k) <= maxKeyLen {
		return k
	}
	sum := sha256.Sum256([]byte(k))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hashed maps keys to map keys.
func hashed(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = key(k)
	}
	return out
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

// sweepLocked forgets every key whose failures have all aged out. Unless
// forced it runs at most once a window, so its cost is spread thin.
func (l *Limiter) sweepLocked(now time.Time, force bool) {
	if !force && now.Sub(l.swept) < l.window {
		return
	}
	l.swept = now
	for k := range l.fails {
		l.prune(k, now)
	}
}

// addLocked records a failure at t against k, making room for a new key
// when the limiter is full.
func (l *Limiter) addLocked(k string, t time.Time) {
	if _, ok := l.fails[k]; !ok && len(l.fails) >= l.maxKeys {
		l.sweepLocked(t, true)
		for len(l.fails) >= l.maxKeys {
			oldest, at := "", time.Time{}
			for k2, ts := range l.fails {
				if last := ts[len(ts)-1]; oldest == "" || last.Before(at) {
					oldest, at = k2, last
				}
			}
			delete(l.fails, oldest)
		}
	}
	l.fails[k] = append(l.fails[k], t)
}

// Check returns *RateLimitError when any key is over the limit. The
// RetryAfter is the longest wait among the blocked keys.
func (l *Limiter) Check(keys ...string) error {
	keys = hashed(keys)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now, false)
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
	keys = hashed(keys)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now, false)
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
		l.addLocked(k, now)
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
	keys = hashed(keys)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now, false)
	for _, k := range keys {
		l.prune(k, now)
		l.addLocked(k, now)
		if ts := l.fails[k]; len(ts) > l.max {
			l.fails[k] = ts[len(ts)-l.max:]
		}
	}
}

// Reset forgets the failures of every key.
func (l *Limiter) Reset(keys ...string) {
	keys = hashed(keys)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}
