package webui

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// scenarioState is the lifecycle classification rendered on the UI card.
const (
	scenarioStateNotApplied = "not_applied"
	scenarioStatePending    = "pending"
	scenarioStateError      = "error"
	scenarioStateResolved   = "resolved"
)

// scenarioStatusView is the JSON shape served to the Scenarios page monitor.
type scenarioStatusView struct {
	Name    string                  `json:"name"`
	State   string                  `json:"state"`
	Summary string                  `json:"summary"`
	Pods    []scenarioPodStatusView `json:"pods,omitempty"`
}

type scenarioPodStatusView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	Reason    string `json:"reason,omitempty"`
}

// handleScenariosStatus inspects every scenario YAML, finds the pods owned
// by its Deployments and classifies the current lifecycle state. The
// Scenarios page polls this endpoint to render a live monitor next to each
// card and flip it to "resolved" once the underlying error condition is gone.
func (s *Server) handleScenariosStatus(w http.ResponseWriter, r *http.Request) {
	scenarios := s.listScenarios()
	out := make([]scenarioStatusView, 0, len(scenarios))
	for _, sc := range scenarios {
		out = append(out, s.scenarioStatus(r.Context(), sc.Name))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scenarios": out,
	})
}

// scenarioStatus loads a scenario YAML and derives a lifecycle state from
// the live pods in the sandbox namespace. The classification is best-effort:
// transient errors are reported as the current state without retries so the
// monitor stays responsive.
func (s *Server) scenarioStatus(ctx context.Context, name string) scenarioStatusView {
	out := scenarioStatusView{
		Name:    name,
		State:   scenarioStateNotApplied,
		Summary: "Scenario not applied",
	}
	objs, err := s.loadScenario(name)
	if err != nil || len(objs) == 0 {
		out.Summary = "Scenario YAML unreadable"
		return out
	}

	type ownerSpec struct {
		ns       string
		selector labels.Selector
	}
	var owners []ownerSpec
	deploymentExists := false
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" {
			continue
		}
		ns := obj.GetNamespace()
		if ns == "" {
			continue
		}
		// Probe the Deployment so we can distinguish "applied but no pods
		// yet" from "never applied".
		_, err := s.opts.Clientset.AppsV1().Deployments(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err == nil {
			deploymentExists = true
		} else if !apierrors.IsNotFound(err) {
			out.Summary = fmt.Sprintf("lookup error: %v", err)
		}
		sel, ok := unstructuredMatchLabels(obj)
		if !ok {
			continue
		}
		owners = append(owners, ownerSpec{ns: ns, selector: sel})
	}
	if len(owners) == 0 {
		return out
	}

	var allPods []corev1.Pod
	for _, o := range owners {
		list, err := s.opts.Clientset.CoreV1().Pods(o.ns).List(ctx, metav1.ListOptions{
			LabelSelector: o.selector.String(),
		})
		if err != nil {
			out.Summary = fmt.Sprintf("pod list error: %v", err)
			continue
		}
		allPods = append(allPods, list.Items...)
	}

	if len(allPods) == 0 {
		if deploymentExists {
			out.State = scenarioStatePending
			out.Summary = "Deployment present, no pods yet"
		}
		return out
	}

	hasError := false
	allReady := true
	var firstReason string
	out.Pods = make([]scenarioPodStatusView, 0, len(allPods))
	for i := range allPods {
		p := &allPods[i]
		ready := podIsReady(p)
		if !ready {
			allReady = false
		}
		reason := podErrorReason(p)
		if reason != "" {
			hasError = true
			if firstReason == "" {
				firstReason = reason
			}
		}
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
		out.Pods = append(out.Pods, scenarioPodStatusView{
			Namespace: p.Namespace,
			Name:      p.Name,
			Phase:     string(p.Status.Phase),
			Ready:     ready,
			Restarts:  restarts,
			Reason:    reason,
		})
	}

	switch {
	case hasError:
		out.State = scenarioStateError
		out.Summary = firstReason
	case allReady:
		out.State = scenarioStateResolved
		out.Summary = "All pods Ready"
	default:
		out.State = scenarioStatePending
		out.Summary = "Pods not Ready yet"
	}
	return out
}

// unstructuredMatchLabels extracts spec.selector.matchLabels from a
// Deployment-shaped unstructured object. Returns false when the field is
// missing so the caller can skip resources without a label selector.
func unstructuredMatchLabels(obj *unstructured.Unstructured) (labels.Selector, bool) {
	ml, found, err := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if !found || err != nil || len(ml) == 0 {
		return nil, false
	}
	return labels.SelectorFromSet(labels.Set(ml)), true
}

func podIsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podErrorReason maps known pod-level failure modes to a short string.
// Empty return means "no detectable error". Order matters: scheduling is
// checked first because such pods have no container statuses to inspect.
func podErrorReason(p *corev1.Pod) string {
	if p.Status.Phase == corev1.PodPending && p.Spec.NodeName == "" {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				if c.Reason != "" {
					return c.Reason
				}
				return "Unschedulable"
			}
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			switch r := cs.State.Waiting.Reason; r {
			case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
				"CreateContainerConfigError", "InvalidImageName",
				"RunContainerError", "ContainerCannotRun":
				return r
			}
		}
		if cs.LastTerminationState.Terminated != nil {
			if r := cs.LastTerminationState.Terminated.Reason; r == "OOMKilled" {
				return r
			}
		}
	}
	return ""
}
