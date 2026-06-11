// Package ollama is the HTTP client for the Ollama chat API: it enforces
// rate limiting, retries transient failures with exponential backoff,
// constrains responses to a JSON schema, and validates the returned action.
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
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/sindi98/k8s-ai-remediator/internal/model"
)

// Client communicates with the Ollama API and enforces rate limiting.
type Client struct {
	baseURL    string
	model      string
	http       *http.Client
	limiter    *rate.Limiter
	maxRetries int

	// think mirrors AgentConfig.OllamaThink: nil omits the chat API `think`
	// parameter (server default), false disables the reasoning mode of
	// thinking models (qwen3.x, gemma4), true enables it. Set once via
	// SetThink before the first Decide.
	think *bool
	// thinkUnsupported latches when the server rejects the think parameter
	// (model without the thinking capability, e.g. qwen2.5). From then on
	// the field is dropped from every request so OLLAMA_THINK=false never
	// breaks non-reasoning models. Atomic out of caution: Decide is called
	// serially by the poll loop today, but the latch must stay correct if a
	// second caller ever appears.
	thinkUnsupported atomic.Bool

	// onRateLimited, onError and onRequest are optional metric hooks. They
	// fire, respectively, when a request is delayed by the rate limiter, when
	// an attempt fails, and once per HTTP attempt with its latency. All may be
	// nil; wire them via SetMetricsHooks. Kept as callbacks rather than
	// importing the metrics package so the client stays decoupled and trivially
	// testable.
	onRateLimited func()
	onError       func()
	onRequest     func(time.Duration)
}

// SetThink configures the reasoning ("thinking") mode requested from the
// model: nil leaves the server default, false disables it, true enables it.
// Call once right after NewClient, before the first Decide.
func (c *Client) SetThink(think *bool) {
	c.think = think
}

// SetMetricsHooks wires optional counters. onRateLimited fires on rate-limit
// waits, onError on each failed attempt, and onRequest once per HTTP attempt
// (success or failure) with its measured latency — so request and error
// counters both reflect per-attempt traffic instead of only successes. Any
// callback may be nil. Call once right after NewClient, before the first Decide.
func (c *Client) SetMetricsHooks(onRateLimited, onError func(), onRequest func(time.Duration)) {
	c.onRateLimited = onRateLimited
	c.onError = onError
	c.onRequest = onRequest
}

// NewClient creates an Ollama client with rate limiting, TLS support, and retry config.
// httpTimeoutSec caps the per-request HTTP timeout (awaiting headers + body). Local
// LLMs can occasionally take well over a minute; defaults to 180s when 0 is passed.
func NewClient(baseURL, mdl string, rps float64, maxRetries int, tlsSkipVerify bool, httpTimeoutSec int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	timeout := time.Duration(httpTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 180 * time.Second
	}

	return &Client{
		baseURL: baseURL,
		model:   mdl,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		limiter:    rate.NewLimiter(rate.Limit(rps), 1),
		maxRetries: maxRetries,
	}
}

// systemPrompt is the fixed system message sent with every decision request.
const systemPrompt = "You are a Kubernetes remediation agent. Return only valid JSON matching the schema. " +
	"Allowed actions: noop,restart_deployment,delete_failed_pod,delete_and_recreate_pod,scale_deployment,inspect_pod_logs,set_deployment_image,patch_probe,patch_resources,patch_registry,mark_for_manual_fix,ask_human. " +
	"Severity must be one of: critical, high, medium, low, info. " +
	"Never suggest shell commands. " +
	"The user message contains detailed rules for when to pick each action; follow them strictly, including any HARD RULE."

// buildRequestBody assembles the chat payload, honouring the configured
// thinking mode. When thinking is explicitly disabled the Qwen-style
// "/no_think" soft switch is also appended to the system message: the
// native `think` parameter needs Ollama >= 0.9, while the in-prompt switch
// covers older servers (inert text for other model families). Once the
// server has declared the model incapable of thinking, the parameter is
// dropped entirely.
func (c *Client) buildRequestBody(prompt string) ([]byte, error) {
	think := c.think
	if c.thinkUnsupported.Load() {
		think = nil
	}
	system := systemPrompt
	if think != nil && !*think {
		system += " /no_think"
	}

	reqBody := model.ChatRequest{
		Model: c.model,
		Messages: []model.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Format:  buildSchema(),
		Options: map[string]any{"temperature": 0},
		Think:   think,
	}
	return json.Marshal(reqBody)
}

// Decide sends the prompt to Ollama and returns a validated Decision.
// Transient errors (network, 5xx) are retried with exponential backoff.
func (c *Client) Decide(ctx context.Context, prompt string) (model.Decision, error) {
	waitStart := time.Now()
	if err := c.limiter.Wait(ctx); err != nil {
		return model.Decision{}, fmt.Errorf("ollama rate limiter: %w", err)
	}
	// Count the call as rate-limited only when the limiter actually made us
	// wait (a token was not immediately available). The sub-millisecond
	// threshold filters out the trivial "token ready" fast path.
	if c.onRateLimited != nil && time.Since(waitStart) > time.Millisecond {
		c.onRateLimited()
	}

	slog.Debug("sending prompt to ollama", "model", c.model, "prompt_len", len(prompt))

	b, err := c.buildRequestBody(prompt)
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
			if c.onError != nil {
				c.onError()
			}
			// The server rejected the think parameter: the configured model has
			// no thinking capability (e.g. qwen2.5). Latch, rebuild the body
			// without the field and redo the call, so OLLAMA_THINK=false never
			// breaks non-reasoning models. The latch makes the recursion depth
			// at most one.
			if isThinkUnsupportedErr(err) && c.think != nil && !c.thinkUnsupported.Load() {
				c.thinkUnsupported.Store(true)
				slog.Warn("ollama: model does not support the think parameter; retrying without it",
					"model", c.model)
				return c.Decide(ctx, prompt)
			}
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
	// Count every attempt that reached the wire (success or failure) so the
	// request counter and average latency reflect real per-attempt traffic.
	if c.onRequest != nil {
		c.onRequest(duration)
	}
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
	content := cleanModelContent(out.Message.Content)
	if content == "" {
		return model.Decision{}, fmt.Errorf("empty ollama response")
	}

	var d model.Decision
	if err := json.Unmarshal([]byte(content), &d); err != nil {
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

// thinkBlockRe matches a reasoning block at the start of the content.
// Qwen-family models emit one (empty with /no_think) on Ollama versions
// that do not separate thinking from content.
var thinkBlockRe = regexp.MustCompile(`(?s)\A\s*<think>.*?</think>`)

// cleanModelContent strips the artifacts thinking-capable models leave
// around the JSON payload: a leading <think>...</think> block and markdown
// code fences (observed when thinking interacts with structured outputs,
// e.g. gemma4). Plain JSON passes through untouched.
func cleanModelContent(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	return s
}

// isThinkUnsupportedErr matches the 4xx error Ollama returns when the
// `think` parameter is sent to a model without the thinking capability.
// Matched on the stable phrase used by the server ("does not support
// thinking").
func isThinkUnsupportedErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "does not support thinking")
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
		model.ActionPatchProbe,
		model.ActionPatchResources,
		model.ActionPatchRegistry,
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
