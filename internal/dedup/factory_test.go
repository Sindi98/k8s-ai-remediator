package dedup

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestNewStore_Memory(t *testing.T) {
	for _, name := range []string{"", "memory", "MEMORY", " Memory "} {
		s, err := NewStore(BackendConfig{Backend: name})
		if err != nil {
			t.Fatalf("backend=%q: unexpected error: %v", name, err)
		}
		if _, ok := s.(*MemoryStore); !ok {
			t.Errorf("backend=%q: expected *MemoryStore, got %T", name, s)
		}
	}
}

func TestNewStore_Redis(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := NewStore(BackendConfig{
		Backend:        "redis",
		RedisAddr:      mr.Addr(),
		RedisKeyPrefix: "f:",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := s.(*RedisStore); !ok {
		t.Errorf("expected *RedisStore, got %T", s)
	}
}

func TestNewStore_UnknownBackend(t *testing.T) {
	if _, err := NewStore(BackendConfig{Backend: "kafka"}); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}
