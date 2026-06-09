package webui

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// maxThrottleEntries caps the throttle map so a distributed attack cannot grow
// it without bound; expired entries are pruned opportunistically once exceeded.
const maxThrottleEntries = 4096

// loginThrottle is a small fixed-window limiter that locks out a client after
// too many failed credential checks, slowing brute-force attempts against the
// admin GUI. It is keyed by client IP; behind a shared reverse proxy that
// effectively throttles per-proxy, an acceptable trade-off for a control plane
// (front the GUI with TLS and a strong password regardless).
type loginThrottle struct {
	mu      sync.Mutex
	entries map[string]*throttleEntry
	max     int           // failures within window before lockout
	window  time.Duration // window over which failures accumulate
	lockout time.Duration // how long a locked-out key stays blocked
	now     func() time.Time
}

type throttleEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

// newLoginThrottle returns a throttle that locks out a key for 15 minutes
// after 5 failed attempts within a 15-minute window.
func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		entries: map[string]*throttleEntry{},
		max:     5,
		window:  15 * time.Minute,
		lockout: 15 * time.Minute,
		now:     time.Now,
	}
}

// allowed reports whether key may attempt a credential check now (i.e. it is
// not currently locked out).
func (t *loginThrottle) allowed(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[key]
	if e == nil {
		return true
	}
	return !t.now().Before(e.lockedUntil)
}

// recordFailure registers a failed attempt and locks the key once it reaches
// the threshold within the window.
func (t *loginThrottle) recordFailure(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if len(t.entries) > maxThrottleEntries {
		t.pruneLocked(now)
	}
	e := t.entries[key]
	if e == nil || now.Sub(e.windowStart) > t.window {
		e = &throttleEntry{windowStart: now}
		t.entries[key] = e
	}
	e.failures++
	if e.failures >= t.max {
		e.lockedUntil = now.Add(t.lockout)
	}
}

// recordSuccess clears any failure state for key.
func (t *loginThrottle) recordSuccess(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// pruneLocked drops entries that are neither locked nor within their failure
// window. Caller must hold t.mu.
func (t *loginThrottle) pruneLocked(now time.Time) {
	for k, e := range t.entries {
		if now.After(e.lockedUntil) && now.Sub(e.windowStart) > t.window {
			delete(t.entries, k)
		}
	}
}

// clientIP extracts the client IP from RemoteAddr for throttle keying. We use
// the direct peer address rather than X-Forwarded-For (which is client
// spoofable); behind a reverse proxy this keys per-proxy, which still throttles
// the attack surface.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
