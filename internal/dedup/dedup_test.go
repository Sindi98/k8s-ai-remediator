package dedup

import (
	"testing"
	"time"
)

func TestMemoryStore_MarkSeenIsIdempotent(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()

	if fresh := s.MarkSeen("k", now, time.Hour); !fresh {
		t.Fatal("first MarkSeen should return fresh=true")
	}
	if fresh := s.MarkSeen("k", now, time.Hour); fresh {
		t.Fatal("second MarkSeen should return fresh=false")
	}
}

func TestMemoryStore_SignalFreshness(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	ttl := 5 * time.Minute

	if s.IsSignalFresh("sig", now, ttl) {
		t.Fatal("unseen signal should not be fresh")
	}
	s.MarkSignal("sig", now, ttl)
	if !s.IsSignalFresh("sig", now.Add(ttl/2), ttl) {
		t.Fatal("signal within ttl should be fresh")
	}
	if s.IsSignalFresh("sig", now.Add(ttl+time.Second), ttl) {
		t.Fatal("signal past ttl should not be fresh")
	}
}

func TestMemoryStore_EvictsExpiredEntries(t *testing.T) {
	s := NewMemoryStore()
	old := time.Now().Add(-2 * time.Hour)
	now := time.Now()
	s.seen["old-key"] = old
	s.signalSeen["old-signal"] = old
	s.seen["fresh"] = now
	s.signalSeen["fresh-signal"] = now

	s.Evict(now, 5*time.Minute, time.Hour)

	if _, ok := s.seen["old-key"]; ok {
		t.Error("expected old seen entry to be evicted")
	}
	if _, ok := s.signalSeen["old-signal"]; ok {
		t.Error("expected old signal entry to be evicted")
	}
	if _, ok := s.seen["fresh"]; !ok {
		t.Error("fresh seen entry should survive eviction")
	}
	if _, ok := s.signalSeen["fresh-signal"]; !ok {
		t.Error("fresh signal entry should survive eviction")
	}
}

// Compile-time check: MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)
