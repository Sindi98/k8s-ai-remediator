// Package model holds the types shared across the agent: the action
// allowlist, the Decision returned by the LLM, severity levels, and the
// Ollama chat request/response shapes.
package model

import "strings"

// Severity represents the severity level of an incident.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// severityRank maps severity levels to numeric ranks for comparison.
var severityRank = map[Severity]int{
	SeverityCritical: 4,
	SeverityHigh:     3,
	SeverityMedium:   2,
	SeverityLow:      1,
	SeverityInfo:     0,
}

// ParseSeverity normalises a severity string to a known Severity value.
// Unknown values default to SeverityLow.
func ParseSeverity(s string) Severity {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case SeverityCritical:
		return SeverityCritical
	case SeverityHigh:
		return SeverityHigh
	case SeverityMedium:
		return SeverityMedium
	case SeverityLow:
		return SeverityLow
	case SeverityInfo:
		return SeverityInfo
	default:
		return SeverityLow
	}
}

// MeetsMinimum returns true if s is at or above the given minimum severity.
func (s Severity) MeetsMinimum(min Severity) bool {
	return severityRank[s] >= severityRank[min]
}

// Action represents a remediation action the agent can take.
type Action string

const (
	ActionNoop               Action = "noop"
	ActionRestartDeployment  Action = "restart_deployment"
	ActionDeleteFailedPod    Action = "delete_failed_pod"
	ActionDeleteAndRecreate  Action = "delete_and_recreate_pod"
	ActionScaleDeployment    Action = "scale_deployment"
	ActionInspectPodLogs     Action = "inspect_pod_logs"
	ActionSetDeploymentImage Action = "set_deployment_image"
	ActionPatchProbe         Action = "patch_probe"
	ActionPatchResources     Action = "patch_resources"
	ActionPatchRegistry      Action = "patch_registry"
	ActionMarkForManualFix   Action = "mark_for_manual_fix"
	ActionAskHuman           Action = "ask_human"
)

// AllActions returns the list of all valid actions.
func AllActions() []Action {
	return []Action{
		ActionNoop, ActionRestartDeployment, ActionDeleteFailedPod,
		ActionDeleteAndRecreate, ActionScaleDeployment, ActionInspectPodLogs,
		ActionSetDeploymentImage, ActionPatchProbe, ActionPatchResources,
		ActionPatchRegistry, ActionMarkForManualFix, ActionAskHuman,
	}
}

// IsOperatorGated reports whether an action can only fire once an operator has
// deliberately enabled it: a global feature flag (ALLOW_PATCH_* /
// ALLOW_IMAGE_UPDATES), a per-Deployment opt-in annotation, and a confidence
// threshold all guard these actions. Because those gates already encode
// operator intent, the agent must NOT additionally drop them through the
// MIN_SEVERITY noise filter: a weak local model that under-rates a genuinely
// actionable incident (e.g. labelling a flaky-probe fix "low") would otherwise
// have its remediation silently discarded before it ever runs. Read-only and
// no-op actions stay subject to MIN_SEVERITY so real noise is still filtered.
func (a Action) IsOperatorGated() bool {
	switch a {
	case ActionPatchProbe, ActionPatchResources, ActionPatchRegistry, ActionSetDeploymentImage:
		return true
	default:
		return false
	}
}

// Decision is the structured response from the LLM.
type Decision struct {
	Summary       string            `json:"summary"`
	Severity      string            `json:"severity"`
	ProbableCause string            `json:"probable_cause"`
	Confidence    float64           `json:"confidence"`
	Action        Action            `json:"action"`
	Namespace     string            `json:"namespace"`
	ResourceKind  string            `json:"resource_kind"`
	ResourceName  string            `json:"resource_name"`
	Parameters    map[string]string `json:"parameters"`
	Reason        string            `json:"reason"`
}

// Message represents a single chat message for the Ollama API.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is the request body for the Ollama /api/chat endpoint.
type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Format   map[string]any `json:"format"`
	Options  map[string]any `json:"options"`
	// Think toggles the model's reasoning ("thinking") mode. It is a
	// top-level chat API parameter (Ollama >= 0.9), NOT an options entry:
	// the generate endpoint ignores it inside options. Pointer + omitempty
	// keeps the three states distinct on the wire: nil omits the field
	// (server/model default), &false serialises as "think":false, &true
	// as "think":true.
	Think *bool `json:"think,omitempty"`
}

// ChatResponse is the response body from the Ollama /api/chat endpoint.
type ChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}
