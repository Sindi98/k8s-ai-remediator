// Package dedup provides a small interface for deduplicating Kubernetes
// events and the signals derived from them. The in-memory implementation
// mirrors the behaviour the agent has always had; separating it behind an
// interface is a stepping stone toward durable backends (Redis, SQLite,
// ConfigMap) so that dedup state survives pod restarts.
package dedup

import (
	"sync"
	"time"
)

// Store is the minimal contract the agent uses to deduplicate work.
//
// Two independent dimensions are tracked:
//   - "seen" keys: unique per event (namespace/name/resourceVersion). Used
//     to skip events already observed on a previous poll.
//   - "signal" keys: the canonical target+reason of an incident. Used to
//     collapse multiple pod-level events onto one deployment-level signal
//     within a TTL window.
//
// Implementations backed by a store with native key expiration (Redis,
// SQLite with WHERE ts>?) may treat Evict as a no-op.
type Store interface {
	// MarkSeen records key if not already present. Returns true when the
	// key was newly added (i.e. the caller should process the event).
	MarkSeen(key string, now time.Time) (fresh bool)

	// IsSignalFresh reports whether signal was marked within ttl of now.
	IsSignalFresh(signal string, now time.Time, ttl time.Duration) bool

	// MarkSignal stamps signal with now. It is the caller's responsibility
	// to call this only after IsSignalFresh returned false.
	MarkSignal(signal string, now time.Time)

	// Evict removes entries older than the respective TTLs. Safe to call
	// on every poll; may be a no-op for backends with native TTL.
	Evict(now time.Time, signalTTL, seenTTL time.Duration)
}

// MemoryStore is the default Store backed by two maps and a mutex.
// State is lost on process restart.
type MemoryStore struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	signalSeen map[string]time.Time
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		seen:       map[string]time.Time{},
		signalSeen: map[string]time.Time{},
	}
}

func (m *MemoryStore) MarkSeen(key string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[key]; ok {
		return false
	}
	m.seen[key] = now
	return true
}

func (m *MemoryStore) IsSignalFresh(signal string, now time.Time, ttl time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.signalSeen[signal]
	return ok && now.Sub(ts) < ttl
}

func (m *MemoryStore) MarkSignal(signal string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signalSeen[signal] = now
}

func (m *MemoryStore) Evict(now time.Time, signalTTL, seenTTL time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for sig, ts := range m.signalSeen {
		if now.Sub(ts) >= signalTTL {
			delete(m.signalSeen, sig)
		}
	}
	for key, ts := range m.seen {
		if now.Sub(ts) >= seenTTL {
			delete(m.seen, key)
		}
	}
}
