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
}

// ChatResponse is the response body from the Ollama /api/chat endpoint.
type ChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}
