package webui

import (
	"testing"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
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
