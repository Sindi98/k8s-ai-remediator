package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tuo-user/k8s-ai-remediator/internal/config"
	"github.com/tuo-user/k8s-ai-remediator/internal/kube"
	"github.com/tuo-user/k8s-ai-remediator/internal/model"
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

func defaultCfg() config.AgentConfig {
	return config.AgentConfig{
		DryRun:               false,
		MinScale:             1,
		MaxScale:             5,
		AllowImageUpdates:    false,
		ImageUpdateThreshold: 0.9,
		PodLogTailLines:      200,
	}
}

func TestExecuteDecision_Noop(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()
	cfg := defaultCfg()

	for _, action := range []model.Action{model.ActionNoop, model.ActionAskHuman, model.ActionMarkForManualFix} {
		d := model.Decision{Action: action, Namespace: "default", ResourceKind: "Pod", ResourceName: "web-abc-123"}
		if err := executeDecision(ctx, cs, d, cfg, "", ""); err != nil {
			t.Errorf("action %s should succeed: %v", action, err)
		}
	}
}

func TestExecuteDecision_RestartDeployment(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "web",
	}
	if err := executeDecision(ctx, cs, d, defaultCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected restart annotation")
	}
}

func TestExecuteDecision_DeletePod(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	d := model.Decision{
		Action: model.ActionDeleteFailedPod, Namespace: "default",
		ResourceKind: "Pod", ResourceName: "web-abc-123",
	}
	if err := executeDecision(ctx, cs, d, defaultCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err := cs.CoreV1().Pods("default").Get(ctx, "web-abc-123", metav1.GetOptions{})
	if err == nil {
		t.Error("pod should have been deleted")
	}
}

func TestExecuteDecision_ScaleDeployment(t *testing.T) {
	cs := newFakeCluster(t)
	ctx := context.Background()

	d := model.Decision{
		Action: model.ActionScaleDeployment, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "web",
		Parameters: map[string]string{"replicas": "4"},
	}
	if err := executeDecision(ctx, cs, d, defaultCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 4 {
		t.Errorf("expected 4 replicas, got %d", *dep.Spec.Replicas)
	}
}

func TestExecuteDecision_RestartDeployment_PodGoneUsesParamName(t *testing.T) {
	// Regression: the LLM often reports kind=Pod with a pod name that no
	// longer exists (stale event or rolled-over ReplicaSet). When params
	// carry deployment_name we must use it instead of failing the pod lookup.
	cs := newFakeCluster(t)
	ctx := context.Background()

	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Pod", ResourceName: "web-gone-5c8d8c8ffc-28wp6",
		Parameters: map[string]string{"deployment_name": "web"},
	}
	if err := executeDecision(ctx, cs, d, defaultCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if _, ok := dep.Spec.Template.Annotations["ai-remediator/restarted-at"]; !ok {
		t.Error("expected restart annotation on deployment resolved via params")
	}
}

func TestExecuteDecision_BlocksRestartForUnhealthy(t *testing.T) {
	cs := newFakeCluster(t)
	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "web",
	}
	err := executeDecision(context.Background(), cs, d, defaultCfg(), "Unhealthy", "")
	if err == nil {
		t.Error("restart_deployment on event reason=Unhealthy should be blocked")
	}
	// Sanity: other reasons still allow restart_deployment.
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "BackOff", ""); err != nil {
		t.Errorf("restart_deployment on BackOff should pass, got %v", err)
	}
}

func TestExecuteDecision_BlocksRestartForOOMKilled(t *testing.T) {
	cs := newFakeCluster(t)
	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "web",
	}
	// Extra mentions OOMKilled -> block.
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "BackOff",
		"container=app lastTerminated reason=OOMKilled exit=137"); err == nil {
		t.Error("expected block when extra mentions OOMKilled")
	}
	// Exit code 137 alone is enough.
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "BackOff",
		"container=app exit=137"); err == nil {
		t.Error("expected block when exit=137 is present")
	}
	// Plain BackOff without OOM evidence -> allow.
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "BackOff",
		"container=app state=Running"); err != nil {
		t.Errorf("plain BackOff should not be blocked, got %v", err)
	}
}

func TestComputeBumpedMemoryLimit(t *testing.T) {
	mustQ := func(s string) resource.Quantity { return resource.MustParse(s) }

	cases := []struct {
		current string
		want    string
	}{
		{"", "256Mi"},    // nothing set → floor
		{"16Mi", "256Mi"}, // doubled (32Mi) < floor → floor wins
		{"32Mi", "256Mi"}, // doubled (64Mi) < floor → floor wins
		{"256Mi", "512Mi"},   // exact 2x above floor
		{"1Gi", "2Gi"},       // 2x scaling
		{"16Gi", "16Gi"},     // capped at MaxMemoryQuantity (16Gi)
	}
	for _, c := range cases {
		var current resource.Quantity
		if c.current != "" {
			current = mustQ(c.current)
		}
		got := computeBumpedMemoryLimit(current)
		gotQ := mustQ(got)
		wantQ := mustQ(c.want)
		if gotQ.Cmp(wantQ) != 0 {
			t.Errorf("computeBumpedMemoryLimit(%q) = %q, want %q", c.current, got, c.want)
		}
	}
}

func TestTryAutoPatchResourcesOnOOM_NoOptIn(t *testing.T) {
	// Deployment without the annotation → transformer declines.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main"}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	cfg := patchFlagsCfg()
	d := model.Decision{Action: model.ActionRestartDeployment, Namespace: "default", ResourceKind: "Deployment", ResourceName: "app"}
	_, ok := tryAutoPatchResourcesOnOOM(context.Background(), cs, d, cfg)
	if ok {
		t.Error("expected transformer to decline when annotation is absent")
	}
}

func TestTryAutoPatchResourcesOnOOM_FlagOff(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "resources")
	cfg := defaultCfg() // AllowPatchResources=false
	d := model.Decision{Action: model.ActionRestartDeployment, Namespace: "default", ResourceKind: "Deployment", ResourceName: "app"}
	_, ok := tryAutoPatchResourcesOnOOM(context.Background(), cs, d, cfg)
	if ok {
		t.Error("expected transformer to decline when ALLOW_PATCH_RESOURCES=false")
	}
}

func TestTryAutoPatchResourcesOnOOM_Success(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "memory-hog", Namespace: "default",
			Annotations: map[string]string{"ai-remediator/allow-patch": "resources"},
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
					},
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	cfg := patchFlagsCfg()

	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Pod", ResourceName: "memory-hog-xxx-yyy",
		Parameters: map[string]string{"deployment_name": "memory-hog"},
	}
	transformed, ok := tryAutoPatchResourcesOnOOM(context.Background(), cs, d, cfg)
	if !ok {
		t.Fatal("expected transformer to succeed")
	}
	if transformed.Action != model.ActionPatchResources {
		t.Errorf("expected action=patch_resources, got %s", transformed.Action)
	}
	if transformed.Parameters["container"] != "app" {
		t.Errorf("expected container=app, got %s", transformed.Parameters["container"])
	}
	// 32Mi * 2 = 64Mi < 256Mi floor → should snap to 256Mi.
	gotLimit := resource.MustParse(transformed.Parameters["memory_limit"])
	wantLimit := resource.MustParse("256Mi")
	if gotLimit.Cmp(wantLimit) != 0 {
		t.Errorf("expected memory_limit=256Mi, got %s", transformed.Parameters["memory_limit"])
	}
}

func TestExecuteDecision_AutoEscalatesOOMRestartToPatchResources(t *testing.T) {
	// End-to-end: blocked restart_deployment + OOM evidence + opt-in
	// → transformed to patch_resources and applied.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "memory-hog", Namespace: "default",
			Annotations: map[string]string{"ai-remediator/allow-patch": "resources"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "memory-hog"}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
					},
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	cfg := patchFlagsCfg()

	d := model.Decision{
		Action: model.ActionRestartDeployment, Namespace: "default",
		ResourceKind: "Pod", ResourceName: "memory-hog-xxx-yyy",
		Confidence: 0.95,
		Parameters: map[string]string{"deployment_name": "memory-hog"},
	}
	// "extra" includes the OOM marker used by the guard.
	extra := "container=app lastTerminated reason=OOMKilled exit=137"
	if err := executeDecision(context.Background(), cs, d, cfg, "BackOff", extra); err != nil {
		t.Fatalf("expected auto-escalation to succeed, got %v", err)
	}
	// Verify the Deployment was patched.
	got, _ := cs.AppsV1().Deployments("default").Get(context.Background(), "memory-hog", metav1.GetOptions{})
	newLimit := got.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	want := resource.MustParse("256Mi")
	if newLimit.Cmp(want) != 0 {
		t.Errorf("expected memory_limit=256Mi after auto-escalation, got %s", newLimit.String())
	}
}

func TestCanonicalReason(t *testing.T) {
	cases := []struct {
		reason, message, want string
	}{
		{"ErrImagePull", "", "ImagePullFailure"},
		{"ImagePullBackOff", "", "ImagePullFailure"},
		{"errimagepull", "", "ImagePullFailure"}, // case-insensitive
		{"Failed", "Failed to pull image busybox:1.36", "ImagePullFailure"},
		{"Failed", "Failed to mount volume", "Failed"}, // not image-pull
		{"BackOff", "Back-off restarting failed container", "BackOff"},
		{"Unhealthy", "Readiness probe failed", "Unhealthy"},
		{"FailedScheduling", "Insufficient cpu", "FailedScheduling"},
	}
	for _, c := range cases {
		got := canonicalReason(c.reason, c.message)
		if got != c.want {
			t.Errorf("canonicalReason(%q, %q) = %q, want %q", c.reason, c.message, got, c.want)
		}
	}
}

func TestExecuteDecision_Scale_RejectsOverflowReplicas(t *testing.T) {
	// Replicas outside int32 would silently wrap on cast; expect an error.
	cs := newFakeCluster(t)
	d := model.Decision{
		Action: model.ActionScaleDeployment, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "web",
		Parameters: map[string]string{"replicas": "9999999999"}, // > MaxInt32
	}
	err := executeDecision(context.Background(), cs, d, defaultCfg(), "", "")
	if err == nil {
		t.Error("expected error for replicas above int32 range")
	}
}

func TestDedupCache_EvictsExpiredEntries(t *testing.T) {
	c := &dedupCache{
		seen:       map[string]time.Time{},
		signalSeen: map[string]time.Time{},
	}
	old := time.Now().Add(-2 * time.Hour)
	now := time.Now()
	c.seen["old-key"] = old
	c.signalSeen["old-signal"] = old
	c.seen["fresh"] = now
	c.signalSeen["fresh-signal"] = now

	c.evict(now, 5*time.Minute, time.Hour)

	if _, ok := c.seen["old-key"]; ok {
		t.Error("expected old seen entry to be evicted")
	}
	if _, ok := c.signalSeen["old-signal"]; ok {
		t.Error("expected old signal entry to be evicted")
	}
	if _, ok := c.seen["fresh"]; !ok {
		t.Error("fresh seen entry should survive eviction")
	}
	if _, ok := c.signalSeen["fresh-signal"]; !ok {
		t.Error("fresh signal entry should survive eviction")
	}
}

func TestExecuteDecision_UnsupportedAction(t *testing.T) {
	cs := newFakeCluster(t)
	d := model.Decision{Action: model.Action("unknown_action"), Namespace: "default"}
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "", ""); err == nil {
		t.Error("expected error for unsupported action")
	}
}

func newPatchableClusterForAgent(t *testing.T, scopes string) *fake.Clientset {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app", Namespace: "default",
			Annotations: map[string]string{kube.AllowPatchAnnotation: scopes},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "wrong.registry.io/repo/app:v1",
						ReadinessProbe: &corev1.Probe{
							PeriodSeconds:    5,
							FailureThreshold: 1,
							TimeoutSeconds:   1,
						},
					}},
				},
			},
		},
	}
	return fake.NewSimpleClientset(dep)
}

func patchFlagsCfg() config.AgentConfig {
	c := defaultCfg()
	c.AllowPatchProbe = true
	c.AllowPatchResources = true
	c.AllowPatchRegistry = true
	c.PatchConfidenceThreshold = 0.9
	return c
}

func TestExecuteDecision_PatchProbe_FlagOffBlocked(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "probe")
	d := model.Decision{
		Action: model.ActionPatchProbe, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.95,
		Parameters: map[string]string{
			"container": "main", "probe": "readiness",
			"initial_delay_seconds": "10",
		},
	}
	if err := executeDecision(context.Background(), cs, d, defaultCfg(), "", ""); err == nil {
		t.Error("patch_probe should be blocked when ALLOW_PATCH_PROBE is off")
	}
}

func TestExecuteDecision_PatchProbe_BelowThresholdBlocked(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "probe")
	d := model.Decision{
		Action: model.ActionPatchProbe, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.5,
		Parameters: map[string]string{
			"container": "main", "probe": "readiness",
			"initial_delay_seconds": "10",
		},
	}
	if err := executeDecision(context.Background(), cs, d, patchFlagsCfg(), "", ""); err == nil {
		t.Error("patch_probe should be blocked below confidence threshold")
	}
}

func TestExecuteDecision_PatchProbe_AnnotationRequired(t *testing.T) {
	// Deployment without the opt-in annotation scope for probe.
	cs := newPatchableClusterForAgent(t, "resources")
	d := model.Decision{
		Action: model.ActionPatchProbe, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.95,
		Parameters: map[string]string{
			"container": "main", "probe": "readiness",
			"initial_delay_seconds": "10",
		},
	}
	if err := executeDecision(context.Background(), cs, d, patchFlagsCfg(), "", ""); err == nil {
		t.Error("patch_probe should require opt-in annotation")
	}
}

func TestExecuteDecision_PatchProbe_HappyPath(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "probe")
	ctx := context.Background()
	d := model.Decision{
		Action: model.ActionPatchProbe, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.95,
		Parameters: map[string]string{
			"container": "main", "probe": "readiness",
			"initial_delay_seconds": "10",
			"timeout_seconds":       "5",
		},
	}
	if err := executeDecision(ctx, cs, d, patchFlagsCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "app", metav1.GetOptions{})
	p := dep.Spec.Template.Spec.Containers[0].ReadinessProbe
	if p.InitialDelaySeconds != 10 || p.TimeoutSeconds != 5 {
		t.Errorf("probe fields not applied: %+v", p)
	}
}

func TestExecuteDecision_PatchResources_HappyPath(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "resources")
	ctx := context.Background()
	d := model.Decision{
		Action: model.ActionPatchResources, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.95,
		Parameters: map[string]string{
			"container":      "main",
			"memory_request": "64Mi",
			"memory_limit":   "128Mi",
		},
	}
	if err := executeDecision(ctx, cs, d, patchFlagsCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecuteDecision_PatchRegistry_HappyPath(t *testing.T) {
	cs := newPatchableClusterForAgent(t, "registry")
	ctx := context.Background()
	d := model.Decision{
		Action: model.ActionPatchRegistry, Namespace: "default",
		ResourceKind: "Deployment", ResourceName: "app", Confidence: 0.95,
		Parameters: map[string]string{
			"container":    "main",
			"new_registry": "host.docker.internal:5050",
		},
	}
	if err := executeDecision(ctx, cs, d, patchFlagsCfg(), "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "app", metav1.GetOptions{})
	if got := dep.Spec.Template.Spec.Containers[0].Image; got != "host.docker.internal:5050/repo/app:v1" {
		t.Errorf("unexpected image %q", got)
	}
}
