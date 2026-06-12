// Package dedup provides a small interface for deduplicating Kubernetes
// events and the signals derived from them. In-memory and Redis-backed
// implementations are included; the Redis backend lets dedup state survive
// agent pod restarts and scale beyond a single leader.
package dedup

import (
	"sync"
	"time"
)

// Store is the minimal contract the agent uses to deduplicate work.
//
// Three independent dimensions are tracked:
//   - "seen" keys: unique per event (namespace/name/resourceVersion). Used
//     to skip events already observed on a previous poll.
//   - "signal" keys: the canonical target+reason of an incident. Used to
//     collapse multiple pod-level events onto one deployment-level signal
//     within a TTL window.
//   - "attempt" counters: how many remediation attempts a signal has
//     consumed within a sliding window. Drives the per-signal backoff and
//     the give-up circuit breaker in the poll loop.
//
// TTLs are passed to Mark* so persistent backends (Redis, SQLite) can set
// native key expiration at write time. For signals, every backend records
// the expiry AT MARK TIME — the ttl argument of IsSignalFresh is ignored —
// so callers can mark with an escalated window and check with the base one.
type Store interface {
	// MarkSeen records key if not already present. Returns true when the
	// key was newly added (i.e. the caller should process the event).
	MarkSeen(key string, now time.Time, ttl time.Duration) (fresh bool)

	// IsSignalFresh reports whether signal was marked and its window (fixed
	// when MarkSignal ran) has not yet elapsed. The ttl argument is ignored
	// by every backend and kept only for signature stability.
	IsSignalFresh(signal string, now time.Time, ttl time.Duration) bool

	// MarkSignal stamps signal with an expiry of now+ttl. It is the
	// caller's responsibility to call this only after IsSignalFresh
	// returned false.
	MarkSignal(signal string, now time.Time, ttl time.Duration)

	// Attempts returns the current attempt count for signal, or 0 when the
	// counter is absent or its window has expired.
	Attempts(signal string, now time.Time) int

	// IncrAttempt increments and returns the attempt counter for signal.
	// Each increment slides the expiry window forward by window, so the
	// counter only resets after the incident has been quiet that long.
	IncrAttempt(signal string, now time.Time, window time.Duration) int

	// Evict removes expired entries. Safe to call on every poll; may be a
	// no-op for backends with native TTL.
	Evict(now time.Time, signalTTL, seenTTL time.Duration)
}

// attemptEntry tracks one signal's remediation attempts in memory.
type attemptEntry struct {
	count   int
	expires time.Time
}

// MemoryStore is the default Store backed by maps and a mutex.
// State is lost on process restart.
type MemoryStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	// signalExpiry stores the deadline computed at mark time (now+ttl), so
	// escalated windows survive freshness checks done with the base TTL —
	// mirroring the native key expiration of the Redis backend.
	signalExpiry map[string]time.Time
	attempts     map[string]*attemptEntry
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		seen:         map[string]time.Time{},
		signalExpiry: map[string]time.Time{},
		attempts:     map[string]*attemptEntry{},
	}
}

func (m *MemoryStore) MarkSeen(key string, now time.Time, _ time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[key]; ok {
		return false
	}
	m.seen[key] = now
	return true
}

func (m *MemoryStore) IsSignalFresh(signal string, now time.Time, _ time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	expiry, ok := m.signalExpiry[signal]
	return ok && now.Before(expiry)
}

func (m *MemoryStore) MarkSignal(signal string, now time.Time, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signalExpiry[signal] = now.Add(ttl)
}

func (m *MemoryStore) Attempts(signal string, now time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.attempts[signal]
	if !ok || now.After(e.expires) {
		return 0
	}
	return e.count
}

func (m *MemoryStore) IncrAttempt(signal string, now time.Time, window time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.attempts[signal]
	if !ok || now.After(e.expires) {
		e = &attemptEntry{}
		m.attempts[signal] = e
	}
	e.count++
	e.expires = now.Add(window)
	return e.count
}

func (m *MemoryStore) Evict(now time.Time, _, seenTTL time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sig, expiry := range m.signalExpiry {
		if !now.Before(expiry) {
			delete(m.signalExpiry, sig)
		}
	}
	for key, ts := range m.seen {
		if now.Sub(ts) >= seenTTL {
			delete(m.seen, key)
		}
	}
	for sig, e := range m.attempts {
		if now.After(e.expires) {
			delete(m.attempts, sig)
		}
	}
}
