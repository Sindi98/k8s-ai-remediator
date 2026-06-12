package dedup

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newTestRedisStore(t *testing.T) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	store, err := NewRedisStore(RedisConfig{
		Addr:      mr.Addr(),
		KeyPrefix: "test:",
	})
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, mr
}

func TestRedisStore_MarkSeenAtomicity(t *testing.T) {
	s, _ := newTestRedisStore(t)
	now := time.Now()

	if fresh := s.MarkSeen("evt1", now, time.Hour); !fresh {
		t.Fatal("first MarkSeen should return fresh=true")
	}
	if fresh := s.MarkSeen("evt1", now, time.Hour); fresh {
		t.Fatal("second MarkSeen on same key should return fresh=false")
	}
	if fresh := s.MarkSeen("evt2", now, time.Hour); !fresh {
		t.Fatal("distinct key should return fresh=true")
	}
}

func TestRedisStore_MarkSeenTTL(t *testing.T) {
	s, mr := newTestRedisStore(t)
	now := time.Now()

	s.MarkSeen("evt-ttl", now, 30*time.Second)
	if !mr.Exists("test:seen:evt-ttl") {
		t.Fatal("expected key to exist after MarkSeen")
	}
	mr.FastForward(31 * time.Second)
	if mr.Exists("test:seen:evt-ttl") {
		t.Fatal("expected key to expire past TTL")
	}
	// After expiration the next MarkSeen is fresh again.
	if fresh := s.MarkSeen("evt-ttl", now, 30*time.Second); !fresh {
		t.Fatal("MarkSeen after expiration should return fresh=true")
	}
}

func TestRedisStore_Signal(t *testing.T) {
	s, mr := newTestRedisStore(t)
	now := time.Now()
	ttl := 5 * time.Minute

	if s.IsSignalFresh("sig", now, ttl) {
		t.Fatal("unseen signal should not be fresh")
	}
	s.MarkSignal("sig", now, ttl)
	if !s.IsSignalFresh("sig", now, ttl) {
		t.Fatal("signal within TTL should be fresh")
	}
	mr.FastForward(ttl + time.Second)
	if s.IsSignalFresh("sig", now, ttl) {
		t.Fatal("signal past TTL should not be fresh")
	}
}

func TestRedisStore_EvictIsNoop(t *testing.T) {
	s, mr := newTestRedisStore(t)
	now := time.Now()

	s.MarkSeen("evt", now, time.Hour)
	s.MarkSignal("sig", now, time.Hour)

	// Evict should not touch keys — Redis handles expiration itself.
	s.Evict(now, time.Second, time.Second)

	if !mr.Exists("test:seen:evt") {
		t.Error("Evict unexpectedly removed seen key")
	}
	if !mr.Exists("test:signal:sig") {
		t.Error("Evict unexpectedly removed signal key")
	}
}

func TestRedisStore_KeyPrefixIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	a, err := NewRedisStore(RedisConfig{Addr: mr.Addr(), KeyPrefix: "a:"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewRedisStore(RedisConfig{Addr: mr.Addr(), KeyPrefix: "b:"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	now := time.Now()
	a.MarkSeen("evt", now, time.Hour)
	if fresh := b.MarkSeen("evt", now, time.Hour); !fresh {
		t.Fatal("different prefixes should not collide")
	}
}

func TestNewRedisStore_RejectsEmptyAddr(t *testing.T) {
	if _, err := NewRedisStore(RedisConfig{}); err == nil {
		t.Fatal("expected error when Addr is empty")
	}
}

func TestRedisStore_Attempts(t *testing.T) {
	s, mr := newTestRedisStore(t)
	now := time.Now()
	window := time.Hour

	if got := s.Attempts("sig", now); got != 0 {
		t.Fatalf("expected 0 attempts initially, got %d", got)
	}
	if got := s.IncrAttempt("sig", now, window); got != 1 {
		t.Fatalf("expected 1 after first increment, got %d", got)
	}
	if got := s.IncrAttempt("sig", now, window); got != 2 {
		t.Fatalf("expected 2 after second increment, got %d", got)
	}
	if got := s.Attempts("sig", now); got != 2 {
		t.Fatalf("expected Attempts=2, got %d", got)
	}
	// Native TTL expiry resets the counter.
	mr.FastForward(window + time.Second)
	if got := s.Attempts("sig", now); got != 0 {
		t.Fatalf("expected counter expired past window, got %d", got)
	}
	if got := s.IncrAttempt("sig", now, window); got != 1 {
		t.Fatalf("expected counter restart from 1, got %d", got)
	}
}

func TestRedisStore_SignalKeepsEscalatedWindow(t *testing.T) {
	// Marked with an escalated TTL, checked with the base one: the native
	// key TTL must rule, exactly like the in-memory deadline.
	s, mr := newTestRedisStore(t)
	now := time.Now()
	base := 5 * time.Minute

	s.MarkSignal("sig", now, 4*base)
	mr.FastForward(2 * base)
	if !s.IsSignalFresh("sig", now, base) {
		t.Fatal("signal marked with 4x ttl must still be fresh at 2x")
	}
	mr.FastForward(2*base + time.Second)
	if s.IsSignalFresh("sig", now, base) {
		t.Fatal("signal past its escalated window should not be fresh")
	}
}
