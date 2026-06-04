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
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	memcached "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
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

	case model.ActionDeleteFailedPod, model.ActionDeleteAndRecreate:
		if strings.EqualFold(d.ResourceKind, "Pod") {
			return kube.DeletePod(ctx, cs, d.Namespace, d.ResourceName, cfg.DryRun)
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
		return kube.PatchDeploymentProbe(ctx, cs, d.Namespace, depName, d.Parameters["container"], d.Parameters["probe"], fields, cfg.DryRun)

	case model.ActionPatchResources:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters)
		if err != nil {
			return err
		}
		return kube.PatchDeploymentResources(ctx, cs, d.Namespace, depName, d.Parameters["container"], d.Parameters, cfg.DryRun)

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

	// Find a container with a probe to relax, preferring readiness since it
	// governs Service membership and is the usual source of Unhealthy noise.
	container, probe := "", ""
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.ReadinessProbe != nil {
			container, probe = c.Name, "readiness"
			break
		}
	}
	if probe == "" {
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.LivenessProbe != nil {
				container, probe = c.Name, "liveness"
				break
			}
		}
	}
	if probe == "" {
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

func runLoop(ctx context.Context, cs kubernetes.Interface, ollamaClient *ollama.Client, cfg config.AgentConfig, m *metrics.Recorder, notifier notify.Notifier, recorder *webui.RecentDecisionRecorder) {
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
	runLoopWithStore(ctx, cs, ollamaClient, cfg, m, notifier, store, recorder)
}

// runLoopWithStore is the injectable form of runLoop: tests and future
// alternative backends (Redis, SQLite, ConfigMap) can pass a custom Store
// so dedup state survives across agent restarts.
func runLoopWithStore(ctx context.Context, cs kubernetes.Interface, ollamaClient *ollama.Client, cfg config.AgentConfig, m *metrics.Recorder, notifier notify.Notifier, cache dedup.Store, recorder *webui.RecentDecisionRecorder) {

	dedupeTTL := time.Duration(cfg.DedupeTTLSec) * time.Second
	seenTTL := time.Duration(cfg.EventSeenTTLSec) * time.Second
	if seenTTL <= 0 {
		seenTTL = time.Hour
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
		"metricsAddr", cfg.MetricsAddr,
		"buildFeatures", "dedup,infer-dep-from-podname,block-restart-on-unhealthy,patch_probe,patch_resources,patch_registry,auto-escalate-oom,auto-escalate-unhealthy,severity-exempt-optin",
	)

	ticker := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
	defer ticker.Stop()

	poll := func() {
		pollTimeout := time.Duration(cfg.PollContextTimeoutSec) * time.Second
		if pollTimeout <= 0 {
			pollTimeout = 5 * time.Minute
		}
		pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()

		list, err := cs.CoreV1().Events("").List(pollCtx, metav1.ListOptions{})
		if err != nil {
			slog.Error("list events failed", "error", err)
			return
		}

		now := time.Now()
		cache.Evict(now, dedupeTTL, seenTTL)

		processed := 0
		for _, e := range list.Items {
			key := e.Namespace + "/" + e.Name + "/" + e.ResourceVersion
			if !cache.MarkSeen(key, now, seenTTL) {
				m.EventsSkipped.Add(1)
				continue
			}

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
			if cache.IsSignalFresh(signal, now, dedupeTTL) {
				m.EventsSkipped.Add(1)
				continue
			}

			if cfg.MaxEventsPerPoll > 0 && processed >= cfg.MaxEventsPerPoll {
				slog.Info("max events per poll reached; remaining events deferred",
					"cap", cfg.MaxEventsPerPoll)
				m.EventsSkipped.Add(1)
				continue
			}

			cache.MarkSignal(signal, now, dedupeTTL)
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

			start := time.Now()
			d, err := ollamaClient.Decide(pollCtx, prompt)
			duration := time.Since(start)

			if err != nil {
				m.DecisionErrors.Add(1)
				slog.Error("ollama decision failed",
					"ns", e.Namespace, "event", e.Name, "error", err)
				continue
			}

			m.RecordOllamaLatency(duration)
			m.RecordDecision(string(d.Action))

			if d.Namespace == "" {
				d.Namespace = e.Namespace
			}
			if d.ResourceKind == "" {
				d.ResourceKind = e.InvolvedObject.Kind
			}
			if d.ResourceName == "" {
				d.ResourceName = e.InvolvedObject.Name
			}
			if d.Parameters == nil {
				d.Parameters = map[string]string{}
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
				recorder.Record(e.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Reason, d, "skipped", "below minSeverity")
				continue
			}

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
	m := metrics.New()

	// Wire the Ollama client's rate-limit and error counters into the metrics
	// recorder so remediator_ollama_rate_limited_total and
	// remediator_ollama_errors_total reflect real traffic instead of staying 0.
	ollamaClient.SetMetricsHooks(
		func() { m.OllamaRateLimited.Add(1) },
		func() { m.OllamaErrors.Add(1) },
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

	// Start metrics server in background
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	go func() {
		slog.Info("metrics server starting", "addr", cfg.MetricsAddr)
		if err := http.ListenAndServe(cfg.MetricsAddr, mux); err != nil {
			slog.Error("metrics server failed", "error", err)
		}
	}()

	// Graceful shutdown: cancel context on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Ring buffer of recent decisions, surfaced by the GUI dashboard.
	// Allocated unconditionally so runLoop can call Record() without nil
	// checks; size is small (20 entries ~few KB).
	decisionRecorder := webui.NewRecentDecisionRecorder(20)

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

	run := func(ctx context.Context) {
		runLoop(ctx, cs, ollamaClient, cfg, m, notifier, decisionRecorder)
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
