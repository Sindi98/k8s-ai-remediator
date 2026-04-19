package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
		return err
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

func runLoop(ctx context.Context, cs kubernetes.Interface, ollamaClient *ollama.Client, cfg config.AgentConfig, m *metrics.Recorder, notifier notify.Notifier) {
	seen := map[string]bool{}

	// Signal dedup: suppresses identical (ns|kind|name|reason) within a TTL.
	// Protects Ollama from bursts (e.g. flaky probes emitting many Unhealthy events).
	signalSeen := map[string]time.Time{}
	dedupeTTL := time.Duration(cfg.DedupeTTLSec) * time.Second

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
		for sig, ts := range signalSeen {
			if now.Sub(ts) >= dedupeTTL {
				delete(signalSeen, sig)
			}
		}

		processed := 0
		for _, e := range list.Items {
			key := e.Namespace + "/" + e.Name + "/" + e.ResourceVersion
			if seen[key] {
				m.EventsSkipped.Add(1)
				continue
			}
			seen[key] = true

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

			signal := e.Namespace + "|" + dedupKind + "|" + dedupName + "|" + e.Reason
			if ts, ok := signalSeen[signal]; ok && now.Sub(ts) < dedupeTTL {
				m.EventsSkipped.Add(1)
				continue
			}

			if cfg.MaxEventsPerPoll > 0 && processed >= cfg.MaxEventsPerPoll {
				slog.Info("max events per poll reached; remaining events deferred",
					"cap", cfg.MaxEventsPerPoll)
				m.EventsSkipped.Add(1)
				continue
			}

			signalSeen[signal] = now
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
