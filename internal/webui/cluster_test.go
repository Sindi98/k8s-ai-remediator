package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestServer(t *testing.T, includeNS []string, pods ...*corev1.Pod) *Server {
	t.Helper()
	objs := make([]runtimePodWrapper, 0, len(pods))
	_ = objs
	cs := fake.NewSimpleClientset()
	for _, p := range pods {
		_, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
		if err != nil {
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
		IncludeNamespaces: includeNS,
		Clientset:         cs,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// runtimePodWrapper exists only to keep the import "k8s.io/apimachinery/pkg/runtime"
// from being needed; the slice itself is unused.
type runtimePodWrapper struct{}

func makePod(ns, name, phase string, ready bool, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Minute)),
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "nginx:latest",
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPhase(phase),
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        ready,
				RestartCount: restarts,
			}},
		},
	}
}

func TestClusterPodsRequiresIncludeNamespacesAllowlist(t *testing.T) {
	s := newTestServer(t, []string{"allowed-ns"},
		makePod("allowed-ns", "p1", "Running", true, 0),
		makePod("other-ns",   "p2", "Running", true, 0),
	)

	// allowed namespace returns the pod
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/cluster/pods?namespace=allowed-ns", nil)
	s.handleClusterPods(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("allowed-ns: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var allowed struct {
		Pods []clusterPodView `json:"pods"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &allowed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(allowed.Pods) != 1 || allowed.Pods[0].Name != "p1" {
		t.Fatalf("allowed-ns pods = %+v, want [p1]", allowed.Pods)
	}

	// non-allowlisted namespace must be 403, not 200 with empty pods
	rr2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/api/cluster/pods?namespace=other-ns", nil)
	s.handleClusterPods(rr2, r2)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("other-ns: got %d, want 403", rr2.Code)
	}
}

func TestClusterPodsPhaseAndNameFilters(t *testing.T) {
	s := newTestServer(t, []string{"ns"},
		makePod("ns", "alpha-running",   "Running", true, 0),
		makePod("ns", "alpha-pending",   "Pending", false, 0),
		makePod("ns", "beta-running",    "Running", true, 0),
	)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet,
		"/api/cluster/pods?namespace=ns&phase=Running&name=alpha", nil)
	s.handleClusterPods(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Pods   []clusterPodView `json:"pods"`
		Counts map[string]int   `json:"counts"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Pods) != 1 || got.Pods[0].Name != "alpha-running" {
		t.Fatalf("filter result = %+v, want [alpha-running]", got.Pods)
	}
}

func TestClusterNamespacesReturnsAllowlist(t *testing.T) {
	s := newTestServer(t, []string{"b", "a"})
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/cluster/namespaces", nil)
	s.handleClusterNamespaces(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rr.Code)
	}
	var got struct {
		Namespaces []string `json:"namespaces"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	want := []string{"a", "b"} // sorted
	if len(got.Namespaces) != 2 || got.Namespaces[0] != want[0] || got.Namespaces[1] != want[1] {
		t.Fatalf("got %v, want %v", got.Namespaces, want)
	}
}
