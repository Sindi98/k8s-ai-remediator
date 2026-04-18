package kube

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestInferDeploymentFromPodName(t *testing.T) {
	cs := newFakeCluster(t) // has deployment "web"
	ctx := context.Background()

	// Standard Kubernetes pod name: {deployment}-{rs-hash}-{pod-hash}
	name, ok := InferDeploymentFromPodName(ctx, cs, "default", "web-5c8d8c8ffc-28wp6")
	if !ok || name != "web" {
		t.Errorf("expected web, got %q ok=%v", name, ok)
	}

	// Deployment name with dashes
	csMulti := fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-flaky-app", Namespace: "default"},
	})
	name, ok = InferDeploymentFromPodName(ctx, csMulti, "default", "my-flaky-app-7c9d8cd456-rw2vp")
	if !ok || name != "my-flaky-app" {
		t.Errorf("expected my-flaky-app, got %q ok=%v", name, ok)
	}

	// Candidate deployment does not exist -> not ok
	if name, ok := InferDeploymentFromPodName(ctx, cs, "default", "nonexistent-abc-123"); ok {
		t.Errorf("expected not-ok for missing deployment, got %q", name)
	}

	// Pod name without enough segments (no owner hash pattern) -> not ok
	if _, ok := InferDeploymentFromPodName(ctx, cs, "default", "lonelypod"); ok {
		t.Error("expected not-ok for pod name without controller suffix")
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

	name, err := ResolveDeploymentTarget(ctx, cs, "default", "Deployment", "web", nil)
	if err != nil || name != "web" {
		t.Errorf("expected web, got %s, err=%v", name, err)
	}

	name, err = ResolveDeploymentTarget(ctx, cs, "default", "Pod", "web-abc-123", nil)
	if err != nil || name != "web" {
		t.Errorf("expected web, got %s, err=%v", name, err)
	}

	_, err = ResolveDeploymentTarget(ctx, cs, "default", "Service", "svc", nil)
	if err == nil {
		t.Error("expected error for unsupported kind")
	}
}

func TestResolveDeploymentTarget_DeploymentNameParam(t *testing.T) {
	cs := fake.NewSimpleClientset()
	ctx := context.Background()

	// Pod does not exist, but params carry the deployment name: must succeed.
	name, err := ResolveDeploymentTarget(ctx, cs, "default", "Pod", "stale-pod-xyz",
		map[string]string{"deployment_name": "flaky-probe"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "flaky-probe" {
		t.Errorf("expected flaky-probe, got %q", name)
	}

	// "deployment" alias is also accepted.
	name, err = ResolveDeploymentTarget(ctx, cs, "default", "Pod", "stale-pod-xyz",
		map[string]string{"deployment": "api"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "api" {
		t.Errorf("expected api, got %q", name)
	}

	// Empty value must not short-circuit; falls back to kind/name resolution.
	cs = newFakeCluster(t)
	name, err = ResolveDeploymentTarget(ctx, cs, "default", "Pod", "web-abc-123",
		map[string]string{"deployment_name": "  "})
	if err != nil || name != "web" {
		t.Errorf("expected web via ownerRefs, got %q err=%v", name, err)
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

func TestSetDeploymentImage_NoopRejected(t *testing.T) {
	cs := newFakeCluster(t) // container "app" already runs "nginx:1.25"
	err := SetDeploymentImage(context.Background(), cs, "default", "web", "nginx:1.25", "app", false)
	if err == nil || !strings.Contains(err.Error(), "no-op") {
		t.Errorf("expected no-op error when proposed image equals current, got %v", err)
	}
	// Sanity: same image with dry-run also rejected (we want the signal, not the apply).
	err = SetDeploymentImage(context.Background(), cs, "default", "web", "nginx:1.25", "app", true)
	if err == nil || !strings.Contains(err.Error(), "no-op") {
		t.Errorf("expected no-op error in dry-run too, got %v", err)
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

// newPatchableCluster returns a fake cluster with a deployment that opts in
// to all patch_* scopes, suitable for exercising patch_probe, patch_resources
// and patch_registry end-to-end.
func newPatchableCluster(scopes string) *fake.Clientset {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app", Namespace: "default",
			Annotations: map[string]string{AllowPatchAnnotation: scopes},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "wrong.registry.io/myrepo/app:v1.2.3",
						ReadinessProbe: &corev1.Probe{
							InitialDelaySeconds: 0,
							PeriodSeconds:       5,
							FailureThreshold:    1,
							TimeoutSeconds:      1,
						},
					}},
				},
			},
		},
	}
	return fake.NewSimpleClientset(dep)
}

func TestDeploymentAllowsPatch(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AllowPatchAnnotation: "probe, resources"},
		},
	}
	if !DeploymentAllowsPatch(dep, "probe") {
		t.Error("probe should be allowed")
	}
	if !DeploymentAllowsPatch(dep, "resources") {
		t.Error("resources should be allowed")
	}
	if DeploymentAllowsPatch(dep, "registry") {
		t.Error("registry should not be allowed")
	}

	dep.Annotations[AllowPatchAnnotation] = "*"
	if !DeploymentAllowsPatch(dep, "registry") {
		t.Error("wildcard should allow any scope")
	}

	dep.Annotations = nil
	if DeploymentAllowsPatch(dep, "probe") {
		t.Error("missing annotation should block all patches")
	}
	if DeploymentAllowsPatch(nil, "probe") {
		t.Error("nil deployment should not allow patch")
	}
}

func TestPatchDeploymentProbe(t *testing.T) {
	cs := newPatchableCluster("probe")
	ctx := context.Background()

	err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "readiness",
		map[string]string{
			"initial_delay_seconds": "10",
			"failure_threshold":     "5",
		}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "app", metav1.GetOptions{})
	p := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if p.InitialDelaySeconds != 10 || p.FailureThreshold != 5 {
		t.Errorf("expected probe fields updated, got initial=%d failure=%d",
			p.InitialDelaySeconds, p.FailureThreshold)
	}
}

func TestPatchDeploymentProbe_RequiresOptIn(t *testing.T) {
	cs := newPatchableCluster("resources")
	ctx := context.Background()

	err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "readiness",
		map[string]string{"initial_delay_seconds": "10"}, false)
	if err == nil || !strings.Contains(err.Error(), "opt in") {
		t.Errorf("expected opt-in error, got %v", err)
	}
}

func TestPatchDeploymentProbe_InvalidInputs(t *testing.T) {
	cs := newPatchableCluster("probe")
	ctx := context.Background()

	// Out of bounds
	if err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "readiness",
		map[string]string{"initial_delay_seconds": "999999"}, false); err == nil {
		t.Error("expected out-of-bounds error")
	}
	// Unknown probe type
	if err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "startup",
		map[string]string{"initial_delay_seconds": "10"}, false); err == nil {
		t.Error("expected error for unknown probe type")
	}
	// No fields
	if err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "readiness",
		map[string]string{}, false); err == nil {
		t.Error("expected error for empty fields")
	}
	// Unknown field
	if err := PatchDeploymentProbe(ctx, cs, "default", "app", "main", "readiness",
		map[string]string{"httpGet_path": "/"}, false); err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestPatchDeploymentResources(t *testing.T) {
	cs := newPatchableCluster("resources")
	ctx := context.Background()

	err := PatchDeploymentResources(ctx, cs, "default", "app", "main",
		map[string]string{
			"cpu_request":    "100m",
			"memory_request": "128Mi",
			"memory_limit":   "256Mi",
		}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "app", metav1.GetOptions{})
	r := dep.Spec.Template.Spec.Containers[0].Resources
	if r.Requests[corev1.ResourceCPU] != resource.MustParse("100m") {
		t.Errorf("expected cpu_request=100m, got %s", r.Requests.Cpu())
	}
	if r.Limits[corev1.ResourceMemory] != resource.MustParse("256Mi") {
		t.Errorf("expected memory_limit=256Mi, got %s", r.Limits.Memory())
	}
}

func TestPatchDeploymentResources_RequestExceedsLimit(t *testing.T) {
	cs := newPatchableCluster("resources")
	ctx := context.Background()

	err := PatchDeploymentResources(ctx, cs, "default", "app", "main",
		map[string]string{
			"memory_request": "512Mi",
			"memory_limit":   "256Mi",
		}, false)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("expected requests > limits error, got %v", err)
	}
}

func TestPatchDeploymentResources_OutOfBounds(t *testing.T) {
	cs := newPatchableCluster("resources")
	ctx := context.Background()

	if err := PatchDeploymentResources(ctx, cs, "default", "app", "main",
		map[string]string{"memory_request": "64Gi"}, false); err == nil {
		t.Error("expected out-of-bounds error on huge memory request")
	}
	if err := PatchDeploymentResources(ctx, cs, "default", "app", "main",
		map[string]string{"cpu_request": "1m"}, false); err == nil {
		t.Error("expected out-of-bounds error on tiny cpu request")
	}
}

func TestSwapRegistry(t *testing.T) {
	cases := []struct {
		image, registry, want string
	}{
		{"wrong.registry.io/repo/app:v1", "host.docker.internal:5050", "host.docker.internal:5050/repo/app:v1"},
		{"nginx:1.25", "host.docker.internal:5050", "host.docker.internal:5050/nginx:1.25"},
		{"localhost:5000/foo:bar", "host.docker.internal:5050", "host.docker.internal:5050/foo:bar"},
		{"library/nginx:1.25", "host.docker.internal:5050", "host.docker.internal:5050/library/nginx:1.25"},
	}
	for _, c := range cases {
		got, err := SwapRegistry(c.image, c.registry)
		if err != nil {
			t.Errorf("SwapRegistry(%q,%q) err=%v", c.image, c.registry, err)
			continue
		}
		if got != c.want {
			t.Errorf("SwapRegistry(%q,%q)=%q want %q", c.image, c.registry, got, c.want)
		}
	}
}

func TestPatchDeploymentRegistry(t *testing.T) {
	cs := newPatchableCluster("registry")
	ctx := context.Background()

	err := PatchDeploymentRegistry(ctx, cs, "default", "app", "main", "host.docker.internal:5050", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "app", metav1.GetOptions{})
	got := dep.Spec.Template.Spec.Containers[0].Image
	want := "host.docker.internal:5050/myrepo/app:v1.2.3"
	if got != want {
		t.Errorf("expected image %q, got %q", want, got)
	}
}

func TestPatchDeploymentRegistry_NoChange(t *testing.T) {
	cs := newPatchableCluster("registry")
	ctx := context.Background()
	// Same registry as the container already uses -> should error
	err := PatchDeploymentRegistry(ctx, cs, "default", "app", "main", "wrong.registry.io", false)
	if err == nil || !strings.Contains(err.Error(), "no change") {
		t.Errorf("expected no-change error, got %v", err)
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
	if !strings.Contains(got, "Allow-patch scopes") {
		t.Errorf("expected allow-patch scopes line, got %s", got)
	}
	if DeploymentToText(nil) != "" {
		t.Error("nil deployment should return empty string")
	}
}

func TestDeploymentToText_IncludesProbeAndAnnotation(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "flaky",
			Annotations: map[string]string{AllowPatchAnnotation: "probe,resources"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "app", Image: "busybox:1.36",
						ReadinessProbe: &corev1.Probe{
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
							FailureThreshold:    2,
							TimeoutSeconds:      2,
							SuccessThreshold:    1,
						},
					}},
				},
			},
		},
	}
	got := DeploymentToText(dep)
	for _, want := range []string{
		"probe,resources",
		"app readinessProbe",
		"failureThreshold=2",
		"period=5",
		"timeout=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in output, got:\n%s", want, got)
		}
	}
}
