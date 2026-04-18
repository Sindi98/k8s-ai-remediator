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

// PatchFlags groups the enable flags for the three patch_* actions so
// callers can pass them around atomically.
type PatchFlags struct {
	AllowProbe     bool
	AllowResources bool
	AllowRegistry  bool
	Threshold      float64
}

// MaybeBlockRestartOnProbeFailure rejects restart_deployment when the source
// event was a probe failure (reason Unhealthy). Restarting a Deployment does
// not fix a misconfigured probe; the LLM is steered toward patch_probe or
// inspect_pod_logs instead. Mirrors the HARD RULE in the prompt so a
// disobedient LLM cannot waste a restart loop.
func MaybeBlockRestartOnProbeFailure(d model.Decision, eventReason string) error {
	if d.Action != model.ActionRestartDeployment {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(eventReason), "Unhealthy") {
		return nil
	}
	return fmt.Errorf("restart_deployment blocked: event reason=Unhealthy; restart does not fix probe misconfigurations, use patch_probe or inspect_pod_logs")
}

// MaybeBlockRestartOnOOMKilled rejects restart_deployment when the extra
// context attached to the decision indicates that the pod was OOMKilled (or
// exited with 137). Restarting would just recreate a pod that hits the same
// memory limit. The LLM is expected to pick patch_resources instead.
func MaybeBlockRestartOnOOMKilled(d model.Decision, extra string) error {
	if d.Action != model.ActionRestartDeployment {
		return nil
	}
	low := strings.ToLower(extra)
	if !strings.Contains(low, "oomkilled") && !strings.Contains(low, "exit=137") {
		return nil
	}
	return fmt.Errorf("restart_deployment blocked: pod status shows OOMKilled; restart does not fix memory pressure, use patch_resources")
}

// MaybeBlockWrongActionOnFailedScheduling rejects scale_deployment and
// restart_deployment when the event reason is FailedScheduling. Neither can
// solve a single-pod resource request larger than any node; patch_resources
// (or mark_for_manual_fix) is the correct path.
func MaybeBlockWrongActionOnFailedScheduling(d model.Decision, eventReason string) error {
	if d.Action != model.ActionScaleDeployment && d.Action != model.ActionRestartDeployment {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(eventReason), "FailedScheduling") {
		return nil
	}
	return fmt.Errorf("%s blocked: event reason=FailedScheduling; scale/restart cannot satisfy impossible resource requests, use patch_resources", d.Action)
}

// MaybeBlockUnsafePatch enforces the global feature flag and the confidence
// threshold for the three patch_* actions. The per-Deployment opt-in
// annotation is checked separately inside the kube package because it
// requires reading the Deployment.
func MaybeBlockUnsafePatch(d model.Decision, flags PatchFlags) error {
	var enabled bool
	switch d.Action {
	case model.ActionPatchProbe:
		enabled = flags.AllowProbe
	case model.ActionPatchResources:
		enabled = flags.AllowResources
	case model.ActionPatchRegistry:
		enabled = flags.AllowRegistry
	default:
		return nil
	}
	if !enabled {
		return fmt.Errorf("%s disabled by policy", d.Action)
	}
	if d.Confidence < flags.Threshold {
		return fmt.Errorf("%s blocked: confidence %.2f below threshold %.2f", d.Action, d.Confidence, flags.Threshold)
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
- Allowed actions: noop,restart_deployment,delete_failed_pod,delete_and_recreate_pod,scale_deployment,inspect_pod_logs,set_deployment_image,patch_probe,patch_resources,patch_registry,mark_for_manual_fix,ask_human
- Severity must be one of: critical, high, medium, low, info
- For critical and high severity incidents, take immediate remediation action when confident
- For medium severity incidents, attempt remediation if a safe action is available (e.g. restart, delete_and_recreate_pod, inspect_pod_logs)
- Use noop or ask_human only for low/info severity or when confidence is very low
- Never propose shell commands
- If the incident mentions CrashLoopBackOff, you may use inspect_pod_logs first, but if the pod is managed by a Deployment and the issue looks recoverable, prefer restart_deployment over delete_and_recreate_pod (the specific pod name from the event may no longer exist)
- Use delete_and_recreate_pod only for standalone pods not managed by a Deployment
- If a pod is failed or stuck and managed by a Deployment, use restart_deployment
- If the issue mentions ImagePullBackOff, ErrImagePull or "Failed to pull image", do not invent an image name
- HARD RULE: never propose set_deployment_image with the SAME image already present in the Deployment snapshot containers list. It is a no-op (the rollout would re-create pods that hit the same pull failure) and the agent will reject it. For transient pull failures (network blip, registry rate-limit on a previously-working image) PICK delete_failed_pod or restart_deployment to retry the pull. Use set_deployment_image only when proposing a DIFFERENT, concrete and safe replacement image.
- Use mark_for_manual_fix when the image problem cannot be resolved safely from the event alone (e.g. tag truly does not exist anywhere)
- If using inspect_pod_logs on a multi-container pod, include parameters.container when possible
- HARD RULE: if the event reason is Unhealthy (readiness or liveness probe failure) and the pod is not crashing, NEVER pick restart_deployment. A rolling restart spawns a new pod that immediately hits the same probe misconfiguration. If the Deployment snapshot reports "Allow-patch scopes" containing "probe" or "*", PICK patch_probe DIRECTLY (do not fall back to inspect_pod_logs; the logs will not reveal a probe timing problem). Read the current probe values from the snapshot and propose new, MORE PERMISSIVE integer values. Typical safe choices: failure_threshold=5, period_seconds=10 or 15, timeout_seconds=5. If the snapshot reports "Allow-patch scopes: none", fall back to inspect_pod_logs or mark_for_manual_fix
- Every patch_probe parameter value MUST be a plain decimal integer string. Good: "5", "15". BAD: "x5", "2x", "+3", "5s", "5 seconds". Do NOT write expressions or multipliers; compute the final integer yourself and emit only that
- When the action targets a Deployment, set parameters.deployment_name so the agent can act even if the specific pod no longer exists
- patch_probe tunes probe timing only. Required parameters: deployment_name, container, probe (readiness|liveness), and at least one of initial_delay_seconds, period_seconds, failure_threshold, success_threshold, timeout_seconds. Do not rewrite the probe handler (exec/httpGet/tcpSocket)
- patch_resources adjusts CPU/memory requests and limits. Required parameters: deployment_name, container, and at least one of cpu_request, memory_request, cpu_limit, memory_limit. Use Kubernetes quantity strings (e.g. "250m", "512Mi"). Prefer this for OOMKilled signals
- HARD RULE: if the pod status shows lastTerminated reason=OOMKilled or exit=137, or the event reason is BackOff on a Deployment whose snapshot reports Allow-patch scopes containing "resources" or "*", PICK patch_resources DIRECTLY (restart_deployment is useless: the new pod will hit the same memory limit). Read the current memory_limit from the container spec (if any) and propose a higher value as a plain quantity string, e.g. memory_limit="256Mi" when the current is 32Mi. If the pod status shows lastTerminated reason=Error and the exit code is not 137, patch_resources is not appropriate; prefer inspect_pod_logs first
- HARD RULE: if the event reason is FailedScheduling (pod stuck Pending with "Insufficient cpu/memory" or similar), NEVER pick scale_deployment or restart_deployment: they cannot solve a single-pod resource request bigger than any node. If the Deployment snapshot reports Allow-patch scopes containing "resources" or "*", PICK patch_resources DIRECTLY with reasonable node-sized values (cpu_request around 100m, memory_request around 64-128Mi). Without the opt-in, pick mark_for_manual_fix
- patch_registry rewrites only the registry host of the container image, keeping the path and tag. Required parameters: deployment_name, container, new_registry (e.g. "host.docker.internal:5050"). Prefer this for ErrImagePull/ErrImageNeverPull caused by a wrong registry prefix
- patch_* actions require a high-confidence read of the event (>= 0.85) and an opt-in annotation on the target Deployment; if either is unlikely, prefer mark_for_manual_fix`,
		etype, reason, message, ns, kind, name, extra)
}
