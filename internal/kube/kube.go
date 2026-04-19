package kube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// mutateDeployment reads a Deployment, applies mutate, and writes it back,
// automatically re-reading on resourceVersion conflicts so concurrent
// controllers (HPA, another agent, kubectl apply) cannot silently clobber
// each other. Returns a terminal error when mutate fails deterministically
// (e.g. validation error) without triggering a retry.
func mutateDeployment(
	ctx context.Context,
	cs kubernetes.Interface,
	ns, name string,
	mutate func(*appsv1.Deployment) error,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := mutate(dep); err != nil {
			return err
		}
		_, err = cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
		return err
	})
}

// AllowPatchAnnotation is the Deployment annotation that opts the workload
// in to the patch_* actions. Its value is a comma-separated list of the
// allowed scopes: "probe", "resources", "registry", or "*" for all.
const AllowPatchAnnotation = "ai-remediator/allow-patch"

// DeploymentAllowsPatch returns true if the deployment carries the opt-in
// annotation and lists the requested scope (or "*").
func DeploymentAllowsPatch(dep *appsv1.Deployment, scope string) bool {
	if dep == nil || dep.Annotations == nil {
		return false
	}
	raw := strings.TrimSpace(dep.Annotations[AllowPatchAnnotation])
	if raw == "" {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(strings.ToLower(part))
		if p == "*" || p == strings.ToLower(scope) {
			return true
		}
	}
	return false
}

// InferDeploymentFromPodName derives the parent Deployment name from the
// Kubernetes naming convention `{deployment}-{replicaset-hash}-{pod-hash}`,
// used as a fallback when the pod itself has already been deleted and
// ownerReferences cannot be followed. The candidate is returned only if
// a Deployment with that name still exists in the namespace.
func InferDeploymentFromPodName(ctx context.Context, cs kubernetes.Interface, ns, podName string) (string, bool) {
	parts := strings.Split(podName, "-")
	if len(parts) < 3 {
		return "", false
	}
	candidate := strings.Join(parts[:len(parts)-2], "-")
	if _, err := cs.AppsV1().Deployments(ns).Get(ctx, candidate, metav1.GetOptions{}); err != nil {
		return "", false
	}
	return candidate, true
}

// ResolveDeploymentFromPod traverses ownerReferences to find the parent Deployment.
func ResolveDeploymentFromPod(ctx context.Context, cs kubernetes.Interface, ns, podName string) (string, error) {
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

// FirstPodForDeployment finds the first pod matching a deployment's label selector.
func FirstPodForDeployment(ctx context.Context, cs kubernetes.Interface, ns, deploymentName string) (string, error) {
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

// ResolveDeploymentTarget resolves a deployment name from either a Deployment
// or Pod resource. If params contains "deployment_name" (or "deployment") it
// takes precedence, covering the case where the pod referenced by the event
// no longer exists by the time the decision is executed.
func ResolveDeploymentTarget(ctx context.Context, cs kubernetes.Interface, ns, kind, name string, params map[string]string) (string, error) {
	if params != nil {
		for _, key := range []string{"deployment_name", "deployment"} {
			if v := strings.TrimSpace(params[key]); v != "" {
				return v, nil
			}
		}
	}
	if strings.EqualFold(kind, "Deployment") {
		return name, nil
	}
	if strings.EqualFold(kind, "Pod") {
		return ResolveDeploymentFromPod(ctx, cs, ns, name)
	}
	return "", fmt.Errorf("cannot resolve deployment from kind=%s", kind)
}

// RestartDeployment triggers a rollout by updating an annotation.
func RestartDeployment(ctx context.Context, cs kubernetes.Interface, ns, name string, dryRun bool) error {
	if dryRun {
		return nil
	}
	return mutateDeployment(ctx, cs, ns, name, func(dep *appsv1.Deployment) error {
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations["ai-remediator/restarted-at"] = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

// DeletePod removes a pod, relying on the controller to recreate it.
func DeletePod(ctx context.Context, cs kubernetes.Interface, ns, name string, dryRun bool) error {
	if dryRun {
		return nil
	}
	return cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

// ScaleDeployment adjusts replica count within the given policy bounds.
func ScaleDeployment(ctx context.Context, cs kubernetes.Interface, ns, name string, replicas, minScale, maxScale int32, dryRun bool) error {
	if replicas < minScale || replicas > maxScale {
		return fmt.Errorf("replicas outside policy")
	}
	if dryRun {
		return nil
	}
	return mutateDeployment(ctx, cs, ns, name, func(dep *appsv1.Deployment) error {
		r := replicas
		dep.Spec.Replicas = &r
		return nil
	})
}

// SetDeploymentImage updates a container's image in the deployment spec.
// Rejects no-op updates (proposed image equals current) since they cannot
// fix transient pull failures and waste a rollout: in those cases the LLM
// should pick delete_failed_pod or restart_deployment instead.
func SetDeploymentImage(ctx context.Context, cs kubernetes.Interface, ns, name, image, container string, dryRun bool) error {
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image parameter is required")
	}
	if dryRun {
		// Validate without mutating so callers get feedback on no-op updates
		// and missing containers in dry-run mode too.
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		_, err = pickContainerForImage(dep, image, container)
		return err
	}
	return mutateDeployment(ctx, cs, ns, name, func(dep *appsv1.Deployment) error {
		idx, err := pickContainerForImage(dep, image, container)
		if err != nil {
			return err
		}
		dep.Spec.Template.Spec.Containers[idx].Image = image
		return nil
	})
}

// pickContainerForImage resolves the target container index and rejects
// no-op image changes. Shared between the dry-run and write paths so both
// surface the same validation errors.
func pickContainerForImage(dep *appsv1.Deployment, image, container string) (int, error) {
	idx := -1
	if container != "" {
		for i, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == container {
				idx = i
				break
			}
		}
		if idx == -1 {
			return -1, fmt.Errorf("container %s not found", container)
		}
	} else {
		if len(dep.Spec.Template.Spec.Containers) == 0 {
			return -1, fmt.Errorf("deployment has no containers")
		}
		idx = 0
	}
	if dep.Spec.Template.Spec.Containers[idx].Image == image {
		return -1, fmt.Errorf("set_deployment_image is a no-op: container %q already runs %q; for transient pull failures use delete_failed_pod or restart_deployment", dep.Spec.Template.Spec.Containers[idx].Name, image)
	}
	return idx, nil
}

// ProbeFieldBounds caps each tunable probe field to a sane range. The LLM
// can only propose values within these bounds; anything outside is rejected.
var ProbeFieldBounds = map[string][2]int32{
	"initial_delay_seconds": {0, 600},
	"period_seconds":        {1, 300},
	"failure_threshold":     {1, 20},
	"success_threshold":     {1, 5},
	"timeout_seconds":       {1, 60},
}

func parseProbeField(key, raw string) (int32, error) {
	bounds, ok := ProbeFieldBounds[key]
	if !ok {
		return 0, fmt.Errorf("unknown probe field %q", key)
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("probe field %q not an integer: %v", key, err)
	}
	if int32(n) < bounds[0] || int32(n) > bounds[1] {
		return 0, fmt.Errorf("probe field %q=%d outside bounds [%d,%d]", key, n, bounds[0], bounds[1])
	}
	return int32(n), nil
}

func findContainerIndex(containers []corev1.Container, name string) (int, error) {
	if name == "" {
		if len(containers) == 0 {
			return -1, fmt.Errorf("deployment has no containers")
		}
		return 0, nil
	}
	for i, c := range containers {
		if c.Name == name {
			return i, nil
		}
	}
	return -1, fmt.Errorf("container %q not found", name)
}

// PatchDeploymentProbe updates the timing fields (initialDelaySeconds,
// periodSeconds, failureThreshold, successThreshold, timeoutSeconds) of
// either the readiness or liveness probe. It never rewrites the probe
// handler (exec/httpGet/tcpSocket) since those are more likely to be
// correct-but-flaky than misconfigured.
func PatchDeploymentProbe(ctx context.Context, cs kubernetes.Interface, ns, name, container, probeType string, fields map[string]string, dryRun bool) error {
	apply := func(dep *appsv1.Deployment) error {
		if !DeploymentAllowsPatch(dep, "probe") {
			return fmt.Errorf("deployment %s/%s does not opt in to patch_probe (set annotation %s)", ns, name, AllowPatchAnnotation)
		}
		idx, err := findContainerIndex(dep.Spec.Template.Spec.Containers, container)
		if err != nil {
			return err
		}
		c := &dep.Spec.Template.Spec.Containers[idx]

		var probe **corev1.Probe
		switch strings.ToLower(strings.TrimSpace(probeType)) {
		case "readiness":
			probe = &c.ReadinessProbe
		case "liveness":
			probe = &c.LivenessProbe
		default:
			return fmt.Errorf("probe must be one of: readiness, liveness")
		}
		if *probe == nil {
			return fmt.Errorf("container %q has no %s probe to patch", c.Name, probeType)
		}

		changed := false
		for key, raw := range fields {
			v, err := parseProbeField(key, raw)
			if err != nil {
				return err
			}
			switch key {
			case "initial_delay_seconds":
				(*probe).InitialDelaySeconds = v
			case "period_seconds":
				(*probe).PeriodSeconds = v
			case "failure_threshold":
				(*probe).FailureThreshold = v
			case "success_threshold":
				(*probe).SuccessThreshold = v
			case "timeout_seconds":
				(*probe).TimeoutSeconds = v
			}
			changed = true
		}
		if !changed {
			return fmt.Errorf("patch_probe requires at least one probe field in parameters")
		}
		return nil
	}

	if dryRun {
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return apply(dep)
	}
	return mutateDeployment(ctx, cs, ns, name, apply)
}

// ResourceBounds caps requests/limits to avoid runaway allocations driven
// by a hallucinated LLM value.
var (
	MaxCPUQuantity    = resource.MustParse("8")
	MaxMemoryQuantity = resource.MustParse("16Gi")
	MinCPUQuantity    = resource.MustParse("10m")
	MinMemoryQuantity = resource.MustParse("16Mi")
)

func parseQuantityInRange(raw string, min, max resource.Quantity) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(strings.TrimSpace(raw))
	if err != nil {
		return q, fmt.Errorf("invalid quantity %q: %v", raw, err)
	}
	if q.Cmp(min) < 0 || q.Cmp(max) > 0 {
		return q, fmt.Errorf("quantity %s outside bounds [%s,%s]", q.String(), min.String(), max.String())
	}
	return q, nil
}

// PatchDeploymentResources rewrites requests/limits on a single container.
// Parameters accepted: cpu_request, memory_request, cpu_limit, memory_limit.
// Any subset can be provided; the rest are left untouched. Values are bounded
// by Min/Max*Quantity and requests must not exceed limits.
func PatchDeploymentResources(ctx context.Context, cs kubernetes.Interface, ns, name, container string, params map[string]string, dryRun bool) error {
	apply := func(dep *appsv1.Deployment) error {
		if !DeploymentAllowsPatch(dep, "resources") {
			return fmt.Errorf("deployment %s/%s does not opt in to patch_resources (set annotation %s)", ns, name, AllowPatchAnnotation)
		}
		idx, err := findContainerIndex(dep.Spec.Template.Spec.Containers, container)
		if err != nil {
			return err
		}
		c := &dep.Spec.Template.Spec.Containers[idx]

		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}

		changed := false
		if raw := strings.TrimSpace(params["cpu_request"]); raw != "" {
			q, err := parseQuantityInRange(raw, MinCPUQuantity, MaxCPUQuantity)
			if err != nil {
				return fmt.Errorf("cpu_request: %w", err)
			}
			c.Resources.Requests[corev1.ResourceCPU] = q
			changed = true
		}
		if raw := strings.TrimSpace(params["memory_request"]); raw != "" {
			q, err := parseQuantityInRange(raw, MinMemoryQuantity, MaxMemoryQuantity)
			if err != nil {
				return fmt.Errorf("memory_request: %w", err)
			}
			c.Resources.Requests[corev1.ResourceMemory] = q
			changed = true
		}
		if raw := strings.TrimSpace(params["cpu_limit"]); raw != "" {
			q, err := parseQuantityInRange(raw, MinCPUQuantity, MaxCPUQuantity)
			if err != nil {
				return fmt.Errorf("cpu_limit: %w", err)
			}
			c.Resources.Limits[corev1.ResourceCPU] = q
			changed = true
		}
		if raw := strings.TrimSpace(params["memory_limit"]); raw != "" {
			q, err := parseQuantityInRange(raw, MinMemoryQuantity, MaxMemoryQuantity)
			if err != nil {
				return fmt.Errorf("memory_limit: %w", err)
			}
			c.Resources.Limits[corev1.ResourceMemory] = q
			changed = true
		}
		if !changed {
			return fmt.Errorf("patch_resources requires at least one of cpu_request, memory_request, cpu_limit, memory_limit")
		}

		// Enforce requests <= limits when both are set.
		for _, rt := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			req, hasReq := c.Resources.Requests[rt]
			lim, hasLim := c.Resources.Limits[rt]
			if hasReq && hasLim && req.Cmp(lim) > 0 {
				return fmt.Errorf("%s request %s exceeds limit %s", rt, req.String(), lim.String())
			}
		}
		return nil
	}

	if dryRun {
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return apply(dep)
	}
	return mutateDeployment(ctx, cs, ns, name, apply)
}

// SwapRegistry replaces the registry host prefix in an image reference,
// preserving the path and tag/digest. If the image has no explicit registry,
// the new registry is prepended.
func SwapRegistry(image, newRegistry string) (string, error) {
	image = strings.TrimSpace(image)
	newRegistry = strings.TrimSpace(strings.TrimRight(newRegistry, "/"))
	if image == "" {
		return "", fmt.Errorf("image is empty")
	}
	if newRegistry == "" {
		return "", fmt.Errorf("new_registry is empty")
	}
	// An image reference carries an explicit registry only when there is a
	// "/" AND the first segment looks like a host (contains "." or ":" or
	// equals "localhost"). Otherwise the leading segment is just a repo
	// namespace (e.g. "library/nginx") and no registry is stripped.
	first := image
	rest := ""
	if idx := strings.Index(image, "/"); idx >= 0 {
		first = image[:idx]
		rest = image[idx+1:]
	}
	hasRegistry := rest != "" && (strings.ContainsAny(first, ".:") || first == "localhost")
	if hasRegistry {
		return newRegistry + "/" + rest, nil
	}
	return newRegistry + "/" + image, nil
}

// PatchDeploymentRegistry rewrites the registry prefix of a container image
// while preserving the path and tag. Intended for fixing typos or moving
// between local registries without allowing arbitrary image swaps.
func PatchDeploymentRegistry(ctx context.Context, cs kubernetes.Interface, ns, name, container, newRegistry string, dryRun bool) error {
	apply := func(dep *appsv1.Deployment) error {
		if !DeploymentAllowsPatch(dep, "registry") {
			return fmt.Errorf("deployment %s/%s does not opt in to patch_registry (set annotation %s)", ns, name, AllowPatchAnnotation)
		}
		idx, err := findContainerIndex(dep.Spec.Template.Spec.Containers, container)
		if err != nil {
			return err
		}
		c := &dep.Spec.Template.Spec.Containers[idx]

		newImage, err := SwapRegistry(c.Image, newRegistry)
		if err != nil {
			return err
		}
		if newImage == c.Image {
			return fmt.Errorf("new registry produces the same image %q; no change", newImage)
		}
		dep.Spec.Template.Spec.Containers[idx].Image = newImage
		return nil
	}

	if dryRun {
		dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return apply(dep)
	}
	return mutateDeployment(ctx, cs, ns, name, apply)
}

// ReadPodLogs reads the logs of a specific container.
func ReadPodLogs(ctx context.Context, cs kubernetes.Interface, ns, podName, container string, previous bool, tailLines int64) (string, error) {
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

// ChooseContainerForLogs picks the best container to read logs from.
func ChooseContainerForLogs(pod *corev1.Pod, preferred string) string {
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

// InspectPodLogs fetches current and previous logs for a pod or deployment.
func InspectPodLogs(ctx context.Context, cs kubernetes.Interface, ns, kind, name string, params map[string]string, tailLines int64) error {
	podName := name

	if strings.EqualFold(kind, "Deployment") {
		var err error
		podName, err = FirstPodForDeployment(ctx, cs, ns, name)
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
			slog.Warn("inspect_pod_logs skipped: pod not found", "ns", ns, "pod", podName)
			return nil
		}
		return err
	}

	container := ChooseContainerForLogs(pod, params["container"])
	if container == "" {
		return fmt.Errorf("no container available for pod %s", podName)
	}

	current, curErr := ReadPodLogs(ctx, cs, ns, podName, container, false, tailLines)
	previous, prevErr := ReadPodLogs(ctx, cs, ns, podName, container, true, tailLines)

	if curErr != nil && prevErr != nil {
		if apierrors.IsNotFound(curErr) || apierrors.IsNotFound(prevErr) {
			slog.Warn("inspect_pod_logs skipped: pod disappeared", "ns", ns, "pod", podName)
			return nil
		}
		return fmt.Errorf("cannot read current or previous logs for container %s: current=%v previous=%v", container, curErr, prevErr)
	}

	slog.Info("inspect_pod_logs", "ns", ns, "pod", podName, "container", container)

	if current != "" {
		slog.Info("pod logs (current)", "ns", ns, "pod", podName, "container", container, "logs", current)
	}
	if previous != "" {
		slog.Info("pod logs (previous)", "ns", ns, "pod", podName, "container", container, "logs", previous)
	}

	return nil
}

// DeploymentToText produces a human-readable summary of a deployment,
// including probe timing and the patch opt-in annotation so the LLM can
// pick between inspect_pod_logs and a concrete patch_* action.
func DeploymentToText(dep *appsv1.Deployment) string {
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

	out := fmt.Sprintf(
		"Deployment snapshot: name=%s replicas=%d containers=%s",
		dep.Name,
		replicas,
		strings.Join(containers, ";"),
	)

	// Surface the patch opt-in so the LLM knows which patch_* actions are
	// unlocked on this specific Deployment.
	if v := strings.TrimSpace(dep.Annotations[AllowPatchAnnotation]); v != "" {
		out += fmt.Sprintf("\nAllow-patch scopes (opt-in via annotation): %s", v)
	} else {
		out += "\nAllow-patch scopes (opt-in via annotation): none"
	}

	// Emit current probe timings so the LLM can propose incremental changes
	// (e.g. double failureThreshold) without guessing blindly.
	for _, c := range dep.Spec.Template.Spec.Containers {
		for probeName, p := range map[string]*corev1.Probe{
			"readinessProbe": c.ReadinessProbe,
			"livenessProbe":  c.LivenessProbe,
		} {
			if p == nil {
				continue
			}
			out += fmt.Sprintf(
				"\nContainer %s %s: initialDelay=%d period=%d failureThreshold=%d successThreshold=%d timeout=%d",
				c.Name, probeName,
				p.InitialDelaySeconds, p.PeriodSeconds,
				p.FailureThreshold, p.SuccessThreshold, p.TimeoutSeconds,
			)
		}
	}

	return out
}

// DeploymentSnapshot fetches and formats a deployment summary.
func DeploymentSnapshot(ctx context.Context, cs kubernetes.Interface, ns, depName string) string {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return DeploymentToText(dep)
}

// PodStatusSummary returns a short textual description of the pod's current
// container states and last termination reasons. It surfaces OOMKilled and
// exit codes so the LLM can distinguish between probe timing issues,
// resource pressure and genuine application crashes. Empty string if the
// pod cannot be read.
func PodStatusSummary(ctx context.Context, cs kubernetes.Interface, ns, podName string) string {
	pod, err := cs.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return ""
	}

	lines := []string{fmt.Sprintf("Pod %s phase=%s restartPolicy=%s", pod.Name, pod.Status.Phase, pod.Spec.RestartPolicy)}
	for _, cs := range pod.Status.ContainerStatuses {
		parts := []string{fmt.Sprintf("container=%s restarts=%d ready=%t", cs.Name, cs.RestartCount, cs.Ready)}
		if cs.State.Waiting != nil {
			parts = append(parts, fmt.Sprintf("state=Waiting reason=%s", cs.State.Waiting.Reason))
		}
		if cs.State.Terminated != nil {
			parts = append(parts, fmt.Sprintf("state=Terminated reason=%s exit=%d", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
		}
		if cs.State.Running != nil {
			parts = append(parts, "state=Running")
		}
		if cs.LastTerminationState.Terminated != nil {
			parts = append(parts, fmt.Sprintf("lastTerminated reason=%s exit=%d", cs.LastTerminationState.Terminated.Reason, cs.LastTerminationState.Terminated.ExitCode))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}
