package kube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	dep.Spec.Replicas = &replicas

	_, err = cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{})
	return err
}

// SetDeploymentImage updates a container's image in the deployment spec.
func SetDeploymentImage(ctx context.Context, cs kubernetes.Interface, ns, name, image, container string, dryRun bool) error {
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

// DeploymentToText produces a human-readable summary of a deployment.
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

	return fmt.Sprintf(
		"Deployment snapshot: name=%s replicas=%d containers=%s",
		dep.Name,
		replicas,
		strings.Join(containers, ";"),
	)
}

// DeploymentSnapshot fetches and formats a deployment summary.
func DeploymentSnapshot(ctx context.Context, cs kubernetes.Interface, ns, depName string) string {
	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, depName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return DeploymentToText(dep)
}
