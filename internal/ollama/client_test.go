package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sindi98/k8s-ai-remediator/internal/model"
)

func validDecisionJSON() []byte {
	d := model.Decision{
		Summary: "test", Severity: "low", ProbableCause: "test",
		Confidence: 0.85, Action: model.ActionRestartDeployment,
		Namespace: "default", ResourceKind: "Deployment", ResourceName: "web",
		Parameters: map[string]string{}, Reason: "test",
	}
	b, _ := json.Marshal(d)
	return b
}

func TestAllowedAction(t *testing.T) {
	for _, a := range model.AllActions() {
		if !AllowedAction(a) {
			t.Errorf("expected action %q to be allowed", a)
		}
	}
	if AllowedAction(model.Action("exec_shell")) {
		t.Error("action exec_shell should not be allowed")
	}
}

func TestDecide_ValidResponse(t *testing.T) {
	decJSON := validDecisionJSON()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = string(decJSON)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	got, err := client.Decide(context.Background(), "test prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Action != model.ActionRestartDeployment {
		t.Errorf("expected restart_deployment, got %s", got.Action)
	}
	if got.Confidence != 0.85 {
		t.Errorf("expected confidence 0.85, got %f", got.Confidence)
	}
}

func TestDecide_DisallowedAction(t *testing.T) {
	decision := map[string]any{
		"summary": "test", "severity": "low", "probable_cause": "test",
		"confidence": 0.5, "action": "exec_shell", "namespace": "default",
		"resource_kind": "Pod", "resource_name": "test",
		"parameters": map[string]string{}, "reason": "test",
	}
	decJSON, _ := json.Marshal(decision)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = string(decJSON)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for disallowed action")
	}
	if !strings.Contains(err.Error(), "action not allowed") {
		t.Errorf("expected 'action not allowed' error, got: %v", err)
	}
}

func TestDecide_HTTPError_4xx_NoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 2, false, 0)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for HTTP 400")
	}
	// 4xx errors should NOT be retried
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 call (no retry for 4xx), got %d", calls)
	}
}

func TestDecide_HTTPError_5xx_Retries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("internal error"))
			return
		}
		// Third call succeeds
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 3, false, 0)
	got, err := client.Decide(context.Background(), "test prompt")
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if got.Action != model.ActionRestartDeployment {
		t.Errorf("expected restart_deployment, got %s", got.Action)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 calls (2 retries + 1 success), got %d", calls)
	}
}

func TestDecide_HTTPError_5xx_ExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 1, false, 0)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "failed after") {
		t.Errorf("expected 'failed after' error, got: %v", err)
	}
	// 1 initial + 1 retry = 2 calls
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestDecide_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestDecide_RateLimiting(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 1000, 0, false, 0)
	for i := 0; i < 3; i++ {
		_, err := client.Decide(context.Background(), "test")
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestDecide_TLSSkipVerify(t *testing.T) {
	// Just verify the client can be created with TLS skip verify without panic
	client := NewClient("https://localhost:99999", "test", 100, 0, true, 0)
	if client.http.Transport == nil {
		t.Error("expected custom transport for TLS skip verify")
	}
}

func TestSetMetricsHooks_OnErrorPerAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
	}))
	defer srv.Close()

	var errHookCalls int32
	client := NewClient(srv.URL, "test-model", 100, 1, false, 0)
	client.SetMetricsHooks(nil, func() { atomic.AddInt32(&errHookCalls, 1) }, nil)

	if _, err := client.Decide(context.Background(), "test"); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial attempt + 1 retry = 2 failed attempts → 2 error hook calls.
	if got := atomic.LoadInt32(&errHookCalls); got != 2 {
		t.Errorf("expected onError to fire twice, got %d", got)
	}
}

func TestSetMetricsHooks_OnRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var rlHookCalls int32
	// rps=2 with burst 1: the first call passes instantly, the second has to
	// wait ~500ms for a token, which must trigger the rate-limited hook.
	client := NewClient(srv.URL, "test-model", 2, 0, false, 0)
	client.SetMetricsHooks(func() { atomic.AddInt32(&rlHookCalls, 1) }, nil, nil)

	for i := 0; i < 2; i++ {
		if _, err := client.Decide(context.Background(), "test"); err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&rlHookCalls); got < 1 {
		t.Errorf("expected onRateLimited to fire at least once, got %d", got)
	}
}

func TestSetMetricsHooks_OnRequestPerAttempt(t *testing.T) {
	// 5xx triggers retries; onRequest must fire once per HTTP attempt (not just
	// on success) so remediator_ollama_requests_total counts real traffic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
	}))
	defer srv.Close()

	var reqHookCalls int32
	client := NewClient(srv.URL, "test-model", 100, 1, false, 0)
	client.SetMetricsHooks(nil, nil, func(time.Duration) { atomic.AddInt32(&reqHookCalls, 1) })

	if _, err := client.Decide(context.Background(), "test"); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial attempt + 1 retry = 2 HTTP attempts → 2 request hook calls.
	if got := atomic.LoadInt32(&reqHookCalls); got != 2 {
		t.Errorf("expected onRequest to fire twice, got %d", got)
	}
}

func TestDecide_NilHooks_NoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// No SetMetricsHooks call: both hooks stay nil and must be skipped safely.
	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	if _, err := client.Decide(context.Background(), "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// newBodyCaptureServer returns a test server that records every request body
// and replies with a valid decision, plus an accessor for the recorded bodies.
func newBodyCaptureServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func TestDecide_ThinkFalse_SendsParamAndSoftSwitch(t *testing.T) {
	srv, bodies := newBodyCaptureServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	think := false
	client.SetThink(&think)
	if _, err := client.Decide(context.Background(), "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	var req model.ChatRequest
	if err := json.Unmarshal([]byte(got[0]), &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req.Think == nil || *req.Think {
		t.Error(`expected "think":false in the request body`)
	}
	if !strings.HasSuffix(req.Messages[0].Content, "/no_think") {
		t.Error("expected the /no_think soft switch appended to the system message")
	}
}

func TestDecide_ThinkUnset_OmitsParam(t *testing.T) {
	srv, bodies := newBodyCaptureServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	if _, err := client.Decide(context.Background(), "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := bodies()
	if len(got) != 1 {
		t.Fatalf("expected 1 request, got %d", len(got))
	}
	if strings.Contains(got[0], `"think"`) {
		t.Error("think field must be omitted when no mode is configured")
	}
	if strings.Contains(got[0], "/no_think") {
		t.Error("soft switch must not be appended when no mode is configured")
	}
}

func TestDecide_ThinkTrue_SendsParamWithoutSoftSwitch(t *testing.T) {
	srv, bodies := newBodyCaptureServer(t)
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	think := true
	client.SetThink(&think)
	if _, err := client.Decide(context.Background(), "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := bodies()
	var req model.ChatRequest
	if err := json.Unmarshal([]byte(got[0]), &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req.Think == nil || !*req.Think {
		t.Error(`expected "think":true in the request body`)
	}
	if strings.Contains(req.Messages[0].Content, "/no_think") {
		t.Error("soft switch must not be appended when thinking is enabled")
	}
}

func TestDecide_ThinkUnsupported_AutoDegrades(t *testing.T) {
	// OLLAMA_THINK=false must never break models without the thinking
	// capability (qwen2.5 and friends): on the server's rejection the client
	// drops the parameter, retries, and latches the degrade for later calls.
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), `"think"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"\"test-model\" does not support thinking"}`))
			return
		}
		resp := model.ChatResponse{}
		resp.Message.Content = string(validDecisionJSON())
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	think := false
	client.SetThink(&think)

	got, err := client.Decide(context.Background(), "test")
	if err != nil {
		t.Fatalf("expected auto-degrade to succeed, got %v", err)
	}
	if got.Action != model.ActionRestartDeployment {
		t.Errorf("expected restart_deployment, got %s", got.Action)
	}

	mu.Lock()
	n, first, second := len(bodies), bodies[0], bodies[len(bodies)-1]
	mu.Unlock()
	if n != 2 {
		t.Fatalf("expected 2 requests (rejected + degraded), got %d", n)
	}
	if !strings.Contains(first, `"think"`) {
		t.Error("first request should carry the think parameter")
	}
	if strings.Contains(second, `"think"`) {
		t.Error("degraded retry must drop the think parameter")
	}

	// The latch persists: later calls go straight out without the parameter.
	if _, err := client.Decide(context.Background(), "test"); err != nil {
		t.Fatalf("unexpected error after degrade: %v", err)
	}
	mu.Lock()
	n, third := len(bodies), bodies[len(bodies)-1]
	mu.Unlock()
	if n != 3 {
		t.Fatalf("expected 3 total requests, got %d", n)
	}
	if strings.Contains(third, `"think"`) {
		t.Error("latched client must not send think again")
	}
}

func TestCleanModelContent(t *testing.T) {
	plain := `{"a":1}`
	cases := []struct{ in, want string }{
		{plain, plain},
		{"  \n" + plain + "\n ", plain},
		// Empty reasoning block: qwen3 emits one even with /no_think on
		// Ollama versions that do not separate thinking from content.
		{"<think>\n\n</think>\n" + plain, plain},
		{"<think>step 1\nstep 2</think>" + plain, plain},
		{"```json\n" + plain + "\n```", plain},
		{"```\n" + plain + "\n```", plain},
		{"<think>x</think>\n```json\n" + plain + "\n```", plain},
		// Nothing but reasoning: cleans to empty, surfaced upstream as an
		// "empty ollama response" error rather than a JSON parse failure.
		{"<think>only reasoning</think>", ""},
	}
	for _, c := range cases {
		if got := cleanModelContent(c.in); got != c.want {
			t.Errorf("cleanModelContent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecide_ParsesThinkBlockAndFencedContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = "<think>\nreasoning...\n</think>\n```json\n" + string(validDecisionJSON()) + "\n```"
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	got, err := client.Decide(context.Background(), "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Action != model.ActionRestartDeployment {
		t.Errorf("expected restart_deployment, got %s", got.Action)
	}
}

func TestDecide_OnlyThinkBlockIsEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = "<think>endless pondering</think>"
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100, 0, false, 0)
	_, err := client.Decide(context.Background(), "test")
	if err == nil || !strings.Contains(err.Error(), "empty ollama response") {
		t.Errorf("expected empty-response error, got %v", err)
	}
}
