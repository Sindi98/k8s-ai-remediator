package webui

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"github.com/sindi98/k8s-ai-remediator/internal/model"
)

func TestRecordRingBufferOverwritesOldest(t *testing.T) {
	r := NewRecentDecisionRecorder(3)
	for i := 0; i < 5; i++ {
		r.Record("ns", "Pod", "p", "Reason", model.Decision{
			Action:   model.ActionNoop,
			Severity: "low",
			Summary:  string(rune('a' + i)),
		}, "success", "")
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d, want 3", len(snap))
	}
	// Newest first: e, d, c (b and a were overwritten).
	want := []string{"e", "d", "c"}
	for i, w := range want {
		if snap[i].Summary != w {
			t.Errorf("snap[%d].Summary = %q, want %q", i, snap[i].Summary, w)
		}
	}
}

func TestRecordRingBufferPartialFill(t *testing.T) {
	r := NewRecentDecisionRecorder(5)
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "first"}, "success", "")
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "second"}, "success", "")
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	if snap[0].Summary != "second" || snap[1].Summary != "first" {
		t.Errorf("got order %q,%q want second,first", snap[0].Summary, snap[1].Summary)
	}
}

func TestZeroCapacityRecorderIsNoOp(t *testing.T) {
	r := NewRecentDecisionRecorder(0)
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "x"}, "success", "")
	if got := r.Snapshot(); got != nil {
		t.Fatalf("snapshot from zero-capacity recorder = %v, want nil", got)
	}
}

func TestNilRecorderRecordIsSafe(t *testing.T) {
	var r *RecentDecisionRecorder
	// Must not panic; main.go relies on this when WEBUI is disabled and
	// the recorder is wired through anyway.
	r.Record("ns", "Pod", "p", "R", model.Decision{}, "success", "")
}

func newRedisRecorder(t *testing.T, capacity int) (*RecentDecisionRecorder, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	r := NewRecentDecisionRecorderRedis(capacity, RedisDecisionOptions{
		Addr:      mr.Addr(),
		KeyPrefix: "test:",
	})
	t.Cleanup(func() { _ = r.Close() })
	return r, mr
}

func TestRedisRecorder_PersistsCappedList(t *testing.T) {
	r, mr := newRedisRecorder(t, 3)
	for i := 0; i < 5; i++ {
		r.Record("ns", "Pod", "p", "Reason", model.Decision{
			Action:  model.ActionNoop,
			Summary: string(rune('a' + i)),
		}, "success", "")
	}

	// The Redis list is capped at the capacity, newest first.
	vals, err := mr.List("test:decisions:recent")
	if err != nil {
		t.Fatalf("redis list: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("redis list len = %d, want 3", len(vals))
	}
	snap := r.Snapshot()
	if len(snap) != 3 || snap[0].Summary != "e" || snap[2].Summary != "c" {
		t.Errorf("unexpected snapshot from redis: %+v", snap)
	}
}

func TestRedisRecorder_HistorySurvivesRestart(t *testing.T) {
	r, mr := newRedisRecorder(t, 5)
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "before-restart"}, "success", "")

	// A fresh recorder (same Redis, empty memory ring) simulates the agent
	// pod restarting: the dashboard history must come back from Redis.
	fresh := NewRecentDecisionRecorderRedis(5, RedisDecisionOptions{
		Addr:      mr.Addr(),
		KeyPrefix: "test:",
	})
	defer fresh.Close()
	snap := fresh.Snapshot()
	if len(snap) != 1 || snap[0].Summary != "before-restart" {
		t.Fatalf("expected history from redis after restart, got %+v", snap)
	}
}

func TestRedisRecorder_UnreachableRedisDegradesToMemory(t *testing.T) {
	r := NewRecentDecisionRecorderRedis(3, RedisDecisionOptions{
		Addr: "127.0.0.1:1", // nothing listens here
	})
	defer r.Close()
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "mem-only"}, "success", "")
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Summary != "mem-only" {
		t.Fatalf("expected memory-only fallback to work, got %+v", snap)
	}
}

func TestRedisRecorder_FallsBackToMemoryWhenRedisDies(t *testing.T) {
	r, mr := newRedisRecorder(t, 3)
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "x"}, "success", "")
	mr.Close() // Redis outage after startup
	r.Record("ns", "Pod", "p", "R", model.Decision{Summary: "y"}, "success", "")
	snap := r.Snapshot() // LRANGE fails → memory ring
	if len(snap) != 2 || snap[0].Summary != "y" {
		t.Fatalf("expected memory fallback during outage, got %+v", snap)
	}
}
