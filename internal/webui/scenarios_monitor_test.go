package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// scenarioMonitorTestServer wires up a Server with the embedded scenarios
// FS and a fake clientset preloaded with the supplied k8s objects. The
// scenarios on disk reference namespace=incident-lab; we mirror that here
// so the monitor finds them.
func scenarioMonitorTestServer(t *testing.T, deploys []*appsv1.Deployment, pods []*corev1.Pod) *Server {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for _, d := range deploys {
		if _, err := cs.AppsV1().Deployments(d.Namespace).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed deployment: %v", err)
		}
	}
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod: %v", err)
		}
	}
	s, err := New(Options{
		Username:          "u",
		Password:          "p",
		Namespace:         "ai-remediator",
		DeploymentName:    "ai-remediator-agent",
		ConfigMapName:     "ai-remediator-config",
		SecretName:        "ai-remediator-secrets",
		SandboxNamespaces: []string{"incident-lab"},
		Clientset:         cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func deploymentFor(name string, matchLabels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "incident-lab"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

func podWithLabels(name string, labels map[string]string, status corev1.PodStatus, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "incident-lab",
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Minute)),
		},
		Spec:   corev1.PodSpec{NodeName: nodeName, Containers: []corev1.Container{{Name: "app", Image: "x"}}},
		Status: status,
	}
}

func readyStatus(ready bool, restarts int32, waitReason, lastTermReason string) corev1.PodStatus {
	cs := corev1.ContainerStatus{
		Name:         "app",
		Ready:        ready,
		RestartCount: restarts,
	}
	if waitReason != "" {
		cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitReason}}
	}
	if lastTermReason != "" {
		cs.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: lastTermReason}}
	}
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{cs},
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
	}
}

// TestScenarioStatusFlipsToResolvedWhenPodsAreReady mirrors the operator
// flow: the OOM scenario starts crashing, then once the Deployment is
// patched and the pod becomes Ready the monitor must mark it resolved.
func TestScenarioStatusFlipsToResolvedWhenPodsAreReady(t *testing.T) {
	dep := deploymentFor("memory-hog", map[string]string{"app": "memory-hog"})
	errorPod := podWithLabels("memory-hog-1",
		map[string]string{"app": "memory-hog", "scenario": "critical"},
		readyStatus(false, 5, "CrashLoopBackOff", "OOMKilled"),
		"node-1",
	)
	s := scenarioMonitorTestServer(t, []*appsv1.Deployment{dep}, []*corev1.Pod{errorPod})

	got := s.scenarioStatus(context.Background(), "critical-oomkilled")
	if got.State != scenarioStateError {
		t.Fatalf("error pod: state=%q want %q (summary=%q)", got.State, scenarioStateError, got.Summary)
	}
	if got.Summary != "CrashLoopBackOff" {
		t.Fatalf("error pod summary=%q want CrashLoopBackOff", got.Summary)
	}

	// Simulate operator/agent fix: replace the pod with a Ready one.
	healthy := podWithLabels("memory-hog-2",
		map[string]string{"app": "memory-hog", "scenario": "critical"},
		readyStatus(true, 0, "", ""),
		"node-1",
	)
	s2 := scenarioMonitorTestServer(t, []*appsv1.Deployment{dep}, []*corev1.Pod{healthy})
	got2 := s2.scenarioStatus(context.Background(), "critical-oomkilled")
	if got2.State != scenarioStateResolved {
		t.Fatalf("ready pod: state=%q want %q (summary=%q)", got2.State, scenarioStateResolved, got2.Summary)
	}
}

func TestScenarioStatusNotAppliedWhenNothingDeployed(t *testing.T) {
	s := scenarioMonitorTestServer(t, nil, nil)
	got := s.scenarioStatus(context.Background(), "medium-imagepullbackoff")
	if got.State != scenarioStateNotApplied {
		t.Fatalf("state=%q want %q", got.State, scenarioStateNotApplied)
	}
}

func TestScenarioStatusUnschedulablePending(t *testing.T) {
	dep := deploymentFor("unschedulable", map[string]string{"app": "unschedulable"})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unschedulable-1", Namespace: "incident-lab",
			Labels: map[string]string{"app": "unschedulable"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "x"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"},
			},
		},
	}
	s := scenarioMonitorTestServer(t, []*appsv1.Deployment{dep}, []*corev1.Pod{pod})
	got := s.scenarioStatus(context.Background(), "severe-failedscheduling")
	if got.State != scenarioStateError {
		t.Fatalf("state=%q want %q (summary=%q)", got.State, scenarioStateError, got.Summary)
	}
	if got.Summary != "Unschedulable" {
		t.Fatalf("summary=%q want Unschedulable", got.Summary)
	}
}

func TestScenariosStatusEndpointReturnsAll(t *testing.T) {
	s := scenarioMonitorTestServer(t, nil, nil)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/scenarios/status", nil)
	s.handleScenariosStatus(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Scenarios []scenarioStatusView `json:"scenarios"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// One entry per YAML in the embedded scenarios FS (4 today).
	if len(got.Scenarios) < 1 {
		t.Fatalf("expected at least one scenario in the response")
	}
	for _, sc := range got.Scenarios {
		if sc.State != scenarioStateNotApplied {
			t.Fatalf("scenario %s state=%q want not_applied", sc.Name, sc.State)
		}
	}
}
