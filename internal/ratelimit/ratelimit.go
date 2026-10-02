// Package ratelimit bounds how much a single anonymous caller can take.
//
// The service hands out lab environments to anyone who asks, with no account and
// no login, so the only things standing between it and exhaustion are these
// limits. They are in-process and best-effort: the addresses are counted in
// memory and reset on restart, which is a deliberate trade against a shared
// store. The limits that matter most — the concurrent-session caps — are
// enforced against the store, where they are durable, and this package only
// meters the request rate on top of them.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter per key, plus a sweep of stale keys so that
// a stream of one-off addresses cannot grow the map without bound.
type Limiter struct {
	mu      sync.Mutex
	count   int
	window  time.Duration
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	count int
	start time.Time
}

// New returns a limiter allowing count requests per window per key. A count of
// zero or less disables it, which is what a deployment that would rather not
// limit sets.
func New(count int, window time.Duration) *Limiter {
	return &Limiter{
		count:   count,
		window:  window,
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// Allow records a request from key and reports whether it is within the limit,
// along with how long until the window resets.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l.count <= 0 {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, seen := l.buckets[key]
	if !seen || now.Sub(b.start) >= l.window {
		l.buckets[key] = &bucket{count: 1, start: now}
		l.sweepLocked(now)
		return true, 0
	}
	if b.count >= l.count {
		return false, l.window - now.Sub(b.start)
	}
	b.count++
	return true, 0
}

// sweepLocked drops buckets whose window has passed. Callers hold the lock.
//
// It runs on the new-bucket path rather than on a timer, so the map is pruned
// exactly when it is growing and there is no goroutine to own.
func (l *Limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.start) >= l.window {
			delete(l.buckets, k)
		}
	}
}
