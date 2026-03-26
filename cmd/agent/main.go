package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Action string

const (
	ActionNoop               Action = "noop"
	ActionRestartDeployment  Action = "restart_deployment"
	ActionDeleteFailedPod    Action = "delete_failed_pod"
	ActionDeleteAndRecreate  Action = "delete_and_recreate_pod"
	ActionScaleDeployment    Action = "scale_deployment"
	ActionInspectPodLogs     Action = "inspect_pod_logs"
	ActionSetDeploymentImage Action = "set_deployment_image"
	ActionMarkForManualFix   Action = "mark_for_manual_fix"
	ActionAskHuman           Action = "ask_human"
)

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

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Format   map[string]any `json:"format"`
	Options  map[string]any `json:"options"`
}

type ChatResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

func getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func getbool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

func getint(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getfloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func allowedAction(a Action) bool {
	switch a {
	case ActionNoop,
		ActionRestartDeployment,
		ActionDeleteFailedPod,
		ActionDeleteAndRecreate,
		ActionScaleDeployment,
		ActionInspectPodLogs,
		ActionSetDeploymentImage,
		ActionMarkForManualFix,
		ActionAskHuman:
		return true
	default:
		return false
	}
}

func ollamaDecision(ctx context.Context, baseURL, model, prompt string) (Decision, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary":        map[string]any{"type": "string"},
			"severity":       map[string]any{"type": "string"},
			"probable_cause": map[string]any{"type": "string"},
			"confidence":     map[string]any{"type": "number"},
			"action": map[string]any{
				"type": "string",
				"enum": []string{
					string(ActionNoop),
					string(ActionRestartDeployment),
					string(ActionDeleteFailedPod),
					string(ActionDeleteAndRecreate),
					string(ActionScaleDeployment),
					string(ActionInspectPodLogs),
					string(ActionSetDeploymentImage),
					string(ActionMarkForManualFix),
					string(ActionAskHuman),
				},
			},
			"namespace":     map[string]any{"type": "string"},
			"resource_kind": map[string]any{"type": "string"},
			"resource_name": map[string]any{"type": "string"},
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"reason": map[string]any{"type": "string"},
		},
		"required": []string{
			"summary", "severity", "probable_cause", "confidence",
			"action", "namespace", "resource_kind", "resource_name",
			"parameters", "reason",
		},
	}

	reqBody := ChatRequest{
		Model: model,
		Messages: []Message{
			{
				Role: "system",
				Content: "Return only valid JSON matching the schema. " +
					"Allowed actions: noop,restart_deployment,delete_failed_pod,delete_and_recreate_pod,scale_deployment,inspect_pod_logs,set_deployment_image,mark_for_manual_fix,ask_human. " +
					"Never suggest shell commands. " +
					"If the issue contains CrashLoopBackOff, you may use inspect_pod_logs first, but if the pod is managed by a Deployment and the issue looks recoverable, prefer delete_and_recreate_pod or restart_deployment. " +
					"If the issue contains ImagePullBackOff or ErrImagePull, prefer mark_for_manual_fix unless a concrete safe replacement image is explicit. " +
					"Use set_deployment_image only when parameters.image contains a concrete replacement image string.",
			},
			{
				Role:    "user",
				Content: prompt,
			},
		},
		Stream: false,
		Format: schema,
		Options: map[string]any{
			"temperature": 0,
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return Decision{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat", bytes.NewReader(b))
	if err != nil {
		return Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Decision{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return Decision{}, fmt.Errorf("ollama http %d: %s", resp.StatusCode, string(body))
	}

	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Decision{}, err
	}
	if out.Message.Content == "" {
		return Decision{}, fmt.Errorf("empty ollama response")
	}

	var d Decision
	if err := json.Unmarshal([]byte(out.Message.Content), &d); err != nil {
		return Decision{}, err
	}
	if !allowedAction(d.Action) {
		return Decision{}, fmt.Errorf("action not allowed: %s", d.Action)
	}
	if d.Parameters == nil {
		d.Parameters = map[string]string{}
	}

	return d, nil
}

func resolveDeploymentFromPod(ctx context.Context, cs kubernetes.Interface, ns, podName string) (string, error) {
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			rs, err := cs.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			for _, rsOwner := range rs.OwnerReferences {
				if rsOwner.Kind == "Deployment" {
					return rsOwner.Name, nil
				}
			}
		}
	}

	return "", fmt.Errorf("deployment owner not found")
}

func firstPodForDeployment(ctx context.Context, cs kubernetes.Interface, ns, deploymentName string) (string, error) {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		return "", fmt.Errorf("deployment selector not usable")
	}

	parts := make([]string, 0, len(dep.Spec.Selector.MatchLabels))
	for k, v := range dep.Spec.Selector.MatchLabels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	selector := strings.Join(parts, ",")

	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for deployment")
	}

	return pods.Items[0].Name, nil
}

func restartDeployment(ctx context.Context, cs kubernetes.Interface, ns, name string, dryRun bool) error {
	if dryRun {
		return nil
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations["ai-remediator/restarted-at"] = time.Now().UTC().Format(time.RFC3339)

	_, err = cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
	return err
}

func deletePod(ctx context.Context, cs kubernetes.Interface, ns, name string, dryRun bool) error {
	if dryRun {
		return nil
	}
	return cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

func scaleDeployment(ctx context.Context, cs kubernetes.Interface, ns, name string, replicas int32, minScale, maxScale int32, dryRun bool) error {
	if replicas < minScale || replicas > maxScale {
		return fmt.Errorf("replicas outside policy")
	}
	if dryRun {
		return nil
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	dep.Spec.Replicas = &replicas

	_, err = cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
	return err
}

func setDeploymentImage(ctx context.Context, cs kubernetes.Interface, ns, name, image, container string, dryRun bool) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image parameter is required")
	}
	if dryRun {
		return nil
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	idx := -1
	if container != "" {
		for i, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == container {
				idx = i
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("container %s not found", container)
		}
	} else {
		if len(dep.Spec.Template.Spec.Containers) == 0 {
			return fmt.Errorf("deployment has no containers")
		}
		idx = 0
	}

	dep.Spec.Template.Spec.Containers[idx].Image = image
	_, err = cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
	return err
}

func readPodLogs(ctx context.Context, cs kubernetes.Interface, ns, podName, container string, previous bool, tailLines int64) (string, error) {
	req := cs.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	b, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func chooseContainerForLogs(pod *corev1.Pod, preferred string) string {
	if pod == nil {
		return preferred
	}

	if preferred != "" {
		for _, c := range pod.Spec.Containers {
			if c.Name == preferred {
				return preferred
			}
		}
	}

	var maxRestarts int32 = -1
	selected := ""

	for _, st := range pod.Status.ContainerStatuses {
		if st.RestartCount > maxRestarts {
			maxRestarts = st.RestartCount
			selected = st.Name
		}
	}
	if selected != "" {
		return selected
	}

	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}

	return preferred
}

func inspectPodLogs(ctx context.Context, cs kubernetes.Interface, ns, kind, name string, params map[string]string, tailLines int64) error {
	podName := name

	if strings.EqualFold(kind, "Deployment") {
		var err error
		podName, err = firstPodForDeployment(ctx, cs, ns, name)
		if err != nil {
			return err
		}
	}

	if !strings.EqualFold(kind, "Pod") && !strings.EqualFold(kind, "Deployment") {
		return fmt.Errorf("inspect_pod_logs requires resource_kind=Pod or Deployment")
	}

	pod, err := cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Printf("inspect_pod_logs skipped: pod %s/%s not found anymore", ns, podName)
			return nil
		}
		return err
	}

	container := chooseContainerForLogs(pod, params["container"])
	if container == "" {
		return fmt.Errorf("no container available for pod %s", podName)
	}

	current, curErr := readPodLogs(ctx, cs, ns, podName, container, false, tailLines)
	previous, prevErr := readPodLogs(ctx, cs, ns, podName, container, true, tailLines)

	if curErr != nil && prevErr != nil {
		if apierrors.IsNotFound(curErr) || apierrors.IsNotFound(prevErr) {
			log.Printf("inspect_pod_logs skipped: pod %s/%s disappeared during log read", ns, podName)
			return nil
		}
		return fmt.Errorf("cannot read current or previous logs for container %s: current=%v previous=%v", container, curErr, prevErr)
	}

	log.Printf("inspect_pod_logs target ns=%s pod=%s container=%s", ns, podName, container)

	if current != "" {
		log.Printf("pod logs current ns=%s pod=%s container=%s\n%s", ns, podName, container, current)
	}
	if previous != "" {
		log.Printf("pod logs previous ns=%s pod=%s container=%s\n%s", ns, podName, container, previous)
	}

	return nil
}

func resolveDeploymentTarget(ctx context.Context, cs kubernetes.Interface, ns, kind, name string) (string, error) {
	if strings.EqualFold(kind, "Deployment") {
		return name, nil
	}
	if strings.EqualFold(kind, "Pod") {
		return resolveDeploymentFromPod(ctx, cs, ns, name)
	}
	return "", fmt.Errorf("cannot resolve deployment from kind=%s", kind)
}

func maybeBlockUnsafeImageUpdate(d Decision, allowImageUpdates bool, threshold float64) error {
	if d.Action != ActionSetDeploymentImage {
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

func executeDecision(
	ctx context.Context,
	cs kubernetes.Interface,
	d Decision,
	dryRun bool,
	minScale, maxScale int32,
	allowImageUpdates bool,
	imageUpdateThreshold float64,
	podLogTailLines int64,
) error {
	if err := maybeBlockUnsafeImageUpdate(d, allowImageUpdates, imageUpdateThreshold); err != nil {
		return err
	}

	switch d.Action {
	case ActionNoop, ActionAskHuman, ActionMarkForManualFix:
		return nil

	case ActionRestartDeployment:
		depName, err := resolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
		if err != nil {
			return err
		}
		return restartDeployment(ctx, cs, d.Namespace, depName, dryRun)

	case ActionDeleteFailedPod, ActionDeleteAndRecreate:
		if strings.EqualFold(d.ResourceKind, "Pod") {
			return deletePod(ctx, cs, d.Namespace, d.ResourceName, dryRun)
		}
		return fmt.Errorf("%s requires resource_kind=Pod", d.Action)

	case ActionScaleDeployment:
		depName, err := resolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
		if err != nil {
			return err
		}
		r, err := strconv.Atoi(d.Parameters["replicas"])
		if err != nil {
			return fmt.Errorf("invalid replicas parameter")
		}
		return scaleDeployment(ctx, cs, d.Namespace, depName, int32(r), minScale, maxScale, dryRun)

	case ActionInspectPodLogs:
		return inspectPodLogs(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName, d.Parameters, podLogTailLines)

	case ActionSetDeploymentImage:
		depName, err := resolveDeploymentTarget(ctx, cs, d.Namespace, d.ResourceKind, d.ResourceName)
		if err != nil {
			return err
		}
		return setDeploymentImage(
			ctx,
			cs,
			d.Namespace,
			depName,
			d.Parameters["image"],
			d.Parameters["container"],
			dryRun,
		)

	default:
		return fmt.Errorf("unsupported action")
	}
}

func deploymentToText(dep *appsv1.Deployment) string {
	if dep == nil {
		return ""
	}

	containers := make([]string, 0, len(dep.Spec.Template.Spec.Containers))
	for _, c := range dep.Spec.Template.Spec.Containers {
		containers = append(containers, fmt.Sprintf("%s=%s", c.Name, c.Image))
	}

	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}

	return fmt.Sprintf(
		"Deployment snapshot: name=%s replicas=%d containers=%s",
		dep.Name,
		replicas,
		strings.Join(containers, ";"),
	)
}

func deploymentSnapshot(ctx context.Context, cs kubernetes.Interface, ns, depName string) string {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return deploymentToText(dep)
}

// sanitizeForPrompt removes characters and patterns that could be used for
// prompt injection attacks in LLM inputs sourced from Kubernetes events.
// It strips control characters, trims excessive length, and removes sequences
// that attempt to override system instructions.
func sanitizeForPrompt(s string, maxLen int) string {
	// Remove control characters except newline and tab
	re := regexp.MustCompile(`[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]`)
	s = re.ReplaceAllString(s, "")

	// Strip common prompt injection patterns (case-insensitive)
	injectionPatterns := regexp.MustCompile(`(?i)(ignore previous instructions|ignore all instructions|disregard above|system:\s|you are now|forget everything|new instructions:)`)
	s = injectionPatterns.ReplaceAllString(s, "[REDACTED]")

	// Truncate to prevent excessively long inputs
	if maxLen > 0 && len(s) > maxLen {
		s = s[:maxLen] + "...[truncated]"
	}

	return strings.TrimSpace(s)
}

func buildPrompt(ns, kind, name, etype, reason, message, extra string) string {
	// Sanitize all user-controllable fields to prevent prompt injection
	const fieldMaxLen = 2000
	reason = sanitizeForPrompt(reason, 500)
	message = sanitizeForPrompt(message, fieldMaxLen)
	extra = sanitizeForPrompt(extra, fieldMaxLen)

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

// AgentConfig holds all configuration values for the remediation agent.
type AgentConfig struct {
	BaseURL              string
	Model                string
	DryRun               bool
	MinScale             int32
	MaxScale             int32
	PollSec              int
	AllowImageUpdates    bool
	ImageUpdateThreshold float64
	PodLogTailLines      int64
}

// LoadConfigFromEnv reads agent configuration from environment variables.
func LoadConfigFromEnv() AgentConfig {
	return AgentConfig{
		BaseURL:              getenv("OLLAMA_BASE_URL", "http://ollama.ollama.svc.cluster.local:11434/api"),
		Model:                getenv("OLLAMA_MODEL", "gemma3"),
		DryRun:               getbool("DRY_RUN", false),
		MinScale:             int32(getint("SCALE_MIN", 1)),
		MaxScale:             int32(getint("SCALE_MAX", 5)),
		PollSec:              getint("POLL_INTERVAL_SECONDS", 30),
		AllowImageUpdates:    getbool("ALLOW_IMAGE_UPDATES", false),
		ImageUpdateThreshold: getfloat("IMAGE_UPDATE_CONFIDENCE_THRESHOLD", 0.92),
		PodLogTailLines:      int64(getint("POD_LOG_TAIL_LINES", 200)),
	}
}

// runLoop executes the main event polling loop. It returns when the parent
// context is cancelled (e.g. on SIGTERM), allowing for graceful shutdown.
func runLoop(ctx context.Context, cs kubernetes.Interface, cfg AgentConfig) {
	seen := map[string]bool{}
	log.Printf(
		"agent started model=%s baseURL=%s dryRun=%v allowImageUpdates=%v imageUpdateThreshold=%.2f podLogTailLines=%d",
		cfg.Model, cfg.BaseURL, cfg.DryRun, cfg.AllowImageUpdates, cfg.ImageUpdateThreshold, cfg.PodLogTailLines,
	)

	ticker := time.NewTicker(time.Duration(cfg.PollSec) * time.Second)
	defer ticker.Stop()

	// Run immediately on start, then on ticker
	poll := func() {
		pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		list, err := cs.CoreV1().Events("").List(pollCtx, metav1.ListOptions{})
		if err != nil {
			log.Printf("list events error: %v", err)
			return
		}

		for _, e := range list.Items {
			key := e.Namespace + "/" + e.Name + "/" + e.ResourceVersion
			if seen[key] {
				continue
			}
			seen[key] = true

			if !strings.EqualFold(e.Type, "Warning") {
				continue
			}

			extra := ""
			if strings.EqualFold(e.InvolvedObject.Kind, "Deployment") {
				extra = deploymentSnapshot(pollCtx, cs, e.Namespace, e.InvolvedObject.Name)
			}
			if strings.EqualFold(e.InvolvedObject.Kind, "Pod") {
				if depName, err := resolveDeploymentFromPod(pollCtx, cs, e.Namespace, e.InvolvedObject.Name); err == nil {
					extra = "Resolved owner deployment: " + depName + "\n" + deploymentSnapshot(pollCtx, cs, e.Namespace, depName)
				}
			}

			prompt := buildPrompt(
				e.Namespace,
				e.InvolvedObject.Kind,
				e.InvolvedObject.Name,
				e.Type,
				e.Reason,
				e.Message,
				extra,
			)

			d, err := ollamaDecision(pollCtx, cfg.BaseURL, cfg.Model, prompt)
			if err != nil {
				log.Printf("ollama decision error for %s/%s: %v", e.Namespace, e.Name, err)
				continue
			}

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

			log.Printf(
				"decision summary=%s action=%s ns=%s kind=%s name=%s confidence=%.2f probable_cause=%s reason=%s params=%v",
				d.Summary, d.Action, d.Namespace, d.ResourceKind, d.ResourceName, d.Confidence, d.ProbableCause, d.Reason, d.Parameters,
			)

			if err := executeDecision(
				pollCtx,
				cs,
				d,
				cfg.DryRun,
				cfg.MinScale,
				cfg.MaxScale,
				cfg.AllowImageUpdates,
				cfg.ImageUpdateThreshold,
				cfg.PodLogTailLines,
			); err != nil {
				log.Printf("execute decision error: %v", err)
			}
		}
	}

	// First poll immediately
	poll()

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down gracefully: %v", ctx.Err())
			return
		case <-ticker.C:
			poll()
		}
	}
}

func main() {
	cfg := LoadConfigFromEnv()

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatal(err)
	}

	// Graceful shutdown: cancel context on SIGTERM/SIGINT
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	runLoop(ctx, cs, cfg)
	log.Println("agent stopped")
}
