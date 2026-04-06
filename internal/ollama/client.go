package ollama

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

// Client communicates with the Ollama API and enforces rate limiting.
type Client struct {
	baseURL    string
	model      string
	http       *http.Client
	limiter    *rate.Limiter
	maxRetries int
}

// NewClient creates an Ollama client with rate limiting, TLS support, and retry config.
func NewClient(baseURL, mdl string, rps float64, maxRetries int, tlsSkipVerify bool) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		baseURL: baseURL,
		model:   mdl,
		http: &http.Client{
			Timeout:   90 * time.Second,
			Transport: transport,
		},
		limiter:    rate.NewLimiter(rate.Limit(rps), 1),
		maxRetries: maxRetries,
	}
}

// Decide sends the prompt to Ollama and returns a validated Decision.
// Transient errors (network, 5xx) are retried with exponential backoff.
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
					"Severity must be one of: critical, high, medium, low, info. " +
					"For critical, high, and medium severity incidents, attempt remediation when a safe action is available. " +
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

	var lastErr error
	attempts := c.maxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			slog.Warn("retrying ollama request", "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return model.Decision{}, ctx.Err()
			case <-time.After(backoff):
			}
		}

		d, err := c.doRequest(ctx, b)
		if err != nil {
			lastErr = err
			if isRetryable(err) {
				continue
			}
			return model.Decision{}, err
		}
		return d, nil
	}

	return model.Decision{}, fmt.Errorf("ollama request failed after %d attempts: %w", attempts, lastErr)
}

func (c *Client) doRequest(ctx context.Context, body []byte) (model.Decision, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/chat", bytes.NewReader(body))
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

	if resp.StatusCode >= 500 {
		respBody, _ := io.ReadAll(resp.Body)
		return model.Decision{}, &retryableError{msg: fmt.Sprintf("ollama http %d: %s", resp.StatusCode, string(respBody))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return model.Decision{}, fmt.Errorf("ollama http %d: %s", resp.StatusCode, string(respBody))
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

// retryableError marks errors that should trigger a retry.
type retryableError struct {
	msg string
}

func (e *retryableError) Error() string { return e.msg }

func isRetryable(err error) bool {
	if _, ok := err.(*retryableError); ok {
		return true
	}
	// Network errors (connection refused, timeout, DNS) are retryable
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout")
}

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
			"severity":       map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "info"}},
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
