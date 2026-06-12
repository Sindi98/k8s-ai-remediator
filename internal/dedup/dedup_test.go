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
	now := time.Now()
	s.seen["old-key"] = now.Add(-2 * time.Hour)
	s.seen["fresh"] = now
	// Signals store their expiry deadline: one already past, one in the future.
	s.signalExpiry["old-signal"] = now.Add(-time.Hour)
	s.signalExpiry["fresh-signal"] = now.Add(time.Hour)
	s.attempts["old-attempt"] = &attemptEntry{count: 3, expires: now.Add(-time.Minute)}
	s.attempts["fresh-attempt"] = &attemptEntry{count: 1, expires: now.Add(time.Hour)}

	s.Evict(now, 5*time.Minute, time.Hour)

	if _, ok := s.seen["old-key"]; ok {
		t.Error("expected old seen entry to be evicted")
	}
	if _, ok := s.signalExpiry["old-signal"]; ok {
		t.Error("expected old signal entry to be evicted")
	}
	if _, ok := s.seen["fresh"]; !ok {
		t.Error("fresh seen entry should survive eviction")
	}
	if _, ok := s.signalExpiry["fresh-signal"]; !ok {
		t.Error("fresh signal entry should survive eviction")
	}
	if _, ok := s.attempts["old-attempt"]; ok {
		t.Error("expected expired attempt counter to be evicted")
	}
	if _, ok := s.attempts["fresh-attempt"]; !ok {
		t.Error("fresh attempt counter should survive eviction")
	}
}

func TestMemoryStore_SignalKeepsEscalatedWindow(t *testing.T) {
	// Marking with an escalated TTL and checking with the base one must
	// honour the escalated window: the expiry is fixed at mark time.
	s := NewMemoryStore()
	now := time.Now()
	base := 5 * time.Minute

	s.MarkSignal("sig", now, 4*base)
	if !s.IsSignalFresh("sig", now.Add(2*base), base) {
		t.Fatal("signal marked with 4x ttl must still be fresh at 2x, regardless of the check ttl")
	}
	if s.IsSignalFresh("sig", now.Add(4*base+time.Second), base) {
		t.Fatal("signal past its escalated window should not be fresh")
	}
}

func TestMemoryStore_Attempts(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	window := time.Hour

	if got := s.Attempts("sig", now); got != 0 {
		t.Fatalf("expected 0 attempts initially, got %d", got)
	}
	if got := s.IncrAttempt("sig", now, window); got != 1 {
		t.Fatalf("expected 1 after first increment, got %d", got)
	}
	if got := s.IncrAttempt("sig", now.Add(time.Minute), window); got != 2 {
		t.Fatalf("expected 2 after second increment, got %d", got)
	}
	if got := s.Attempts("sig", now.Add(2*time.Minute)); got != 2 {
		t.Fatalf("expected Attempts=2 within window, got %d", got)
	}
	// The window slides from the LAST increment.
	if got := s.Attempts("sig", now.Add(time.Minute).Add(window).Add(time.Second)); got != 0 {
		t.Fatalf("expected counter expired past window, got %d", got)
	}
	// And a new increment after expiry restarts from 1.
	if got := s.IncrAttempt("sig", now.Add(time.Minute).Add(window).Add(time.Second), window); got != 1 {
		t.Fatalf("expected counter reset to 1 after expiry, got %d", got)
	}
}

// Compile-time check: MemoryStore satisfies Store.
var _ Store = (*MemoryStore)(nil)
