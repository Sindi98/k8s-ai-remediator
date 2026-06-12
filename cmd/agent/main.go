package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	memcached "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/sindi98/k8s-ai-remediator/internal/config"
	"github.com/sindi98/k8s-ai-remediator/internal/dedup"
	"github.com/sindi98/k8s-ai-remediator/internal/kube"
	"github.com/sindi98/k8s-ai-remediator/internal/metrics"
	"github.com/sindi98/k8s-ai-remediator/internal/model"
	"github.com/sindi98/k8s-ai-remediator/internal/notify"
	"github.com/sindi98/k8s-ai-remediator/internal/ollama"
	"github.com/sindi98/k8s-ai-remediator/internal/policy"
	"github.com/sindi98/k8s-ai-remediator/internal/registry"
	"github.com/sindi98/k8s-ai-remediator/internal/webui"
)

func executeDecision(
	ctx context.Context,
	cs kubernetes.Interface,
	d model.Decision,
	cfg config.AgentConfig,
	eventReason string,
	extra string,
) error {
	if err := policy.MaybeBlockRestartOnProbeFailure(d, eventReason); err != nil {
		// Auto-escalation: a probe failure (reason Unhealthy) is never fixed by
		// a restart. If the Deployment opts in to probe patching, relax the
		// probe instead of rejecting the decision outright. Mirrors the OOM
		// auto-escalation below.
		if transformed, ok := tryAutoPatchProbeOnUnhealthy(ctx, cs, d, cfg); ok {
			slog.Info("auto-escalation: restart_deployment → patch_probe (Unhealthy)",
				"ns", transformed.Namespace, "deployment", transformed.Parameters["deployment_name"],
				"container", transformed.Parameters["container"], "probe", transformed.Parameters["probe"])
			d = transformed
		} else {
			return err
		}
	}
	if err := policy.MaybeBlockRestartOnOOMKilled(d, extra); err != nil {
		// Auto-escalation: if the Deployment opts in to resources patching,
		// silently transform the rejected restart into a patch_resources
		// with a safe 4x bump (floor 512Mi). This covers the common case
		// where the LLM fails the "NEVER restart on OOMKilled" rule.
		if transformed, ok := tryAutoPatchResourcesOnOOM(ctx, cs, d, cfg); ok {
			slog.Info("auto-escalation: restart_deployment → patch_resources (OOMKilled)",
				"ns", transformed.Namespace, "deployment", transformed.Parameters["deployment_name"],
				"memory_limit", transformed.Parameters["memory_limit"])
			d = transformed
		} else {
			return err
		}
	}
	if err := policy.MaybeBlockWrongActionOnFailedScheduling(d, eventReason); err != nil {
		return err
	}
	// Complete a set_deployment_image that names no image BEFORE the policy
	// guard. Models reliably emit parameters they can copy from the context
	// but omit ones they must invent, so the image is derived: preferably the
	// newest tag advertised by the image's own registry (IMAGE_TAG_DISCOVERY),
	// otherwise the operator's fallback tag (IMAGE_FALLBACK_TAG). The
	// synthesized reference then passes the exact same gates as a
	// model-provided one: feature flag, confidence threshold, OCI validation
	// and the no-op rejection (current image already on the derived tag).
	if d.Action == model.ActionSetDeploymentImage && cfg.AllowImageUpdates &&
		(cfg.ImageTagDiscovery || cfg.ImageFallbackTag != "") &&
		strings.TrimSpace(d.Parameters["image"]) == "" {
		if img, ok := deriveFallbackImage(ctx, cs, d, cfg); ok {
			slog.Info("set_deployment_image: completing missing image",
				"ns", d.Namespace, "image", img)
			if d.Parameters == nil {
				d.Parameters = map[string]string{}
			}
			d.Parameters["image"] = img
		}
	}
	if err := policy.MaybeBlockUnsafeImageUpdate(d, cfg.AllowImageUpdates, cfg.ImageUpdateThreshold); err != nil {
		return err
	}
	if err := policy.MaybeBlockUnsafePatch(d, policy.PatchFlags{
		AllowProbe:     cfg.AllowPatchProbe,
		AllowResources: cfg.AllowPatchResources,
		AllowRegistry:  cfg.AllowPatchRegistry,
		Threshold:      cfg.PatchConfidenceThreshold,
	}); err != nil {
		return err
	}

	switch d.Action {
	case model.ActionNoop, model.ActionAskHuman, model.ActionMarkForManualFix:
		return nil

	case model.ActionRestartDeployment:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		return kube.RestartDeployment(ctx, cs, d.Namespace, depName, cfg.DryRun)

	case model.ActionDeleteFailedPod:
		if strings.EqualFold(d.ResourceKind, "Pod") {
			return kube.DeletePod(ctx, cs, d.Namespace, d.ResourceName, cfg.DryRun)
		}
		return fmt.Errorf("%s requires resource_kind=Pod", d.Action)

	case model.ActionDeleteAndRecreate:
		// Distinct from delete_failed_pod: force-delete (zero grace period) so a
		// pod wedged in termination is replaced from scratch immediately.
		if strings.EqualFold(d.ResourceKind, "Pod") {
			return kube.DeleteAndRecreatePod(ctx, cs, d.Namespace, d.ResourceName, cfg.DryRun)
		}
		return fmt.Errorf("%s requires resource_kind=Pod", d.Action)

	case model.ActionScaleDeployment:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		r, err := strconv.Atoi(d.Parameters["replicas"])
		if err != nil {
			return fmt.Errorf("invalid replicas parameter")
		}
		// Guard against int->int32 overflow before casting: strconv.Atoi
		// returns a platform int, which on 64-bit boxes accepts values well
		// outside int32. ScaleDeployment then applies the user policy.
		if r < math.MinInt32 || r > math.MaxInt32 {
			return fmt.Errorf("replicas %d out of range", r)
		}
		return kube.ScaleDeployment(ctx, cs, d.Namespace, depName, int32(r), cfg.MinScale, cfg.MaxScale, cfg.DryRun)

	case model.ActionInspectPodLogs:
		return kube.InspectPodLogs(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters, cfg.PodLogTailLines)

	case model.ActionSetDeploymentImage:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		return kube.SetDeploymentImage(ctx, cs, d.Namespace, depName, d.Parameters["image"], d.Parameters["container"], cfg.DryRun)

	case model.ActionPatchProbe:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		fields := map[string]string{}
		for _, k := range []string{"initial_delay_seconds", "period_seconds", "failure_threshold", "success_threshold", "timeout_seconds"} {
			if v := strings.TrimSpace(d.Parameters[k]); v != "" {
				fields[k] = v
			}
		}
		container := strings.TrimSpace(d.Parameters["container"])
		probeType := strings.ToLower(strings.TrimSpace(d.Parameters["probe"]))
		// Deterministic completion for a model that picked the right action
		// but emitted empty parameters (seen with constrained decoding
		// dropping map-typed params): derive the target probe from the
		// Deployment spec and fall back to the same tolerant timing defaults
		// the Unhealthy auto-escalation uses. The real gates (global flag,
		// per-Deployment annotation, confidence threshold) still apply.
		if probeType != "readiness" && probeType != "liveness" {
			dep, derr := cs.AppsV1().Deployments(d.Namespace).Get(ctx, depName, metav1.GetOptions{})
			if derr != nil {
				return fmt.Errorf("probe must be one of: readiness, liveness (deriving a default failed: %v)", derr)
			}
			c, p, ok := chooseProbeFromDeployment(dep)
			if !ok {
				return fmt.Errorf("probe must be one of: readiness, liveness (and deployment %s has no probe to derive a default from)", depName)
			}
			probeType = p
			if container == "" {
				container = c
			}
		}
		if len(fields) == 0 {
			fields["failure_threshold"] = "6"
			fields["period_seconds"] = "15"
			fields["timeout_seconds"] = "5"
		}
		return kube.PatchDeploymentProbe(ctx, cs, d.Namespace, depName, container, probeType, fields, cfg.DryRun)

	case model.ActionPatchResources:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		params := d.Parameters
		switch {
		case isOOMContext(extra):
			// Deterministic safety net for a directly-chosen patch_resources:
			// a weak model often proposes a memory_limit only marginally above
			// the current one (or omits it), which passes validation but OOMs
			// again. Raise it to the same floor the OOM auto-escalation uses.
			params = withOOMMemoryFloor(ctx, cs, d.Namespace, depName, params)
		case strings.EqualFold(strings.TrimSpace(eventReason), "FailedScheduling") && !hasResourceParams(params):
			// The prompt's rule 2 tells the model to lower requests to
			// schedulable-anywhere values on FailedScheduling, but a model that
			// emits empty parameters would otherwise leave the workload Pending
			// forever. Mirror those exact values; the opt-in annotation gates it.
			params = withSchedulableResourceDefaults(params)
		case !hasResourceParams(params):
			// patch_resources with no quantities and no OOM marker in our
			// snapshot (the pod read can time out, so `extra` may miss the
			// OOMKilled/exit=137 the model saw): the dominant cause is memory
			// pressure. Apply the same memory floor from the container's
			// current limit instead of failing — observed with memory-hog,
			// where the model picks patch_resources but invents no quantity.
			params = withOOMMemoryFloor(ctx, cs, d.Namespace, depName, params)
		}
		return kube.PatchDeploymentResources(ctx, cs, d.Namespace, depName, params["container"], params, cfg.DryRun)

	case model.ActionPatchRegistry:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		return kube.PatchDeploymentRegistry(ctx, cs, d.Namespace, depName, d.Parameters["container"], d.Parameters["new_registry"], cfg.DryRun)

	default:
		return fmt.Errorf("unsupported action")
	}
}

// runLoop executes the main event polling loop. It returns when the parent
// context is cancelled (e.g. on SIGTERM), allowing for graceful shutdown.
// tryAutoPatchResourcesOnOOM converts a blocked restart_deployment into a
// patch_resources when the target Deployment has the "resources" opt-in
// annotation and the global flag is on. Uses a 4x bump of the current
// memory_limit (minimum 512Mi) so the retry has real headroom over the
// allocation and does not immediately OOM again. Returns (transformed, true)
// on success, (untouched, false) otherwise.
func tryAutoPatchResourcesOnOOM(ctx context.Context, cs kubernetes.Interface, d model.Decision, cfg config.AgentConfig) (model.Decision, bool) {
	if !cfg.AllowPatchResources {
		return d, false
	}

	depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
	if err != nil {
		return d, false
	}

	dep, err := cs.AppsV1().Deployments(d.Namespace).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return d, false
	}
	if !kube.DeploymentAllowsPatch(dep, "resources") {
		return d, false
	}
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return d, false
	}

	// Use the first container (common case for single-container Deployments).
	c := dep.Spec.Template.Spec.Containers[0]
	newLimit := computeBumpedMemoryLimit(c.Resources.Limits[corev1.ResourceMemory])

	transformed := d
	transformed.Action = model.ActionPatchResources
	// The escalation is a deterministic, opt-in-gated policy transform, not an
	// LLM guess: the confidence the model attached to the rejected restart is
	// irrelevant, so lift it to the patch threshold and let the real gates
	// (global flag + annotation) decide.
	if transformed.Confidence < cfg.PatchConfidenceThreshold {
		transformed.Confidence = cfg.PatchConfidenceThreshold
	}
	if transformed.Parameters == nil {
		transformed.Parameters = map[string]string{}
	}
	transformed.Parameters["deployment_name"] = depName
	transformed.Parameters["container"] = c.Name
	transformed.Parameters["memory_limit"] = newLimit
	return transformed, true
}

// tryAutoPatchProbeOnUnhealthy converts a blocked restart_deployment into a
// patch_probe when the source event was a probe failure (reason Unhealthy)
// and the target Deployment opts in to "probe" patching. Restarting never
// fixes a readiness/liveness probe that is too strict for a workload's natural
// readiness flapping — the replacement pod flaps in exactly the same way.
// Mirrors tryAutoPatchResourcesOnOOM so a weak local model that disobeys the
// "NEVER restart on Unhealthy" rule still lands on the correct remediation:
// the probe is relaxed (longer period, higher failure threshold) so brief
// unready windows stop tripping it. Returns (transformed, true) on success,
// (untouched, false) otherwise.
func tryAutoPatchProbeOnUnhealthy(ctx context.Context, cs kubernetes.Interface, d model.Decision, cfg config.AgentConfig) (model.Decision, bool) {
	if !cfg.AllowPatchProbe {
		return d, false
	}

	depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
	if err != nil {
		return d, false
	}

	dep, err := cs.AppsV1().Deployments(d.Namespace).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return d, false
	}
	if !kube.DeploymentAllowsPatch(dep, "probe") {
		return d, false
	}

	container, probe, ok := chooseProbeFromDeployment(dep)
	if !ok {
		return d, false
	}

	transformed := d
	transformed.Action = model.ActionPatchProbe
	if transformed.Confidence < cfg.PatchConfidenceThreshold {
		transformed.Confidence = cfg.PatchConfidenceThreshold
	}
	if transformed.Parameters == nil {
		transformed.Parameters = map[string]string{}
	}
	transformed.Parameters["deployment_name"] = depName
	transformed.Parameters["container"] = container
	transformed.Parameters["probe"] = probe
	// Tolerant defaults: 6 consecutive failures at a 15s period means ~90s of
	// continuous unreadiness before the pod leaves the Service, well above the
	// short flaps that generate the Unhealthy noise.
	transformed.Parameters["failure_threshold"] = "6"
	transformed.Parameters["period_seconds"] = "15"
	transformed.Parameters["timeout_seconds"] = "5"
	return transformed, true
}

// deriveFallbackImage resolves the decision's target Deployment, picks the
// container named in params (or the first one) and derives a replacement
// image for it: the newest tag its registry advertises when tag discovery is
// on (falling back gracefully on any registry failure), else the configured
// fallback tag. Returns ("", false) when nothing can be derived — the
// decision then fails downstream with the regular "parameters.image missing"
// guard.
func deriveFallbackImage(ctx context.Context, cs kubernetes.Interface, d model.Decision, cfg config.AgentConfig) (string, bool) {
	depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
	if err != nil {
		return "", false
	}
	dep, err := cs.AppsV1().Deployments(d.Namespace).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return "", false
	}
	idx := 0
	if name := strings.TrimSpace(d.Parameters["container"]); name != "" {
		for i, c := range containers {
			if c.Name == name {
				idx = i
				break
			}
		}
	}
	current := containers[idx].Image

	// Preferred: the newest tag advertised by the image's own registry
	// (excludes the broken tag the image currently carries).
	if cfg.ImageTagDiscovery {
		if tag, err := registry.NewestTag(ctx, current); err == nil {
			if img, rerr := kube.RetagImage(current, tag); rerr == nil {
				slog.Info("set_deployment_image: newest tag discovered from the registry",
					"current", current, "tag", tag)
				return img, true
			}
		} else {
			slog.Info("image tag discovery failed; using the fallback tag",
				"image", current, "error", err)
		}
	}

	if cfg.ImageFallbackTag == "" {
		return "", false
	}
	img, err := kube.RetagImage(current, cfg.ImageFallbackTag)
	if err != nil {
		return "", false
	}
	return img, true
}

// chooseProbeFromDeployment picks the container+probe to relax, preferring
// readiness (it governs Service membership and is the usual source of
// Unhealthy noise) and falling back to liveness. Shared by the Unhealthy
// auto-escalation and the patch_probe executor fallback.
func chooseProbeFromDeployment(dep *appsv1.Deployment) (container, probe string, ok bool) {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.ReadinessProbe != nil {
			return c.Name, "readiness", true
		}
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.LivenessProbe != nil {
			return c.Name, "liveness", true
		}
	}
	return "", "", false
}

// hasResourceParams reports whether params carries at least one of the four
// quantities PatchDeploymentResources accepts.
func hasResourceParams(params map[string]string) bool {
	for _, k := range []string{"cpu_request", "memory_request", "cpu_limit", "memory_limit"} {
		if strings.TrimSpace(params[k]) != "" {
			return true
		}
	}
	return false
}

// withSchedulableResourceDefaults returns a copy of params filled with the
// conservative, schedulable-anywhere values that prompt rule 2 instructs the
// model to use on FailedScheduling. The map is copied before mutation so the
// recorded decision stays intact.
func withSchedulableResourceDefaults(params map[string]string) map[string]string {
	out := make(map[string]string, len(params)+4)
	for k, v := range params {
		out[k] = v
	}
	out["cpu_request"] = "100m"
	out["memory_request"] = "64Mi"
	out["cpu_limit"] = "500m"
	out["memory_limit"] = "256Mi"
	return out
}

// computeBumpedMemoryLimit returns a safe "next step" memory limit: 4x the
// current value (or 512Mi when the current is unset or smaller), capped at
// the package-wide MaxMemoryQuantity so the subsequent validator does not
// reject it. The 512Mi floor leaves real headroom over a workload that was
// OOMKilled near a small limit (e.g. a container touching ~256MB under a
// 32Mi limit): a value only marginally above the old limit just OOMs again,
// which is exactly why a 256Mi target failed to resolve the memory-hog
// scenario reliably.
func computeBumpedMemoryLimit(current resource.Quantity) string {
	floor := resource.MustParse("512Mi")
	var target resource.Quantity
	if current.IsZero() {
		target = floor
	} else {
		bumped := current.DeepCopy()
		bumped.Set(current.Value() * 4)
		if bumped.Cmp(floor) < 0 {
			target = floor
		} else {
			target = bumped
		}
	}
	if target.Cmp(kube.MaxMemoryQuantity) > 0 {
		target = kube.MaxMemoryQuantity.DeepCopy()
	}
	return target.String()
}

// isOOMContext reports whether the incident context (pod status summary,
// lastTerminated reason or event message attached as "extra") shows an OOM
// kill. Same signal MaybeBlockRestartOnOOMKilled uses.
func isOOMContext(extra string) bool {
	low := strings.ToLower(extra)
	return strings.Contains(low, "oomkilled") || strings.Contains(low, "exit=137")
}

// withOOMMemoryFloor returns params with memory_limit raised to a safe floor
// for an OOM incident. A small local model frequently proposes patch_resources
// with a memory_limit only marginally above the current one — or omits it
// entirely — which passes the [16Mi,16Gi] validator but lets the container OOM
// again immediately. That is why memory-hog kept failing even after the floor
// was added: a *directly* chosen patch_resources never went through it (only
// the restart→patch_resources auto-escalation did). We reuse the same floor
// (computeBumpedMemoryLimit of the container's current limit: 4x current, min
// 512Mi) and only raise toward it — a higher LLM value is preserved. The map
// is copied before mutation so the recorded decision is left intact.
func withOOMMemoryFloor(ctx context.Context, cs kubernetes.Interface, ns, depName string, params map[string]string) map[string]string {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return params
	}

	// Current memory limit of the target container (first one when the param
	// is empty or does not match), used to size the floor.
	var current resource.Quantity
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) > 0 {
		idx := 0
		if name := strings.TrimSpace(params["container"]); name != "" {
			for i, c := range containers {
				if c.Name == name {
					idx = i
					break
				}
			}
		}
		current = containers[idx].Resources.Limits[corev1.ResourceMemory]
	}

	floor := resource.MustParse(computeBumpedMemoryLimit(current))
	if raw := strings.TrimSpace(params["memory_limit"]); raw != "" {
		if q, perr := resource.ParseQuantity(raw); perr == nil && q.Cmp(floor) >= 0 {
			return params // already adequate
		}
	}

	out := make(map[string]string, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out["memory_limit"] = floor.String()
	// Never let memory_request exceed the new limit (the validator rejects
	// that); cap it at half the floor, matching the prompt guidance.
	if raw := strings.TrimSpace(out["memory_request"]); raw != "" {
		if q, perr := resource.ParseQuantity(raw); perr == nil && q.Cmp(floor) > 0 {
			half := floor.DeepCopy()
			half.Set(floor.Value() / 2)
			out["memory_request"] = half.String()
		}
	}
	return out
}

// formatTriBool renders a tri-state *bool for logs: "auto" when nil.
func formatTriBool(b *bool) string {
	if b == nil {
		return "auto"
	}
	return strconv.FormatBool(*b)
}

// decider abstracts the LLM client so the poll loop can be exercised in
// tests with canned decisions. *ollama.Client is the production implementation.
type decider interface {
	Decide(ctx context.Context, prompt string) (model.Decision, error)
}

// eventLister returns the Warning events to evaluate in one poll cycle.
type eventLister func(ctx context.Context) ([]corev1.Event, error)

// newWarningEventSource returns an eventLister backed by a shared informer:
// one long-lived WATCH keeps a local cache of Warning events, so every poll
// reads from memory instead of re-LISTing all events cluster-wide. When the
// cache cannot sync (RBAC gap, API flakiness) it falls back to direct LISTs
// so remediation never stalls behind a broken watch. The informer stops with
// ctx, i.e. when leadership is lost.
func newWarningEventSource(ctx context.Context, cs kubernetes.Interface) eventLister {
	directList := func(c context.Context) ([]corev1.Event, error) {
		list, err := cs.CoreV1().Events("").List(c, metav1.ListOptions{FieldSelector: "type=Warning"})
		if err != nil {
			return nil, err
		}
		return list.Items, nil
	}

	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = "type=Warning"
		}))
	informer := factory.Core().V1().Events().Informer()
	factory.Start(ctx.Done())

	syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !k8scache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		slog.Warn("event informer did not sync; falling back to a direct LIST per poll")
		return directList
	}
	slog.Info("event informer synced; polling from the local cache")
	return func(context.Context) ([]corev1.Event, error) {
		objs := informer.GetStore().List()
		out := make([]corev1.Event, 0, len(objs))
		for _, o := range objs {
			if e, ok := o.(*corev1.Event); ok {
				out = append(out, *e)
			}
		}
		return out, nil
	}
}

// loopHealth tracks the liveness of the polling loop for /healthz. Only the
// replica holding leadership is expected to poll, so staleness is checked
// exclusively while leading: followers always report healthy.
type loopHealth struct {
	leading  atomic.Bool
	lastPoll atomic.Int64 // unix seconds of the last completed poll
}

func (h *loopHealth) markLeading(v bool) {
	if h == nil {
		return
	}
	if v {
		h.stamp()
	}
	h.leading.Store(v)
}

func (h *loopHealth) stamp() {
	if h == nil {
		return
	}
	h.lastPoll.Store(time.Now().Unix())
}

// healthy reports whether the loop completed a poll recently enough. A loop
// that has never polled while leading is still healthy: markLeading stamps
// the clock so slow startups (informer sync, first LLM call) do not flap
// the liveness probe.
func (h *loopHealth) healthy(staleAfter time.Duration) bool {
	if h == nil || !h.leading.Load() {
		return true
	}
	last := h.lastPoll.Load()
	if last == 0 {
		return true
	}
	return time.Since(time.Unix(last, 0)) <= staleAfter
}

// signalBackoffTTL escalates the dedup window of a signal that keeps firing:
// 1x, 2x, 4x, then capped at 8x the base TTL. Repeated remediation attempts
// against the same incident space out instead of burning an LLM call (and a
// cluster mutation) every base window.
func signalBackoffTTL(base time.Duration, attempts int) time.Duration {
	const maxFactor = 8
	factor := 1
	for i := 1; i < attempts && factor < maxFactor; i++ {
		factor *= 2
	}
	return base * time.Duration(factor)
}

// toSet builds a quick-lookup set from a slice of strings.
func toSet(in []string) map[string]bool {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]bool, len(in))
	for _, v := range in {
		out[v] = true
	}
	return out
}

// canonicalReason collapses families of Kubernetes event reasons that
// represent the same underlying incident. Examples:
//   - "Failed" (on image pull), "ErrImagePull", "ImagePullBackOff" all
//     describe one failed pull sequence — dedup them as "ImagePullFailure".
//
// Keep the original reason in the event/prompt; only the dedup key uses
// this canonical form.
func canonicalReason(reason, message string) string {
	trimmed := strings.TrimSpace(reason)
	r := strings.ToLower(trimmed)
	switch r {
	case "errimagepull", "imagepullbackoff":
		return "ImagePullFailure"
	case "failed":
		if strings.Contains(strings.ToLower(message), "pull") {
			return "ImagePullFailure"
		}
	}
	// Return the trimmed form so "Failed" and "  Failed  " collapse to the
	// same dedup key. Case is preserved for human readability in logs.
	return trimmed
}

// runLoopWithStore runs the event polling loop against injected
// collaborators: the dedup Store (created once in main and reused across
// leader re-elections so a flapping lease never leaks a Redis pool), the
// LLM decider, and the event lister (informer-backed in production).
// Tests inject stubs for all three.
func runLoopWithStore(ctx context.Context, cs kubernetes.Interface, llm decider, cfg config.AgentConfig, m *metrics.Recorder, notifier notify.Notifier, listEvents eventLister, store dedup.Store, health *loopHealth, recorder *webui.RecentDecisionRecorder) {
	health.markLeading(true)
	defer health.markLeading(false)

	dedupeTTL := time.Duration(cfg.DedupeTTLSec) * time.Second
	seenTTL := time.Duration(cfg.EventSeenTTLSec) * time.Second
	if seenTTL <= 0 {
		seenTTL = time.Hour
	}
	// attemptWindow bounds how long the per-signal attempt counter survives
	// without new increments: well past the maximum backoff (8x dedupeTTL),
	// so consecutive failed attempts accumulate, while an incident that has
	// stayed quiet this long starts again from a clean slate.
	attemptWindow := 12 * dedupeTTL

	// llmCallBudget is the minimum poll-context headroom required to launch
	// one more LLM call: the HTTP timeout of a single attempt plus a margin
	// for the rate limiter and the Kubernetes lookups around it. Below this,
	// a call is guaranteed to die mid-flight on the poll deadline.
	llmCallBudget := time.Duration(cfg.OllamaHTTPTimeoutSec)*time.Second + 15*time.Second
	if cfg.OllamaHTTPTimeoutSec <= 0 {
		llmCallBudget = 180*time.Second + 15*time.Second // mirrors the client default
	}

	// Build O(1) namespace lookup tables once. include is empty -> "all
	// allowed except excluded"; non-empty include -> only listed allowed.
	include := toSet(cfg.IncludeNamespaces)
	exclude := toSet(cfg.ExcludeNamespaces)

	minSev := model.ParseSeverity(cfg.MinSeverity)

	slog.Info("agent started",
		"model", cfg.Model,
		"baseURL", cfg.BaseURL,
		"dryRun", cfg.DryRun,
		"minSeverity", string(minSev),
		"allowImageUpdates", cfg.AllowImageUpdates,
		"imageUpdateThreshold", cfg.ImageUpdateThreshold,
		"allowPatchProbe", cfg.AllowPatchProbe,
		"allowPatchResources", cfg.AllowPatchResources,
		"allowPatchRegistry", cfg.AllowPatchRegistry,
		"patchConfidenceThreshold", cfg.PatchConfidenceThreshold,
		"dedupeTTLSec", cfg.DedupeTTLSec,
		"eventSeenTTLSec", cfg.EventSeenTTLSec,
		"maxEventsPerPoll", cfg.MaxEventsPerPoll,
		"includeNamespaces", cfg.IncludeNamespaces,
		"excludeNamespaces", cfg.ExcludeNamespaces,
		"podLogTailLines", cfg.PodLogTailLines,
		"ollamaRPS", cfg.OllamaRPS,
		"ollamaHTTPTimeoutSec", cfg.OllamaHTTPTimeoutSec,
		"pollContextTimeoutSec", cfg.PollContextTimeoutSec,
		"ollamaThink", formatTriBool(cfg.OllamaThink),
		"signalMaxAttempts", cfg.SignalMaxAttempts,
		"metricsAddr", cfg.MetricsAddr,
		"buildFeatures", "dedup,infer-dep-from-podname,block-restart-on-unhealthy,patch_probe,patch_resources,patch_registry,auto-escalate-oom,auto-escalate-unhealthy,severity-exempt-optin,ollama-think-toggle,target-pinning,signal-circuit-breaker,event-informer,explicit-param-schema,exec-param-fallbacks,poll-time-budget,image-tag-discovery",
	)

	ticker := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
	defer ticker.Stop()

	poll := func() {
		// Stamp completion even on early returns: a poll that ran (and, say,
		// failed to list) still proves the loop is alive for /healthz.
		defer health.stamp()

		pollTimeout := time.Duration(cfg.PollContextTimeoutSec) * time.Second
		if pollTimeout <= 0 {
			pollTimeout = 5 * time.Minute
		}
		pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()

		// Warning events only; in production this reads from the informer's
		// local cache (one background WATCH) instead of a cluster-wide LIST.
		list, err := listEvents(pollCtx)
		if err != nil {
			slog.Error("list events failed", "error", err)
			return
		}

		now := time.Now()
		store.Evict(now, dedupeTTL, seenTTL)

		processed := 0
		for _, e := range list {
			// Cheap in-memory filters first, before any dedup-store round trip:
			// non-Warning events (defence in depth — the List already
			// field-selects type=Warning) and namespaces outside the configured
			// scope. This keeps a persistent dedup backend (Redis SetNX) from
			// being hit once per cluster-wide event.
			if !strings.EqualFold(e.Type, "Warning") {
				m.EventsSkipped.Add(1)
				continue
			}
			// Namespace filter: skip kube-system and friends, plus anything
			// not in the include allowlist when present.
			if exclude[e.Namespace] {
				m.EventsSkipped.Add(1)
				continue
			}
			if len(include) > 0 && !include[e.Namespace] {
				m.EventsSkipped.Add(1)
				continue
			}

			// Per-poll work budget, enforced BEFORE the event is marked seen
			// so the deferral is honest: an event skipped here is genuinely
			// retried on the next poll instead of being silently dropped
			// behind its resourceVersion. Two limits apply: the configured
			// event cap, and the remaining time in the poll context — once a
			// single LLM round-trip no longer fits, launching it would only
			// die mid-flight ("rate limiter: context deadline exceeded").
			if cfg.MaxEventsPerPoll > 0 && processed >= cfg.MaxEventsPerPoll {
				slog.Info("max events per poll reached; remaining events deferred to the next poll",
					"cap", cfg.MaxEventsPerPoll)
				m.EventsSkipped.Add(1)
				break
			}
			if deadline, ok := pollCtx.Deadline(); ok && time.Until(deadline) < llmCallBudget {
				slog.Info("poll time budget exhausted; remaining events deferred to the next poll",
					"remaining", time.Until(deadline).Round(time.Second),
					"requiredPerCall", llmCallBudget)
				m.EventsSkipped.Add(1)
				break
			}

			key := e.Namespace + "/" + e.Name + "/" + e.ResourceVersion
			if !store.MarkSeen(key, now, seenTTL) {
				m.EventsSkipped.Add(1)
				continue
			}

			// Resolve a stable dedup target: for pods owned by a Deployment,
			// collapse all pod generations onto the deployment so flaky
			// workloads emitting the same reason across multiple pods count
			// as one signal.
			extra := ""
			dedupKind := e.InvolvedObject.Kind
			dedupName := e.InvolvedObject.Name
			if strings.EqualFold(e.InvolvedObject.Kind, "Deployment") {
				extra = kube.DeploymentSnapshot(pollCtx, cs, e.Namespace, e.InvolvedObject.Name)
			}
			if strings.EqualFold(e.InvolvedObject.Kind, "Pod") {
				if depName, err := kube.ResolveDeploymentFromPod(pollCtx, cs, e.Namespace, e.InvolvedObject.Name); err == nil {
					extra = "Resolved owner deployment: " + depName + "\n" + kube.DeploymentSnapshot(pollCtx, cs, e.Namespace, depName)
					dedupKind = "Deployment"
					dedupName = depName
				} else if depName, ok := kube.InferDeploymentFromPodName(pollCtx, cs, e.Namespace, e.InvolvedObject.Name); ok {
					// Pod is gone; rely on the naming convention so stale
					// events from rolled-over ReplicaSets still collapse into
					// a single dedup key.
					extra = "Inferred owner deployment from pod name: " + depName + "\n" + kube.DeploymentSnapshot(pollCtx, cs, e.Namespace, depName)
					dedupKind = "Deployment"
					dedupName = depName
				}
				// Add pod-level state (OOMKilled, exit codes) so the LLM can
				// tell a resource-pressure crash from a probe misconfiguration.
				if ps := kube.PodStatusSummary(pollCtx, cs, e.Namespace, e.InvolvedObject.Name); ps != "" {
					if extra != "" {
						extra += "\n"
					}
					extra += ps
				}
			}

			signal := e.Namespace + "|" + dedupKind + "|" + dedupName + "|" + canonicalReason(e.Reason, e.Message)
			if store.IsSignalFresh(signal, now, dedupeTTL) {
				m.EventsSkipped.Add(1)
				continue
			}

			// Circuit breaker: a signal that already consumed SignalMaxAttempts
			// remediation attempts without resolving stops burning LLM calls
			// and cluster mutations. It is parked for the maximum backoff
			// window and surfaced (dashboard + notification) as needing a
			// human. The counter expires after attemptWindow of silence, so a
			// later regression of the same workload starts fresh.
			if cfg.SignalMaxAttempts > 0 {
				if attempts := store.Attempts(signal, now); attempts >= cfg.SignalMaxAttempts {
					parkTTL := signalBackoffTTL(dedupeTTL, attempts+1)
					store.MarkSignal(signal, now, parkTTL)
					m.EventsSkipped.Add(1)
					giveUp := model.Decision{
						Action:        model.ActionMarkForManualFix,
						Severity:      string(model.SeverityHigh),
						Summary:       fmt.Sprintf("circuit breaker: %d remediation attempts on %s/%s (%s) without resolution", attempts, e.Namespace, dedupName, e.Reason),
						ProbableCause: "automatic remediation is not resolving this incident",
						Namespace:     e.Namespace,
						ResourceKind:  e.InvolvedObject.Kind,
						ResourceName:  e.InvolvedObject.Name,
						Parameters:    map[string]string{},
					}
					slog.Warn("signal exceeded max remediation attempts; manual fix required",
						"signal", signal, "attempts", attempts, "parkedFor", parkTTL)
					recorder.Record(e.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, giveUp, "blocked", "max remediation attempts reached")
					notifier.NotifyDecision(pollCtx, notify.DecisionResult{
						Namespace:    e.Namespace,
						Kind:         e.InvolvedObject.Kind,
						Name:         e.InvolvedObject.Name,
						EventReason:  e.Reason,
						EventMessage: e.Message,
						Decision:     giveUp,
						ExecutionErr: fmt.Errorf("max remediation attempts (%d) reached; manual intervention required", attempts),
					})
					continue
				}
			}

			processed++
			m.EventsProcessed.Add(1)

			prompt := policy.BuildPrompt(
				e.Namespace,
				e.InvolvedObject.Kind,
				e.InvolvedObject.Name,
				e.Type,
				e.Reason,
				e.Message,
				extra,
			)

			d, err := llm.Decide(pollCtx, prompt)
			if err != nil {
				m.DecisionErrors.Add(1)
				slog.Error("ollama decision failed",
					"ns", e.Namespace, "event", e.Name, "error", err)
				// Deliberately NOT marked: a transient LLM failure (timeout,
				// 5xx, Ollama down) must not burn the dedup window and silence
				// the incident for DEDUPE_TTL_SECONDS. The signal is retried
				// as soon as the incident emits its next event update (kubelet
				// bumps count/resourceVersion on recurring events); the rate
				// limiter and MaxEventsPerPoll bound the cost.
				continue
			}

			// Latency/request counters are recorded per HTTP attempt inside the
			// Ollama client via the onRequest metric hook (wired in main).
			m.RecordDecision(string(d.Action))

			// The LLM proposes, the event anchors: a decision may only target
			// the object (and namespace) of the event that produced it. The
			// namespace filters above ran against the EVENT namespace; without
			// this pinning a hallucinated — or prompt-injected — namespace or
			// deployment_name in the model output could retarget the action
			// onto workloads outside the configured scope.
			if d.Namespace != "" && d.Namespace != e.Namespace {
				slog.Warn("decision namespace differs from event namespace; overriding",
					"decision", d.Namespace, "event", e.Namespace)
			}
			d.Namespace = e.Namespace
			d.ResourceKind = e.InvolvedObject.Kind
			d.ResourceName = e.InvolvedObject.Name
			if d.Parameters == nil {
				d.Parameters = map[string]string{}
			}
			if strings.EqualFold(dedupKind, "Deployment") && dedupName != "" {
				// Owner resolution already identified the target Deployment;
				// never let a divergent deployment_name retarget the action.
				if v := strings.TrimSpace(d.Parameters["deployment_name"]); v != "" && v != dedupName {
					slog.Warn("decision deployment_name differs from resolved owner; overriding",
						"decision", v, "resolved", dedupName)
				}
				d.Parameters["deployment_name"] = dedupName
				delete(d.Parameters, "deployment")
			}

			severity := model.ParseSeverity(d.Severity)

			slog.Info("decision",
				"summary", d.Summary,
				"severity", string(severity),
				"action", d.Action,
				"ns", d.Namespace,
				"kind", d.ResourceKind,
				"name", d.ResourceName,
				"confidence", d.Confidence,
				"probable_cause", d.ProbableCause,
				"reason", d.Reason,
				"params", d.Parameters,
			)

			// MIN_SEVERITY is a noise filter. Opt-in actions (patch_*,
			// set_deployment_image) are exempt: they already pass stricter,
			// operator-controlled gates (global flag + per-Deployment annotation
			// + confidence threshold), so a weak model that under-rates a
			// genuinely actionable incident — e.g. tagging a flaky-probe fix
			// "low" — must not have its remediation silently discarded here.
			if !severity.MeetsMinimum(minSev) && !d.Action.IsOperatorGated() {
				slog.Info("skipping decision below minimum severity",
					"severity", string(severity),
					"minSeverity", string(minSev),
					"action", d.Action,
				)
				m.EventsSkipped.Add(1)
				// A decision WAS reached (just noise-filtered): mark the base
				// dedup window, but do not count a remediation attempt.
				store.MarkSignal(signal, now, dedupeTTL)
				recorder.Record(e.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, d, "skipped", "below minSeverity")
				continue
			}

			// Count this remediation attempt and escalate the signal's dedup
			// window accordingly (1x → 8x the base TTL). The counter lives in
			// the dedup store — with the Redis backend it survives restarts —
			// and resets after attemptWindow of silence. With the breaker
			// disabled (SignalMaxAttempts=0) the window stays at the base TTL.
			attempts := 1
			if cfg.SignalMaxAttempts > 0 {
				attempts = store.IncrAttempt(signal, now, attemptWindow)
			}
			store.MarkSignal(signal, now, signalBackoffTTL(dedupeTTL, attempts))

			execErr := executeDecision(pollCtx, cs, d, cfg, e.Reason, extra)
			if execErr != nil {
				m.ExecutionErrors.Add(1)
				slog.Error("execute decision failed", "action", d.Action, "error", execErr)
			}

			// Classify the outcome for the dashboard ring buffer. Policy
			// validators (the ones in internal/policy) signal the agent
			// declined to act with non-error returns shaped like "blocked
			// by policy"; we surface that distinct from a real failure.
			outcome := "success"
			outcomeErr := ""
			if execErr != nil {
				if strings.Contains(strings.ToLower(execErr.Error()), "policy") ||
					strings.Contains(strings.ToLower(execErr.Error()), "blocked") ||
					strings.Contains(strings.ToLower(execErr.Error()), "opt in") {
					outcome = "blocked"
				} else {
					outcome = "error"
				}
				outcomeErr = execErr.Error()
			}
			recorder.Record(e.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, d, outcome, outcomeErr)

			// Fire-and-forget notification so SMTP latency never blocks
			// the poll loop. The notifier filters on NOTIFY_MIN_SEVERITY
			// and silently skips when SMTP is not configured.
			notifier.NotifyDecision(pollCtx, notify.DecisionResult{
				Namespace:    e.Namespace,
				Kind:         e.InvolvedObject.Kind,
				Name:         e.InvolvedObject.Name,
				EventReason:  e.Reason,
				EventMessage: e.Message,
				Decision:     d,
				ExecutionErr: execErr,
			})
		}
	}

	// First poll immediately
	poll()

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down gracefully", "reason", ctx.Err())
			return
		case <-ticker.C:
			poll()
		}
	}
}

func main() {
	// Structured JSON logging for Kubernetes
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := config.LoadFromEnv()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("failed to load in-cluster config", "error", err)
		os.Exit(1)
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		slog.Error("failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	ollamaClient := ollama.NewClient(cfg.BaseURL, cfg.Model, cfg.OllamaRPS, cfg.OllamaMaxRetries, cfg.OllamaTLSSkipVerify, cfg.OllamaHTTPTimeoutSec)
	// Reasoning mode (OLLAMA_THINK). Defaults to off: thinking models
	// (qwen3.x, gemma4) would otherwise spend minutes per call emitting
	// reasoning tokens on CPU and time out before producing any JSON.
	ollamaClient.SetThink(cfg.OllamaThink)
	m := metrics.New()

	// Wire the Ollama client's metric hooks into the recorder. onRequest fires
	// once per HTTP attempt with its latency, so remediator_ollama_requests_total
	// and the average-latency gauge count real per-attempt traffic (not just
	// successes) and stay consistent with remediator_ollama_errors_total.
	ollamaClient.SetMetricsHooks(
		func() { m.OllamaRateLimited.Add(1) },
		func() { m.OllamaErrors.Add(1) },
		func(d time.Duration) { m.RecordOllamaLatency(d) },
	)

	notifier := notify.New(notify.SMTPConfig{
		Host:        cfg.NotifySMTPHost,
		Port:        cfg.NotifySMTPPort,
		User:        cfg.NotifySMTPUser,
		Password:    cfg.NotifySMTPPassword,
		From:        cfg.NotifyFrom,
		To:          cfg.NotifyTo,
		MinSeverity: model.ParseSeverity(cfg.NotifyMinSeverity),
	})

	// Loop heartbeat: /healthz turns 503 when the leader's poll loop stops
	// completing cycles, so a liveness probe can restart a wedged agent.
	// The threshold tolerates one full poll running to its context timeout
	// plus a few idle intervals before declaring the loop stalled.
	health := &loopHealth{}
	staleAfter := time.Duration(cfg.PollContextTimeoutSec)*time.Second +
		3*time.Duration(cfg.PollSec)*time.Second

	// Start metrics server in background
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !health.healthy(staleAfter) {
			http.Error(w, "poll loop stalled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	metricsSrv := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("metrics server starting", "addr", cfg.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	// Graceful shutdown: cancel context on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Ring buffer of recent decisions, surfaced by the GUI dashboard.
	// Allocated unconditionally so runLoop can call Record() without nil
	// checks; size is small (20 entries ~few KB). With the Redis dedup
	// backend the buffer is also mirrored into Redis, so the dashboard
	// history survives pod restarts and leader failovers.
	var decisionRecorder *webui.RecentDecisionRecorder
	if strings.EqualFold(cfg.DedupBackend, "redis") {
		decisionRecorder = webui.NewRecentDecisionRecorderRedis(20, webui.RedisDecisionOptions{
			Addr:      cfg.RedisAddr,
			Password:  cfg.RedisPassword,
			DB:        cfg.RedisDB,
			KeyPrefix: cfg.RedisKeyPrefix,
		})
	} else {
		decisionRecorder = webui.NewRecentDecisionRecorder(20)
	}
	defer func() { _ = decisionRecorder.Close() }()

	// Optional admin GUI. Runs alongside the polling loop and is independent
	// of leader election: every replica serves the GUI so the dashboard stays
	// reachable even when the active leader is elsewhere.
	if cfg.WebUIEnabled {
		dynClient, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			slog.Error("webui: dynamic client init failed", "error", err)
		} else {
			discoveryClient := memcached.NewMemCacheClient(discovery.NewDiscoveryClientForConfigOrDie(restCfg))
			mapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
			webuiSrv, err := webui.New(webui.Options{
				Addr:              cfg.WebUIAddr,
				Username:          cfg.WebUIUsername,
				Password:          cfg.WebUIPassword,
				Namespace:         cfg.AgentNamespace,
				DeploymentName:    cfg.AgentDeploymentName,
				ConfigMapName:     cfg.AgentConfigMapName,
				SecretName:        cfg.AgentSecretName,
				SandboxNamespaces: cfg.ScenarioSandboxNamespaces,
				PodLogTailLines:   cfg.PodLogTailLines,
				IncludeNamespaces: cfg.IncludeNamespaces,
				OllamaBaseURL:     cfg.BaseURL,
				DedupBackend:      cfg.DedupBackend,
				RedisAddr:         cfg.RedisAddr,
				RedisPassword:     cfg.RedisPassword,
				RedisDB:           cfg.RedisDB,
				Decisions:         decisionRecorder,
				Clientset:         cs,
				DynamicClient:     dynClient,
				RESTMapper:        mapper,
				RESTConfig:        restCfg,
			})
			if err != nil {
				slog.Error("webui: init failed", "error", err)
			} else {
				go func() {
					if err := webuiSrv.ListenAndServe(ctx); err != nil {
						slog.Error("webui: server stopped", "error", err)
					}
				}()
			}
		}
	}

	// Dedup store: created once and reused across leader re-elections so a
	// flapping lease never leaks a Redis connection pool (and in-memory dedup
	// state survives within the process). Closed on shutdown.
	store, err := dedup.NewStore(dedup.BackendConfig{
		Backend:        cfg.DedupBackend,
		RedisAddr:      cfg.RedisAddr,
		RedisPassword:  cfg.RedisPassword,
		RedisDB:        cfg.RedisDB,
		RedisKeyPrefix: cfg.RedisKeyPrefix,
	})
	if err != nil {
		slog.Error("dedup store init failed, falling back to in-memory", "error", err)
		store = dedup.NewMemoryStore()
	} else {
		slog.Info("dedup store initialised", "backend", cfg.DedupBackend)
	}
	if closer, ok := store.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}

	run := func(ctx context.Context) {
		runLoopWithStore(ctx, cs, ollamaClient, cfg, m, notifier, newWarningEventSource(ctx, cs), store, health, decisionRecorder)
	}

	if cfg.LeaderElection {
		hostname, _ := os.Hostname()
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      cfg.LeaseName,
				Namespace: cfg.LeaseNamespace,
			},
			Client: cs.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: hostname,
			},
		}

		slog.Info("leader election enabled", "identity", hostname, "lease", cfg.LeaseName, "namespace", cfg.LeaseNamespace)

		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: run,
				OnStoppedLeading: func() {
					slog.Info("lost leadership, stopping agent")
				},
				OnNewLeader: func(identity string) {
					if identity != hostname {
						slog.Info("new leader elected", "leader", identity)
					}
				},
			},
		})
	} else {
		run(ctx)
	}

	slog.Info("agent stopped")
}
