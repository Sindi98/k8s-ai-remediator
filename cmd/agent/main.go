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
	"github.com/tuo-user/k8s-ai-remediator/internal/ollama"
	"github.com/tuo-user/k8s-ai-remediator/internal/policy"
)

func executeDecision(
	ctx context.Context,
	cs kubernetes.Interface,
	d model.Decision,
	cfg config.AgentConfig,
) error {
	if err := policy.MaybeBlockUnsafeImageUpdate(d, cfg.AllowImageUpdates, cfg.ImageUpdateThreshold); err != nil {
		return err
	}

	switch d.Action {
	case model.ActionNoop, model.ActionAskHuman, model.ActionMarkForManualFix:
		return nil

	case model.ActionRestartDeployment:
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
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
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
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
		depName, err := kube.ResolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
		if err != nil {
			return err
		}
		return kube.SetDeploymentImage(ctx, cs, d.Namespace, depName, d.Parameters["image"], d.Parameters["container"], cfg.DryRun)

	default:
		return fmt.Errorf("unsupported action")
	}
}

// runLoop executes the main event polling loop. It returns when the parent
// context is cancelled (e.g. on SIGTERM), allowing for graceful shutdown.
func runLoop(ctx context.Context, cs kubernetes.Interface, ollamaClient *ollama.Client, cfg config.AgentConfig, m *metrics.Recorder) {
	seen := map[string]bool{}

	slog.Info("agent started",
		"model", cfg.Model,
		"baseURL", cfg.BaseURL,
		"dryRun", cfg.DryRun,
		"allowImageUpdates", cfg.AllowImageUpdates,
		"imageUpdateThreshold", cfg.ImageUpdateThreshold,
		"podLogTailLines", cfg.PodLogTailLines,
		"ollamaRPS", cfg.OllamaRPS,
		"metricsAddr", cfg.MetricsAddr,
	)

	ticker := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
	defer ticker.Stop()

	poll := func() {
		pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		list, err := cs.CoreV1().Events("").List(pollCtx, metav1.ListOptions{})
		if err != nil {
			slog.Error("list events failed", "error", err)
			return
		}

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

			m.EventsProcessed.Add(1)

			extra := ""
			if strings.EqualFold(e.InvolvedObject.Kind, "Deployment") {
				extra = kube.DeploymentSnapshot(pollCtx, cs, e.Namespace, e.InvolvedObject.Name)
			}
			if strings.EqualFold(e.InvolvedObject.Kind, "Pod") {
				if depName, err := kube.ResolveDeploymentFromPod(pollCtx, cs, e.Namespace, e.InvolvedObject.Name); err == nil {
					extra = "Resolved owner deployment: " + depName + "\n" + kube.DeploymentSnapshot(pollCtx, cs, e.Namespace, depName)
				}
			}

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

			slog.Info("decision",
				"summary", d.Summary,
				"action", d.Action,
				"ns", d.Namespace,
				"kind", d.ResourceKind,
				"name", d.ResourceName,
				"confidence", d.Confidence,
				"probable_cause", d.ProbableCause,
				"reason", d.Reason,
				"params", d.Parameters,
			)

			if err := executeDecision(pollCtx, cs, d, cfg); err != nil {
				m.ExecutionErrors.Add(1)
				slog.Error("execute decision failed", "action", d.Action, "error", err)
			}
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

	ollamaClient := ollama.NewClient(cfg.BaseURL, cfg.Model, cfg.OllamaRPS, cfg.OllamaMaxRetries, cfg.OllamaTLSSkipVerify)
	m := metrics.New()

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
		runLoop(ctx, cs, ollamaClient, cfg, m)
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
