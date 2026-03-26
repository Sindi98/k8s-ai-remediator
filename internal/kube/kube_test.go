package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func int32Ptr(i int32) *int32 { return &i }

func newFakeCluster(t *testing.T) *fake.Clientset {
	t.Helper()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "dep-uid-1"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(2),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25"}},
				},
			},
		},
	}

	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc-123", Namespace: "default",
			Labels:          map[string]string{"app": "web"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.25"},
				{Name: "sidecar", Image: "envoy:latest"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 5},
				{Name: "sidecar", RestartCount: 0},
			},
		},
	}

	return fake.NewSimpleClientset(dep, rs, pod)
}

func TestResolveDeploymentFromPod(t *testing.T) {
	cs := newFakeCluster(t)
	name, err := ResolveDeploymentFromPod(context.Background(), cs, "default", "web-abc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "web" {
		t.Errorf("expected 'web', got %q", name)
	}
}

func TestResolveDeploymentFromPod_NotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := ResolveDeploymentFromPod(context.Background(), cs, "default", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent pod")
	}
}

func TestFirstPodForDeployment(t *testing.T) {
	cs := newFakeCluster(t)
	podName, err := FirstPodForDeployment(context.Background(), cs, "default", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if podName != "web-abc-123" {
		t.Errorf("expected 'web-abc-123', got %q", podName)
	}
}

func TestResolveDeploymentTarget(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	name, err := ResolveDeploymentTarget(ctx, cs, "default", "Deployment", "web")
	if err != nil || name != "web" {
		t.Errorf("expected web, got %s, err=%v", name, err)
	}

	name, err = ResolveDeploymentTarget(ctx, cs, "default", "Pod", "web-abc-123")
	if err != nil || name != "web" {
		t.Errorf("expected web, got %s, err=%v", name, err)
	}

	_, err = ResolveDeploymentTarget(ctx, cs, "default", "Service", "svc")
	if err == nil {
		t.Error("expected error for unsupported kind")
	}
}

func TestRestartDeployment(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := RestartDeployment(ctx, cs, "default", "web", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected restart annotation")
	}
}

func TestRestartDeployment_DryRun(t *testing.T) {
	cs := newFakeCluster(t)
	if err := RestartDeployment(context.Background(), cs, "default", "web", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if dep.Spec.Template.Annotations != nil {
		if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; ok {
			t.Error("dry run should not set annotation")
		}
	}
}

func TestDeletePod(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := DeletePod(ctx, cs, "default", "web-abc-123", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err := cs.CoreV1().Pods("default").Get(ctx, "web-abc-123", metav1.GetOptions{})
	if err == nil {
		t.Error("pod should be deleted")
	}
}

func TestDeletePod_DryRun(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := DeletePod(ctx, cs, "default", "web-abc-123", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err := cs.CoreV1().Pods("default").Get(ctx, "web-abc-123", metav1.GetOptions{})
	if err != nil {
		t.Error("dry run should not delete pod")
	}
}

func TestScaleDeployment(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := ScaleDeployment(ctx, cs, "default", "web", 3, 1, 5, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *dep.Spec.Replicas)
	}
}

func TestScaleDeployment_OutsidePolicy(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := ScaleDeployment(ctx, cs, "default", "web", 10, 1, 5, false); err == nil {
		t.Error("expected error for replicas above max")
	}
	if err := ScaleDeployment(ctx, cs, "default", "web", 0, 1, 5, false); err == nil {
		t.Error("expected error for replicas below min")
	}
}

func TestSetDeploymentImage(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	if err := SetDeploymentImage(ctx, cs, "default", "web", "nginx:1.26", "app", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if dep.Spec.Template.Spec.Containers[0].Image != "nginx:1.26" {
		t.Errorf("expected nginx:1.26, got %s", dep.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestSetDeploymentImage_EmptyImage(t *testing.T) {
	cs := newFakeCluster(t)
	if err := SetDeploymentImage(context.Background(), cs, "default", "web", "", "app", false); err == nil {
		t.Error("expected error for empty image")
	}
}

func TestSetDeploymentImage_ContainerNotFound(t *testing.T) {
	cs := newFakeCluster(t)
	if err := SetDeploymentImage(context.Background(), cs, "default", "web", "nginx:1.26", "nonexistent", false); err == nil {
		t.Error("expected error for nonexistent container")
	}
}

func TestChooseContainerForLogs(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 5},
				{Name: "sidecar", RestartCount: 0},
			},
		},
	}

	if got := ChooseContainerForLogs(pod, "sidecar"); got != "sidecar" {
		t.Errorf("expected sidecar, got %s", got)
	}
	if got := ChooseContainerForLogs(pod, ""); got != "app" {
		t.Errorf("expected app (most restarts), got %s", got)
	}
	if got := ChooseContainerForLogs(nil, "fallback"); got != "fallback" {
		t.Errorf("expected fallback, got %s", got)
	}
	if got := ChooseContainerForLogs(pod, "nonexistent"); got != "app" {
		t.Errorf("expected app, got %s", got)
	}
}

func TestDeploymentToText(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(3),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:1.25"}},
				},
			},
		},
	}
	got := DeploymentToText(dep)
	if !strings.Contains(got, "name=web") || !strings.Contains(got, "replicas=3") || !strings.Contains(got, "app=nginx:1.25") {
		t.Errorf("unexpected output: %s", got)
	}
	if DeploymentToText(nil) != "" {
		t.Error("nil deployment should return empty string")
	}
}
