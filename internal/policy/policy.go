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
// The prompt is tuned for smaller local models (e.g. qwen2.5:7b): a compact
// decision tree, few-shot examples, and explicit format constraints, instead
// of long prose that smaller models often ignore.
func BuildPrompt(ns, kind, name, etype, reason, message, extra string) string {
	const fieldMaxLen = 2000
	// Every Kubernetes-supplied string is treated as untrusted: namespace
	// and resource names can be user-controlled (e.g. CRD-managed workloads)
	// and must be stripped of control chars and known injection phrases
	// before reaching the LLM.
	ns = SanitizeForPrompt(ns, 253)      // k8s name hard limit
	kind = SanitizeForPrompt(kind, 64)
	name = SanitizeForPrompt(name, 253)
	etype = SanitizeForPrompt(etype, 32)
	reason = SanitizeForPrompt(reason, 500)
	message = SanitizeForPrompt(message, fieldMaxLen)
	extra = SanitizeForPrompt(extra, fieldMaxLen)

	return fmt.Sprintf(`You are a Kubernetes remediation agent. Return ONLY valid JSON matching the schema, nothing else.

=== INCIDENT ===
Event type: %s
Reason: %s
Message: %s
Namespace: %s
Object kind: %s
Object name: %s
%s

=== ALLOWED ACTIONS ===
noop, restart_deployment, delete_failed_pod, delete_and_recreate_pod, scale_deployment, inspect_pod_logs, set_deployment_image, patch_probe, patch_resources, patch_registry, mark_for_manual_fix, ask_human

=== DECISION TREE (evaluate in order, stop at first match) ===

0. PRIORITY SCAN: search the entire incident text for "OOMKilled",
   "exit=137", "Insufficient cpu", "Insufficient memory", "FailedScheduling".
   If present, jump IMMEDIATELY to the matching rule below (1 for OOM,
   2 for FailedScheduling) without considering the event reason alone.

1. OOMKilled or exit=137 present ANYWHERE (event message, PodStatusSummary,
   lastTerminated reason):
   - If snapshot "Allow-patch scopes" contains "resources" or "*":
       action=patch_resources, severity=high
       params: deployment_name, container,
               memory_limit=<2x-8x current limit, at least 256Mi>,
               memory_request=<half of limit>
   - Else: action=mark_for_manual_fix
   NEVER pick restart_deployment when OOMKilled is visible. The new pod
   hits the same limit and OOMs again.

2. Event reason is "FailedScheduling" (pod Pending, Insufficient cpu/memory):
   - If "Allow-patch scopes" contains "resources" or "*":
       action=patch_resources, severity=critical
       params: deployment_name, container,
               cpu_request="100m", memory_request="64Mi",
               cpu_limit="500m", memory_limit="256Mi"
   - Else: action=mark_for_manual_fix
   NEVER pick scale_deployment or restart_deployment on FailedScheduling.

3. Event reason is "Unhealthy" (probe failure, no crash, no OOM):
   - If snapshot "Allow-patch scopes" contains "probe" or "*":
       action=patch_probe, severity=high
       params: deployment_name, container, probe (readiness|liveness),
               failure_threshold="5", period_seconds="15", timeout_seconds="5"
   - Else: action=inspect_pod_logs (or mark_for_manual_fix if no container)
   NEVER pick restart_deployment for Unhealthy.

4. Event reason is "BackOff" (CrashLoopBackOff) AND no OOMKilled/exit=137:
   - action=restart_deployment, severity=high
     params: deployment_name
   - If container is crashing for a reason you can deduce (bad config, missing file),
     prefer inspect_pod_logs first (include parameters.container).

5. Image pull failure ("Failed to pull image", ErrImagePull, ImagePullBackOff):
   - If the image in the Deployment snapshot is SYNTACTICALLY VALID (looks like a real tag):
       action=restart_deployment, severity=high, params: deployment_name
       (this retries the pull; transient network or registry rate-limit)
   - If the image is clearly wrong/inventato AND you know a safe replacement:
       action=set_deployment_image, severity=high
       params: deployment_name, image=<DIFFERENT valid image string>
       NEVER propose the SAME image already in the snapshot (no-op).
   - If the wrong registry host is the root cause AND "Allow-patch scopes" contains "registry" or "*":
       action=patch_registry
       params: deployment_name, container, new_registry
   - Else: action=mark_for_manual_fix

6. Nothing else matches: action=inspect_pod_logs if you need logs, else mark_for_manual_fix.

=== OUTPUT FORMAT CONSTRAINTS ===
- severity ∈ {critical, high, medium, low, info}
- confidence: float 0.0-1.0 (>=0.85 unlocks set_deployment_image and patch_*)
- Whenever the action targets a Deployment, ALWAYS set params.deployment_name
- Numeric params for patch_probe: decimal integer strings only
  OK: "5", "15"
  NOT OK: "x5", "2x", "+3", "5s", "5 seconds"
- Quantity params for patch_resources: Kubernetes quantity strings ("100m", "256Mi")
- NEVER propose shell commands
- NEVER invent image names or registry hosts; only use concrete strings

=== EXAMPLES ===

Example A - Unhealthy probe with opt-in:
  Event reason: Unhealthy
  Snapshot: Allow-patch scopes: probe; readinessProbe failureThreshold=2 period=5
  → {"action":"patch_probe","severity":"high","confidence":0.95,
     "probable_cause":"Probe timing too strict for this workload",
     "params":{"deployment_name":"app","container":"main","probe":"readiness",
               "failure_threshold":"5","period_seconds":"15"}}

Example B - OOMKilled with opt-in:
  Event reason: BackOff; Pod lastTerminated reason=OOMKilled exit=137
  Snapshot: Allow-patch scopes: resources; container memory_limit=32Mi
  → {"action":"patch_resources","severity":"high","confidence":0.95,
     "probable_cause":"Memory limit too low for workload allocation",
     "params":{"deployment_name":"app","container":"main",
               "memory_limit":"256Mi","memory_request":"128Mi"}}

Example C - Failed to pull, valid image:
  Event reason: Failed; Message: Failed to pull image "busybox:1.36"
  Snapshot: containers=main=busybox:1.36
  → {"action":"restart_deployment","severity":"high","confidence":0.9,
     "probable_cause":"Transient pull failure (network or registry rate-limit)",
     "params":{"deployment_name":"app"}}`,
		etype, reason, message, ns, kind, name, extra)
}
