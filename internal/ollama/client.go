package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

// Client communicates with the Ollama API and enforces rate limiting.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
	limiter *rate.Limiter
}

// NewClient creates an Ollama client with the given rate limit (requests/sec).
func NewClient(baseURL, mdl string, rps float64) *Client {
	return &Client{
		baseURL: baseURL,
		model:   mdl,
		http:    &http.Client{Timeout: 90 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

// Decide sends the prompt to Ollama and returns a validated Decision.
func (c *Client) Decide(ctx context.Context, prompt string) (model.Decision, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return model.Decision{}, fmt.Errorf("ollama rate limiter: %w", err)
	}

	slog.Debug("sending prompt to ollama", "model", c.model, "prompt_len", len(prompt))

	schema := buildSchema()

	reqBody := model.ChatRequest{
		Model: c.model,
		Messages: []model.Message{
			{
				Role: "system",
				Content: "Return only valid JSON matching the schema. " +
					"Allowed actions: noop,restart_deployment,delete_failed_pod,delete_and_recreate_pod,scale_deployment,inspect_pod_logs,set_deployment_image,mark_for_manual_fix,ask_human. " +
					"Never suggest shell commands. " +
					"If the issue contains CrashLoopBackOff, you may use inspect_pod_logs first, but if the pod is managed by a Deployment and the issue looks recoverable, prefer delete_and_recreate_pod or restart_deployment. " +
					"If the issue contains ImagePullBackOff or ErrImagePull, prefer mark_for_manual_fix unless a concrete safe replacement image is explicit. " +
					"Use set_deployment_image only when parameters.image contains a concrete replacement image string.",
			},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Format:  schema,
		Options: map[string]any{"temperature": 0},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return model.Decision{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/chat", bytes.NewReader(b))
	if err != nil {
		return model.Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.http.Do(req)
	duration := time.Since(start)
	if err != nil {
		return model.Decision{}, err
	}
	defer resp.Body.Close()

	slog.Info("ollama response received", "status", resp.StatusCode, "duration", duration.Round(time.Millisecond))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return model.Decision{}, fmt.Errorf("ollama http %d: %s", resp.StatusCode, string(body))
	}

	var out model.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return model.Decision{}, err
	}
	if out.Message.Content == "" {
		return model.Decision{}, fmt.Errorf("empty ollama response")
	}

	var d model.Decision
	if err := json.Unmarshal([]byte(out.Message.Content), &d); err != nil {
		return model.Decision{}, err
	}
	if !AllowedAction(d.Action) {
		return model.Decision{}, fmt.Errorf("action not allowed: %s", d.Action)
	}
	if d.Parameters == nil {
		d.Parameters = map[string]string{}
	}

	return d, nil
}

// Duration returns the duration of the last request (for metrics).
// This is a convenience — callers can also measure externally.

// AllowedAction returns true if the action is in the safe allowlist.
func AllowedAction(a model.Action) bool {
	switch a {
	case model.ActionNoop,
		model.ActionRestartDeployment,
		model.ActionDeleteFailedPod,
		model.ActionDeleteAndRecreate,
		model.ActionScaleDeployment,
		model.ActionInspectPodLogs,
		model.ActionSetDeploymentImage,
		model.ActionMarkForManualFix,
		model.ActionAskHuman:
		return true
	default:
		return false
	}
}

func buildSchema() map[string]any {
	actions := make([]string, 0, len(model.AllActions()))
	for _, a := range model.AllActions() {
		actions = append(actions, string(a))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary":        map[string]any{"type": "string"},
			"severity":       map[string]any{"type": "string"},
			"probable_cause": map[string]any{"type": "string"},
			"confidence":     map[string]any{"type": "number"},
			"action":         map[string]any{"type": "string", "enum": actions},
			"namespace":      map[string]any{"type": "string"},
			"resource_kind":  map[string]any{"type": "string"},
			"resource_name":  map[string]any{"type": "string"},
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"reason": map[string]any{"type": "string"},
		},
		"required": []string{
			"summary", "severity", "probable_cause", "confidence",
			"action", "namespace", "resource_kind", "resource_name",
			"parameters", "reason",
		},
	}
}
