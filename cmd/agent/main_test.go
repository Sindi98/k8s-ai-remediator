package main

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/tuo-user/k8s-ai-remediator/internal/config"
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
		if err := executeDecision(ctx, cs, d, cfg); err != nil {
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
	if err := executeDecision(ctx, cs, d, defaultCfg()); err != nil {
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
	if err := executeDecision(ctx, cs, d, defaultCfg()); err != nil {
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
	if err := executeDecision(ctx, cs, d, defaultCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dep, _ := cs.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 4 {
		t.Errorf("expected 4 replicas, got %d", *dep.Spec.Replicas)
	}
}

func TestExecuteDecision_UnsupportedAction(t *testing.T) {
	cs := newFakeCluster(t)
	d := model.Decision{Action: model.Action("unknown_action"), Namespace: "default"}
	if err := executeDecision(context.Background(), cs, d, defaultCfg()); err == nil {
		t.Error("expected error for unsupported action")
	}
}
