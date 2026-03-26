package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

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
	decision := model.Decision{
		Summary:       "Pod crash detected",
		Severity:      "high",
		ProbableCause: "OOM",
		Confidence:    0.85,
		Action:        model.ActionRestartDeployment,
		Namespace:     "default",
		ResourceKind:  "Deployment",
		ResourceName:  "web",
		Parameters:    map[string]string{},
		Reason:        "Restart to recover from OOM",
	}
	decJSON, _ := json.Marshal(decision)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		resp.Message.Content = string(decJSON)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100)
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

	client := NewClient(srv.URL, "test-model", 100)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for disallowed action")
	}
	if !strings.Contains(err.Error(), "action not allowed") {
		t.Errorf("expected 'action not allowed' error, got: %v", err)
	}
}

func TestDecide_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "ollama http 500") {
		t.Errorf("expected http error, got: %v", err)
	}
}

func TestDecide_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := model.ChatResponse{}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-model", 100)
	_, err := client.Decide(context.Background(), "test prompt")
	if err == nil {
		t.Error("expected error for empty response")
	}
}

func TestDecide_RateLimiting(t *testing.T) {
	calls := 0
	decision := model.Decision{
		Summary: "test", Severity: "low", ProbableCause: "test",
		Confidence: 0.5, Action: model.ActionNoop, Namespace: "default",
		ResourceKind: "Pod", ResourceName: "test",
		Parameters: map[string]string{}, Reason: "test",
	}
	decJSON, _ := json.Marshal(decision)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		resp := model.ChatResponse{}
		resp.Message.Content = string(decJSON)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// High RPS so the test doesn't actually block
	client := NewClient(srv.URL, "test-model", 1000)
	for i := 0; i < 3; i++ {
		_, err := client.Decide(context.Background(), "test")
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}
