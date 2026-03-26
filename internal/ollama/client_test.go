package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
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

	client := NewClient(srv.URL, "test-model", 100, 0, false)
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

	client := NewClient(srv.URL, "test-model", 100, 0, false)
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

	client := NewClient(srv.URL, "test-model", 100, 2, false)
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

	client := NewClient(srv.URL, "test-model", 100, 3, false)
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

	client := NewClient(srv.URL, "test-model", 100, 1, false)
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

	client := NewClient(srv.URL, "test-model", 100, 0, false)
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

	client := NewClient(srv.URL, "test-model", 1000, 0, false)
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
	client := NewClient("https://localhost:99999", "test", 100, 0, true)
	if client.http.Transport == nil {
		t.Error("expected custom transport for TLS skip verify")
	}
}
