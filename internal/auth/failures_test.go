package auth

import (
	"testing"
	"time"
)

// Failures counts the failures of a key inside the window, at most max.
func TestLimiterFailures(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	l := NewLimiter(time.Minute, 3, func() time.Time { return now })
	if n := l.Failures("ip:192.0.2.1"); n != 0 {
		t.Fatalf("no failures: %d", n)
	}
	for i := range 5 {
		l.Fail("ip:192.0.2.1")
		if n, want := l.Failures("ip:192.0.2.1"), min(i+1, 3); n != want {
			t.Fatalf("after %d failures: %d, want %d", i+1, n, want)
		}
	}
	if n := l.Failures("ip:192.0.2.2"); n != 0 {
		t.Fatalf("another key: %d", n)
	}
	now = now.Add(time.Minute)
	if n := l.Failures("ip:192.0.2.1"); n != 0 {
		t.Fatalf("after the window: %d", n)
	}
}
