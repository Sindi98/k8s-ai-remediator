package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tuo-user/k8s-ai-remediator/internal/model"
)

// MaybeBlockUnsafeImageUpdate enforces safety checks for image update decisions.
func MaybeBlockUnsafeImageUpdate(d model.Decision, allowImageUpdates bool, threshold float64) error {
	if d.Action != model.ActionSetDeploymentImage {
		return nil
	}
	if !allowImageUpdates {
		return fmt.Errorf("set_deployment_image disabled by policy")
	}
	if d.Confidence < threshold {
		return fmt.Errorf("set_deployment_image blocked: confidence %.2f below threshold %.2f", d.Confidence, threshold)
	}
	if strings.TrimSpace(d.Parameters["image"]) == "" {
		return fmt.Errorf("set_deployment_image blocked: parameters.image missing")
	}
	return nil
}

var (
	controlCharRe    = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)
	injectionPattern = regexp.MustCompile(`(?i)(ignore previous instructions|ignore all instructions|disregard above|system:\s|you are now|forget everything|new instructions:)`)
)

// SanitizeForPrompt removes characters and patterns that could be used for
// prompt injection attacks in LLM inputs sourced from Kubernetes events.
func SanitizeForPrompt(s string, maxLen int) string {
	s = controlCharRe.ReplaceAllString(s, "")
	s = injectionPattern.ReplaceAllString(s, "[REDACTED]")

	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen] + "...[truncated]"
	}

	return strings.TrimSpace(s)
}

// BuildPrompt constructs the LLM prompt from Kubernetes event fields.
func BuildPrompt(ns, kind, name, etype, reason, message, extra string) string {
	const fieldMaxLen = 2000
	reason = SanitizeForPrompt(reason, 500)
	message = SanitizeForPrompt(message, fieldMaxLen)
	extra = SanitizeForPrompt(extra, fieldMaxLen)

	return fmt.Sprintf(`Analyze this Kubernetes incident and return only JSON.

Event type: %s
Reason: %s
Message: %s
Namespace: %s
Object kind: %s
Object name: %s
%s

Rules:
- Allowed actions: noop,restart_deployment,delete_failed_pod,delete_and_recreate_pod,scale_deployment,inspect_pod_logs,set_deployment_image,mark_for_manual_fix,ask_human
- Prefer noop or ask_human if confidence is low
- Never propose shell commands
- If the incident mentions CrashLoopBackOff, you may use inspect_pod_logs first, but if the pod is managed by a Deployment and the issue looks recoverable, prefer delete_and_recreate_pod or restart_deployment
- If a pod is failed or stuck and deleting it is safe, consider delete_and_recreate_pod
- If the issue mentions ImagePullBackOff or ErrImagePull, do not invent an image name
- Use set_deployment_image only if parameters.image contains a concrete image string
- Use mark_for_manual_fix when the image problem cannot be resolved safely from the event alone
- If using inspect_pod_logs on a multi-container pod, include parameters.container when possible`,
		etype, reason, message, ns, kind, name, extra)
}
