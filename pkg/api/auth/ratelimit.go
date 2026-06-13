package auth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// windowCount tracks the number of hits in the current fixed window and when
// that window started.
type windowCount struct {
	count int
	start time.Time
}

// RateLimiter is a concurrency-safe fixed-window limiter keyed by string. It is
// intended for throttling login attempts by client key (e.g. IP).
//
// Note: entries in the internal map are not garbage-collected, so keys
// accumulate over the process lifetime. This is acceptable for the current MVP
// (a small set of client IPs) but is not suitable for unbounded key spaces.
type RateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*windowCount
	now    func() time.Time
}

// NewRateLimiter allows up to max events per window per key.
func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		max:    max,
		window: window,
		hits:   make(map[string]*windowCount),
		now:    time.Now,
	}
}

// Allow records an attempt for key and reports whether it is within the limit.
// The window resets once it has elapsed since the first counted hit in it.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	wc := rl.hits[key]
	if wc == nil || now.Sub(wc.start) >= rl.window {
		rl.hits[key] = &windowCount{count: 1, start: now}
		return rl.max >= 1
	}

	wc.count++
	return wc.count <= rl.max
}

// ClientIP returns a best-effort client key for rate limiting: the request's
// RemoteAddr host (with the port stripped). It deliberately does NOT trust
// X-Forwarded-For, since that header is attacker-controlled unless a trusted
// proxy is known to set it.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
