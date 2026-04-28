package webui

import (
	"sync"
	"time"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

// DecisionRecord captures a single remediation cycle for the dashboard
// "Recent decisions" widget. Keeping it in webui (instead of, say,
// internal/metrics) avoids inverting the dependency direction: the agent
// already imports webui, so it can record without a new package.
type DecisionRecord struct {
	Time         time.Time `json:"time"`
	Namespace    string    `json:"namespace"`
	Kind         string    `json:"kind"`
	Name         string    `json:"name"`
	EventReason  string    `json:"event_reason"`
	Action       string    `json:"action"`
	Severity     string    `json:"severity"`
	Confidence   float64   `json:"confidence"`
	Summary      string    `json:"summary"`
	Outcome      string    `json:"outcome"` // "success" | "blocked" | "error" | "skipped"
	OutcomeError string    `json:"outcome_error,omitempty"`
}

// RecentDecisionRecorder is a fixed-size ring buffer of the most recent
// decisions. It is goroutine-safe so the poll loop can Record concurrently
// with HTTP handlers reading via Snapshot.
//
// Capacity is set at construction; once full, every new record overwrites
// the oldest. Fixed bound matters because the agent runs unattended: an
// unbounded log would leak memory in pods with long uptime.
type RecentDecisionRecorder struct {
	mu       sync.Mutex
	capacity int
	buf      []DecisionRecord
	next     int  // next slot to write
	full     bool // buffer has wrapped at least once
}

// NewRecentDecisionRecorder allocates a recorder with the given capacity.
// A capacity of 0 disables recording (Record/Snapshot become no-ops),
// useful in tests.
func NewRecentDecisionRecorder(capacity int) *RecentDecisionRecorder {
	if capacity < 0 {
		capacity = 0
	}
	return &RecentDecisionRecorder{
		capacity: capacity,
		buf:      make([]DecisionRecord, capacity),
	}
}

// Record appends a single decision. Outcome must be one of
// "success" | "blocked" | "error" | "skipped"; outcomeErr is the
// stringified failure reason or empty for success.
func (r *RecentDecisionRecorder) Record(ns, kind, name, eventReason string, d model.Decision, outcome, outcomeErr string) {
	if r == nil || r.capacity == 0 {
		return
	}
	rec := DecisionRecord{
		Time:         time.Now().UTC(),
		Namespace:    ns,
		Kind:         kind,
		Name:         name,
		EventReason:  eventReason,
		Action:       string(d.Action),
		Severity:     d.Severity,
		Confidence:   d.Confidence,
		Summary:      d.Summary,
		Outcome:      outcome,
		OutcomeError: outcomeErr,
	}
	r.mu.Lock()
	r.buf[r.next] = rec
	r.next = (r.next + 1) % r.capacity
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot returns a copy of the records, newest first. Safe to call
// concurrently with Record.
func (r *RecentDecisionRecorder) Snapshot() []DecisionRecord {
	if r == nil || r.capacity == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []DecisionRecord
	if r.full {
		out = make([]DecisionRecord, 0, r.capacity)
		for i := 0; i < r.capacity; i++ {
			idx := (r.next - 1 - i + r.capacity) % r.capacity
			out = append(out, r.buf[idx])
		}
	} else {
		out = make([]DecisionRecord, 0, r.next)
		for i := r.next - 1; i >= 0; i-- {
			out = append(out, r.buf[i])
		}
	}
	return out
}
