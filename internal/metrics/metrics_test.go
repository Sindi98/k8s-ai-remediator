package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecorder_BasicMetrics(t *testing.T) {
	m := New()
	m.EventsProcessed.Add(10)
	m.EventsSkipped.Add(5)
	m.DecisionErrors.Add(2)
	m.ExecutionErrors.Add(1)

	m.RecordDecision("noop")
	m.RecordDecision("noop")
	m.RecordDecision("restart_deployment")

	m.RecordOllamaLatency(100 * time.Millisecond)
	m.RecordOllamaLatency(200 * time.Millisecond)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()

	checks := []string{
		"remediator_events_processed_total 10",
		"remediator_events_skipped_total 5",
		"remediator_decision_errors_total 2",
		"remediator_execution_errors_total 1",
		"remediator_ollama_requests_total 2",
		`remediator_decisions_total{action="noop"} 2`,
		`remediator_decisions_total{action="restart_deployment"} 1`,
		"remediator_ollama_avg_latency_seconds",
	}

	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("metrics output should contain %q\ngot:\n%s", c, body)
		}
	}

	if !strings.Contains(body, "text/plain") {
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/plain") {
			t.Errorf("expected text/plain content type, got %s", ct)
		}
	}
}

func TestRecorder_EmptyMetrics(t *testing.T) {
	m := New()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "remediator_events_processed_total 0") {
		t.Error("empty metrics should show zero values")
	}
	// No avg latency when no requests
	if strings.Contains(body, "remediator_ollama_avg_latency_seconds") {
		t.Error("avg latency should not appear with zero requests")
	}
}
