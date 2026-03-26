package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder holds all agent metrics and serves them in Prometheus text format.
type Recorder struct {
	mu sync.Mutex

	EventsProcessed  atomic.Int64
	EventsSkipped    atomic.Int64
	DecisionsByAction map[string]*atomic.Int64
	DecisionErrors   atomic.Int64
	ExecutionErrors  atomic.Int64
	OllamaRequests   atomic.Int64
	OllamaErrors     atomic.Int64
	OllamaLatencySum atomic.Int64 // microseconds
	OllamaRateLimited atomic.Int64
}

// New creates a new Recorder with initialized maps.
func New() *Recorder {
	return &Recorder{
		DecisionsByAction: make(map[string]*atomic.Int64),
	}
}

// RecordDecision increments the counter for the given action.
func (r *Recorder) RecordDecision(action string) {
	r.mu.Lock()
	c, ok := r.DecisionsByAction[action]
	if !ok {
		c = &atomic.Int64{}
		r.DecisionsByAction[action] = c
	}
	r.mu.Unlock()
	c.Add(1)
}

// RecordOllamaLatency records a single Ollama request duration.
func (r *Recorder) RecordOllamaLatency(d time.Duration) {
	r.OllamaRequests.Add(1)
	r.OllamaLatencySum.Add(d.Microseconds())
}

// Handler returns an http.Handler that serves metrics in Prometheus text format.
func (r *Recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var b strings.Builder

		writeGauge := func(name, help string, val int64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, val)
		}
		writeCounter := func(name, help string, val int64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, val)
		}

		writeCounter("remediator_events_processed_total", "Total warning events processed", r.EventsProcessed.Load())
		writeGauge("remediator_events_skipped_total", "Total events skipped (dedup or non-warning)", r.EventsSkipped.Load())
		writeCounter("remediator_decision_errors_total", "Total Ollama decision errors", r.DecisionErrors.Load())
		writeCounter("remediator_execution_errors_total", "Total execution errors", r.ExecutionErrors.Load())
		writeCounter("remediator_ollama_requests_total", "Total Ollama requests", r.OllamaRequests.Load())
		writeCounter("remediator_ollama_errors_total", "Total Ollama errors", r.OllamaErrors.Load())
		writeCounter("remediator_ollama_rate_limited_total", "Total Ollama rate limited waits", r.OllamaRateLimited.Load())

		totalReqs := r.OllamaRequests.Load()
		if totalReqs > 0 {
			avgMs := float64(r.OllamaLatencySum.Load()) / float64(totalReqs) / 1000.0
			fmt.Fprintf(&b, "# HELP remediator_ollama_avg_latency_seconds Average Ollama request latency\n")
			fmt.Fprintf(&b, "# TYPE remediator_ollama_avg_latency_seconds gauge\n")
			fmt.Fprintf(&b, "remediator_ollama_avg_latency_seconds %.6f\n", avgMs)
		}

		// Decisions by action
		r.mu.Lock()
		actions := make([]string, 0, len(r.DecisionsByAction))
		for a := range r.DecisionsByAction {
			actions = append(actions, a)
		}
		r.mu.Unlock()
		sort.Strings(actions)

		if len(actions) > 0 {
			fmt.Fprintf(&b, "# HELP remediator_decisions_total Total decisions by action\n")
			fmt.Fprintf(&b, "# TYPE remediator_decisions_total counter\n")
			for _, a := range actions {
				r.mu.Lock()
				c := r.DecisionsByAction[a]
				r.mu.Unlock()
				fmt.Fprintf(&b, "remediator_decisions_total{action=%q} %d\n", a, c.Load())
			}
		}

		w.Write([]byte(b.String()))
	})
}
