package webui

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sindi98/k8s-ai-remediator/internal/model"
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

	// rdb, when non-nil, mirrors every record into a capped Redis list so
	// the dashboard history survives pod restarts and leader failovers.
	// All Redis I/O is fail-open: a Redis hiccup degrades to memory-only.
	rdb      *redis.Client
	redisKey string
}

// redisOpTimeout caps each Redis call so a slow or dead Redis can never
// stall the poll loop or an HTTP handler.
const redisOpTimeout = 500 * time.Millisecond

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

// RedisDecisionOptions configures the optional Redis persistence of the
// recent-decisions buffer. Mirrors the dedup backend connection knobs.
type RedisDecisionOptions struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string
}

// NewRecentDecisionRecorderRedis returns a recorder that, in addition to the
// in-memory ring, mirrors every record into a capped Redis list. When Redis
// is unreachable at startup it degrades to memory-only with a warning —
// fail-open, exactly like the dedup store.
func NewRecentDecisionRecorderRedis(capacity int, opts RedisDecisionOptions) *RecentDecisionRecorder {
	r := NewRecentDecisionRecorder(capacity)
	if opts.Addr == "" || capacity == 0 {
		return r
	}
	client := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  redisOpTimeout,
		WriteTimeout: redisOpTimeout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		slog.Warn("decisions: redis unreachable, history will not survive restarts", "error", err)
		_ = client.Close()
		return r
	}
	r.rdb = client
	r.redisKey = opts.KeyPrefix + "decisions:recent"
	return r
}

// Close releases the Redis connection pool, when one was attached.
func (r *RecentDecisionRecorder) Close() error {
	if r == nil || r.rdb == nil {
		return nil
	}
	return r.rdb.Close()
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

	// Mirror into Redis outside the mutex: network I/O must never block
	// concurrent Snapshot readers. LPUSH+LTRIM keeps the list capped.
	if r.rdb != nil {
		b, err := json.Marshal(rec)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		defer cancel()
		pipe := r.rdb.Pipeline()
		pipe.LPush(ctx, r.redisKey, b)
		pipe.LTrim(ctx, r.redisKey, 0, int64(r.capacity-1))
		if _, err := pipe.Exec(ctx); err != nil {
			slog.Warn("decisions: redis persist failed", "error", err)
		}
	}
}

// Snapshot returns a copy of the records, newest first. Safe to call
// concurrently with Record. When Redis persistence is attached, the list is
// read from Redis (so it includes decisions recorded before the last pod
// restart) and the in-memory ring is the fallback on any Redis error.
func (r *RecentDecisionRecorder) Snapshot() []DecisionRecord {
	if r == nil || r.capacity == 0 {
		return nil
	}

	if r.rdb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
		vals, err := r.rdb.LRange(ctx, r.redisKey, 0, int64(r.capacity-1)).Result()
		cancel()
		if err == nil && len(vals) > 0 {
			out := make([]DecisionRecord, 0, len(vals))
			for _, v := range vals {
				var rec DecisionRecord
				if json.Unmarshal([]byte(v), &rec) == nil {
					out = append(out, rec)
				}
			}
			if len(out) > 0 {
				return out // LPUSH order: index 0 is the newest
			}
		}
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
