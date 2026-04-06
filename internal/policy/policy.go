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
	if err := ValidateOCIImage(d.Parameters["image"]); err != nil {
		return fmt.Errorf("set_deployment_image blocked: %w", err)
	}
	return nil
}

var (
	controlCharRe    = regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)
	injectionPattern = regexp.MustCompile(`(?i)(ignore previous instructions|ignore all instructions|disregard above|system:\s|you are now|forget everything|new instructions:)`)

	// ociImageRe validates OCI-compatible image references:
	// [registry/][namespace/]name[:tag][@sha256:digest]
	ociImageRe = regexp.MustCompile(`^([a-zA-Z0-9][a-zA-Z0-9._-]*(:[0-9]+)?/)?([a-zA-Z0-9][a-zA-Z0-9._/-]*)(:[\w][\w.\-]{0,127})?(@sha256:[a-fA-F0-9]{64})?$`)
)

// ValidateOCIImage checks that the image string is a valid OCI image reference.
func ValidateOCIImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("image is empty")
	}
	if len(image) > 255 {
		return fmt.Errorf("image reference exceeds 255 characters")
	}
	if !ociImageRe.MatchString(image) {
		return fmt.Errorf("invalid OCI image reference: %s", image)
	}
	return nil
}

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
- Severity must be one of: critical, high, medium, low, info
- For critical and high severity incidents, take immediate remediation action when confident
- For medium severity incidents, attempt remediation if a safe action is available (e.g. restart, delete_and_recreate_pod, inspect_pod_logs)
- Use noop or ask_human only for low/info severity or when confidence is very low
- Never propose shell commands
- If the incident mentions CrashLoopBackOff, you may use inspect_pod_logs first, but if the pod is managed by a Deployment and the issue looks recoverable, prefer restart_deployment over delete_and_recreate_pod (the specific pod name from the event may no longer exist)
- Use delete_and_recreate_pod only for standalone pods not managed by a Deployment
- If a pod is failed or stuck and managed by a Deployment, use restart_deployment
- If the issue mentions ImagePullBackOff or ErrImagePull, do not invent an image name
- Use set_deployment_image only if parameters.image contains a concrete image string
- Use mark_for_manual_fix when the image problem cannot be resolved safely from the event alone
- If using inspect_pod_logs on a multi-container pod, include parameters.container when possible`,
		etype, reason, message, ns, kind, name, extra)
}
