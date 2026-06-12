package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/sindi98/k8s-ai-remediator/internal/config"
	"github.com/sindi98/k8s-ai-remediator/internal/dedup"
	"github.com/sindi98/k8s-ai-remediator/internal/metrics"
	"github.com/sindi98/k8s-ai-remediator/internal/model"
	"github.com/sindi98/k8s-ai-remediator/internal/notify"
	"github.com/sindi98/k8s-ai-remediator/internal/webui"
)

// stubDecider is a canned decider implementation for loop tests.
type stubDecider struct {
	mu       sync.Mutex
	calls    int
	prompts  []string
	decision model.Decision
	err      error
}

func (s *stubDecider) Decide(_ context.Context, prompt string) (model.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.prompts = append(s.prompts, prompt)
	return s.decision, s.err
}

func (s *stubDecider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubDecider) lastPrompt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.prompts) == 0 {
		return ""
	}
	return s.prompts[len(s.prompts)-1]
}

func staticEvents(evs ...corev1.Event) eventLister {
	return func(context.Context) ([]corev1.Event, error) {
		return evs, nil
	}
}

func warningEvent(ns, name, rv, kind, objName, reason, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, ResourceVersion: rv},
		Type:       "Warning",
		Reason:     reason,
		Message:    message,
		InvolvedObject: corev1.ObjectReference{
			Kind: kind, Name: objName, Namespace: ns,
		},
	}
}

func loopCfg() config.AgentConfig {
	c := defaultCfg()
	c.PollSec = 30
	c.DedupeTTLSec = 300
	c.EventSeenTTLSec = 3600
	c.MaxEventsPerPoll = 10
	c.MinSeverity = "medium"
	c.SignalMaxAttempts = 5
	c.ExcludeNamespaces = []string{"kube-system"}
	return c
}

// runSinglePoll drives runLoopWithStore through exactly one poll: the first
// poll runs synchronously before the ticker loop, and a pre-cancelled
// context makes the loop exit right after it.
func runSinglePoll(t *testing.T, cs kubernetes.Interface, llm decider, cfg config.AgentConfig, m *metrics.Recorder, store dedup.Store, rec *webui.RecentDecisionRecorder, events ...corev1.Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runLoopWithStore(ctx, cs, llm, cfg, m, notify.New(notify.SMTPConfig{}), staticEvents(events...), store, &loopHealth{}, rec)
}

func TestRunLoop_HappyPathRestartsDeployment(t *testing.T) {
	cs := newFakeCluster(t) // deployment "web" + rs + pod web-abc-123
	llm := &stubDecider{decision: model.Decision{
		Action: model.ActionRestartDeployment, Severity: "high", Confidence: 0.9,
	}}
	m := metrics.New()
	store := dedup.NewMemoryStore()
	rec := webui.NewRecentDecisionRecorder(10)

	ev := warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "Back-off restarting failed container")
	runSinglePoll(t, cs, llm, loopCfg(), m, store, rec, ev)

	if got := llm.callCount(); got != 1 {
		t.Fatalf("expected 1 LLM call, got %d", got)
	}
	// The prompt carries the resolved Deployment snapshot.
	if p := llm.lastPrompt(); !strings.Contains(p, "Deployment snapshot: name=web") {
		t.Errorf("prompt missing deployment snapshot:\n%s", p)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected restart annotation on the owner deployment")
	}
	if got := m.EventsProcessed.Load(); got != 1 {
		t.Errorf("EventsProcessed = %d, want 1", got)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != "success" {
		t.Errorf("unexpected recorder state: %+v", snap)
	}
	// One executed attempt registered for the signal.
	if got := store.Attempts("default|Deployment|web|BackOff", time.Now()); got != 1 {
		t.Errorf("expected 1 attempt recorded, got %d", got)
	}
}

func TestRunLoop_PinsDecisionToEventTarget(t *testing.T) {
	// A hallucinated (or prompt-injected) namespace + deployment_name in the
	// model output must not retarget the action: the event anchors it.
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{
		Action: model.ActionRestartDeployment, Severity: "high", Confidence: 0.9,
		Namespace: "kube-system", ResourceKind: "Deployment", ResourceName: "coredns",
		Parameters: map[string]string{"deployment_name": "coredns"},
	}}
	m := metrics.New()
	rec := webui.NewRecentDecisionRecorder(10)

	ev := warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "boom")
	runSinglePoll(t, cs, llm, loopCfg(), m, dedup.NewMemoryStore(), rec, ev)

	// The decision was redirected to the resolved owner in the event namespace.
	dep, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected the resolved owner deployment to be restarted")
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Namespace != "default" || snap[0].Outcome != "success" {
		t.Errorf("decision not pinned to the event target: %+v", snap)
	}
}

func TestRunLoop_NamespaceFiltersSkipLLM(t *testing.T) {
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{Action: model.ActionNoop, Severity: "high"}}
	m := metrics.New()

	cfg := loopCfg() // excludes kube-system
	evExcluded := warningEvent("kube-system", "ev1", "1", "Pod", "coredns-xyz", "BackOff", "x")
	runSinglePoll(t, cs, llm, cfg, m, dedup.NewMemoryStore(), webui.NewRecentDecisionRecorder(10), evExcluded)
	if got := llm.callCount(); got != 0 {
		t.Fatalf("excluded namespace must not reach the LLM, got %d calls", got)
	}

	cfg.IncludeNamespaces = []string{"incident-lab"}
	evOutsideAllowlist := warningEvent("default", "ev2", "2", "Pod", "web-abc-123", "BackOff", "x")
	runSinglePoll(t, cs, llm, cfg, m, dedup.NewMemoryStore(), webui.NewRecentDecisionRecorder(10), evOutsideAllowlist)
	if got := llm.callCount(); got != 0 {
		t.Fatalf("namespace outside the allowlist must not reach the LLM, got %d calls", got)
	}
}

func TestRunLoop_TransientDecideErrorDoesNotBurnDedup(t *testing.T) {
	cs := newFakeCluster(t)
	llm := &stubDecider{err: errors.New("ollama timeout")}
	m := metrics.New()
	store := dedup.NewMemoryStore()
	rec := webui.NewRecentDecisionRecorder(10)
	cfg := loopCfg()

	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "x"))
	if got := m.DecisionErrors.Load(); got != 1 {
		t.Fatalf("DecisionErrors = %d, want 1", got)
	}
	if store.Attempts("default|Deployment|web|BackOff", time.Now()) != 0 {
		t.Error("a transient LLM error must not count as a remediation attempt")
	}

	// The incident recurs (kubelet bumps the event resourceVersion): the
	// LLM recovers and the decision must go through — the failed call did
	// not burn the dedup window.
	llm.mu.Lock()
	llm.err = nil
	llm.decision = model.Decision{Action: model.ActionRestartDeployment, Severity: "high", Confidence: 0.9}
	llm.mu.Unlock()
	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "2", "Pod", "web-abc-123", "BackOff", "x"))

	if got := llm.callCount(); got != 2 {
		t.Fatalf("expected a retry on the next event occurrence, got %d calls", got)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected the retried decision to execute")
	}
}

func TestRunLoop_SignalDedupAcrossEventVersions(t *testing.T) {
	// After a successful decision the signal is marked: a new version of the
	// same incident within the window must NOT trigger another LLM call.
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{Action: model.ActionRestartDeployment, Severity: "high", Confidence: 0.9}}
	m := metrics.New()
	store := dedup.NewMemoryStore()
	rec := webui.NewRecentDecisionRecorder(10)
	cfg := loopCfg()

	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "x"))
	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "2", "Pod", "web-abc-123", "BackOff", "x"))

	if got := llm.callCount(); got != 1 {
		t.Fatalf("expected the marked signal to suppress the second call, got %d", got)
	}
}

func TestRunLoop_SeverityBelowMinimumSkipsButMarks(t *testing.T) {
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{Action: model.ActionRestartDeployment, Severity: "low", Confidence: 0.9}}
	m := metrics.New()
	store := dedup.NewMemoryStore()
	rec := webui.NewRecentDecisionRecorder(10)
	cfg := loopCfg() // MinSeverity medium

	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "x"))

	dep, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if dep.Spec.Template.Annotations != nil {
		if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; ok {
			t.Error("below-minimum decision must not execute")
		}
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Outcome != "skipped" {
		t.Fatalf("expected a skipped record, got %+v", snap)
	}
	// Noise-filtered decisions do not consume remediation attempts...
	if store.Attempts("default|Deployment|web|BackOff", time.Now()) != 0 {
		t.Error("severity skip must not count as a remediation attempt")
	}
	// ...but the signal is marked: a recurrence stays quiet within the window.
	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "2", "Pod", "web-abc-123", "BackOff", "x"))
	if got := llm.callCount(); got != 1 {
		t.Errorf("expected the skipped signal to be deduplicated, got %d calls", got)
	}
}

func TestRunLoop_CircuitBreakerStopsLLM(t *testing.T) {
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{Action: model.ActionRestartDeployment, Severity: "high", Confidence: 0.9}}
	m := metrics.New()
	store := dedup.NewMemoryStore()
	rec := webui.NewRecentDecisionRecorder(10)
	cfg := loopCfg() // SignalMaxAttempts = 5

	// The signal already burned its attempt budget.
	signal := "default|Deployment|web|BackOff"
	now := time.Now()
	for i := 0; i < cfg.SignalMaxAttempts; i++ {
		store.IncrAttempt(signal, now, time.Hour)
	}

	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "x"))

	if got := llm.callCount(); got != 0 {
		t.Fatalf("breaker open: the LLM must not be called, got %d", got)
	}
	snap := rec.Snapshot()
	if len(snap) != 1 || snap[0].Action != string(model.ActionMarkForManualFix) || snap[0].Outcome != "blocked" {
		t.Fatalf("expected a mark_for_manual_fix blocked record, got %+v", snap)
	}
	// The signal is parked: an immediate recurrence is fully silent.
	runSinglePoll(t, cs, llm, cfg, m, store, rec, warningEvent("default", "ev1", "2", "Pod", "web-abc-123", "BackOff", "x"))
	if got := len(rec.Snapshot()); got != 1 {
		t.Errorf("parked signal must not produce more records, got %d", got)
	}
}

func TestRunLoop_MaxEventsPerPollDefersExcess(t *testing.T) {
	cs := newFakeCluster(t)
	llm := &stubDecider{decision: model.Decision{Action: model.ActionNoop, Severity: "high"}}
	m := metrics.New()
	cfg := loopCfg()
	cfg.MaxEventsPerPoll = 1

	// Two distinct signals on the same pod (different reasons).
	ev1 := warningEvent("default", "ev1", "1", "Pod", "web-abc-123", "BackOff", "x")
	ev2 := warningEvent("default", "ev2", "2", "Pod", "web-abc-123", "Unhealthy", "y")
	runSinglePoll(t, cs, llm, cfg, m, dedup.NewMemoryStore(), webui.NewRecentDecisionRecorder(10), ev1, ev2)

	if got := llm.callCount(); got != 1 {
		t.Fatalf("expected the cap to defer the second event, got %d LLM calls", got)
	}
	if got := m.EventsProcessed.Load(); got != 1 {
		t.Errorf("EventsProcessed = %d, want 1", got)
	}
}

func TestSignalBackoffTTL(t *testing.T) {
	base := 5 * time.Minute
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, base}, {1, base}, {2, 2 * base}, {3, 4 * base},
		{4, 8 * base}, {5, 8 * base}, {10, 8 * base},
	}
	for _, c := range cases {
		if got := signalBackoffTTL(base, c.attempts); got != c.want {
			t.Errorf("signalBackoffTTL(base, %d) = %v, want %v", c.attempts, got, c.want)
		}
	}
}

func TestLoopHealth(t *testing.T) {
	h := &loopHealth{}
	// Followers (not leading) are always healthy.
	if !h.healthy(time.Second) {
		t.Error("follower must report healthy")
	}
	h.markLeading(true)
	if !h.healthy(time.Minute) {
		t.Error("freshly-leading loop must be healthy (markLeading stamps)")
	}
	// Simulate a stalled loop: stamp far in the past.
	h.lastPoll.Store(time.Now().Add(-time.Hour).Unix())
	if h.healthy(time.Minute) {
		t.Error("stale heartbeat while leading must be unhealthy")
	}
	h.markLeading(false)
	if !h.healthy(time.Minute) {
		t.Error("after losing leadership the replica must be healthy again")
	}
	// Nil receiver is safe (used before wiring in tests).
	var nilH *loopHealth
	if !nilH.healthy(time.Second) {
		t.Error("nil loopHealth must report healthy")
	}
}

func TestNewWarningEventSource_ServesEventsFromInformer(t *testing.T) {
	ev := warningEvent("default", "ev-informer", "1", "Pod", "p1", "BackOff", "x")
	cs := fake.NewSimpleClientset(&ev)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := newWarningEventSource(ctx, cs)

	events, err := src(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Name == "ev-informer" && e.Namespace == "default" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the informer-backed source to return the event, got %d events", len(events))
	}
}
