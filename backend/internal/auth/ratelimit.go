package auth

import (
	"sync"
	"time"
)

// Limiter is a fixed-window in-memory rate limiter keyed by arbitrary strings
// (usually "purpose:ip").
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*window
	limit   int
	per     time.Duration
}

type window struct {
	count int
	start time.Time
}

// NewLimiter allows `limit` events per rolling fixed window of duration per.
func NewLimiter(limit int, per time.Duration) *Limiter {
	return &Limiter{entries: map[string]*window{}, limit: limit, per: per}
}

// Allow consumes one event for key; it reports whether the event is allowed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.entries[key]
	if !ok || now.Sub(w.start) >= l.per {
		l.entries[key] = &window{count: 1, start: now}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// RetryAfter returns how long until the key's window resets.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.entries[key]
	if !ok {
		return 0
	}
	remaining := l.per - time.Since(w.start)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// GC drops stale windows; call periodically from a background goroutine.
func (l *Limiter) GC() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, w := range l.entries {
		if time.Since(w.start) >= l.per {
			delete(l.entries, k)
		}
	}
}
