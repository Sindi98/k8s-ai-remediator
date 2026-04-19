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
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/tuo-user/k8s-ai-remediator/internal/config"
	"github.com/tuo-user/k8s-ai-remediator/internal/kube"
	"github.com/tuo-user/k8s-ai-remediator/internal/metrics"
	"github.com/tuo-user/k8s-ai-remediator/internal/model"
	"github.com/tuo-user/k8s-ai-remediator/internal/notify"
	"github.com/tuo-user/k8s-ai-remediator/internal/ollama"
	"github.com/tuo-user/k8s-ai-remediator/internal/policy"
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
		return err
	}
	if err := policy.MaybeBlockRestartOnOOMKilled(d, extra); err != nil {
		// Auto-escalation: if the Deployment opts in to resources patching,
		// silently transform the rejected restart into a patch_resources
		// with a safe 2x bump (floor 256Mi). This covers the common case
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
// annotation and the global flag is on. Uses a conservative 2x bump of
// the current memory_limit (minimum 256Mi) so the retry has a realistic
// chance of succeeding. Returns (transformed, true) on success,
// (untouched, false) otherwise.
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
	if transformed.Parameters == nil {
		transformed.Parameters = map[string]string{}
	}
	transformed.Parameters["deployment_name"] = depName
	transformed.Parameters["container"] = c.Name
	transformed.Parameters["memory_limit"] = newLimit
	return transformed, true
}

// computeBumpedMemoryLimit returns a safe "next step" memory limit: 2x the
// current value (or 256Mi when the current is unset or smaller), capped at
// the package-wide MaxMemoryQuantity so the subsequent validator does not
// reject it.
func computeBumpedMemoryLimit(current resource.Quantity) string {
	floor := resource.MustParse("256Mi")
	var target resource.Quantity
	if current.IsZero() {
		target = floor
	} else {
		doubled := current.DeepCopy()
		doubled.Set(current.Value() * 2)
		if doubled.Cmp(floor) < 0 {
			target = floor
		} else {
			target = doubled
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

// dedupCache bundles the two dedup maps with a mutex. Today poll() runs
// single-threaded on the leader, but the mutex documents the invariant and
// keeps the cache safe if a future change (extra goroutine, informer) makes
// access concurrent. Both maps are TTL-bounded to prevent unbounded growth
// on long-running agents.
type dedupCache struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	signalSeen map[string]time.Time
}

func (d *dedupCache) markSeen(key string, now time.Time) (fresh bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[key]; ok {
		return false
	}
	d.seen[key] = now
	return true
}

func (d *dedupCache) isSignalFresh(signal string, now time.Time, ttl time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ts, ok := d.signalSeen[signal]
	return ok && now.Sub(ts) < ttl
}

func (d *dedupCache) markSignal(signal string, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.signalSeen[signal] = now
}

func (d *dedupCache) evict(now time.Time, signalTTL, seenTTL time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sig, ts := range d.signalSeen {
		if now.Sub(ts) >= signalTTL {
			delete(d.signalSeen, sig)
		}
	}
	for key, ts := range d.seen {
		if now.Sub(ts) >= seenTTL {
			delete(d.seen, key)
		}
	}
}

func runLoop(ctx context.Context, cs kubernetes.Interface, ollamaClient *ollama.Client, cfg config.AgentConfig, m *metrics.Recorder, notifier notify.Notifier) {
	cache := &dedupCache{
		seen:       map[string]time.Time{},
		signalSeen: map[string]time.Time{},
	}

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
		"buildFeatures", "dedup,infer-dep-from-podname,block-restart-on-unhealthy,patch_probe,patch_resources,patch_registry",
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
		cache.evict(now, dedupeTTL, seenTTL)

		processed := 0
		for _, e := range list.Items {
			key := e.Namespace + "/" + e.Name + "/" + e.ResourceVersion
			if !cache.markSeen(key, now) {
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
			if cache.isSignalFresh(signal, now, dedupeTTL) {
				m.EventsSkipped.Add(1)
				continue
			}

			if cfg.MaxEventsPerPoll > 0 && processed >= cfg.MaxEventsPerPoll {
				slog.Info("max events per poll reached; remaining events deferred",
					"cap", cfg.MaxEventsPerPoll)
				m.EventsSkipped.Add(1)
				continue
			}

			cache.markSignal(signal, now)
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

			if !severity.MeetsMinimum(minSev) {
				slog.Info("skipping decision below minimum severity",
					"severity", string(severity),
					"minSeverity", string(minSev),
					"action", d.Action,
				)
				m.EventsSkipped.Add(1)
				continue
			}

			execErr := executeDecision(pollCtx, cs, d, cfg, e.Reason, extra)
			if execErr != nil {
				m.ExecutionErrors.Add(1)
				slog.Error("execute decision failed", "action", d.Action, "error", execErr)
			}

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
		w.Write([]byte("ok"))
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

	run := func(ctx context.Context) {
		runLoop(ctx, cs, ollamaClient, cfg, m, notifier)
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
