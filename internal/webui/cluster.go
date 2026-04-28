package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clusterPodView is the JSON shape served to the Cluster page table.
// It mirrors the columns the operator sees: identity + lifecycle + image
// + last termination state (so OOMKilled jumps out without opening logs).
type clusterPodView struct {
	Namespace      string   `json:"namespace"`
	Name           string   `json:"name"`
	Phase          string   `json:"phase"`
	Ready          bool     `json:"ready"`
	Restarts       int32    `json:"restarts"`
	Age            string   `json:"age"`
	Node           string   `json:"node"`
	Containers     []string `json:"containers"`
	Images         []string `json:"images"`
	LastTermReason string   `json:"last_term_reason,omitempty"`
}

// handleClusterNamespaces returns the namespaces the Cluster page may
// inspect. Today this is the same allowlist the remediation loop uses
// (INCLUDE_NAMESPACES); when empty, the GUI declares no namespaces are
// configured rather than fanning out to the whole cluster, which would
// be surprising and slow on large clusters.
func (s *Server) handleClusterNamespaces(w http.ResponseWriter, _ *http.Request) {
	out := make([]string, 0, len(s.opts.IncludeNamespaces))
	out = append(out, s.opts.IncludeNamespaces...)
	sort.Strings(out)
	writeJSON(w, http.StatusOK, map[string]any{
		"namespaces": out,
	})
}

// handleClusterPods lists pods in the requested namespace plus a few
// derived fields. Phase filter and substring-match name filter happen
// server-side so the table stays small over the wire even on busy
// namespaces.
func (s *Server) handleClusterPods(w http.ResponseWriter, r *http.Request) {
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	if ns == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("namespace query parameter required"))
		return
	}
	if !s.namespaceAllowed(ns) {
		writeJSONError(w, http.StatusForbidden, fmt.Errorf("namespace %q not in INCLUDE_NAMESPACES", ns))
		return
	}

	phaseFilter := strings.TrimSpace(r.URL.Query().Get("phase"))
	nameFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("name")))

	pods, err := s.opts.Clientset.CoreV1().Pods(ns).List(r.Context(), metav1.ListOptions{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	now := time.Now()
	out := make([]clusterPodView, 0, len(pods.Items))
	for _, p := range pods.Items {
		if phaseFilter != "" && !strings.EqualFold(string(p.Status.Phase), phaseFilter) {
			continue
		}
		if nameFilter != "" && !strings.Contains(strings.ToLower(p.Name), nameFilter) {
			continue
		}

		ready := true
		var restarts int32
		var lastTermReason string
		for _, cs := range p.Status.ContainerStatuses {
			if !cs.Ready {
				ready = false
			}
			restarts += cs.RestartCount
			if cs.LastTerminationState.Terminated != nil && lastTermReason == "" {
				lastTermReason = cs.LastTerminationState.Terminated.Reason
			}
			if cs.State.Waiting != nil && lastTermReason == "" {
				lastTermReason = cs.State.Waiting.Reason
			}
		}
		// Pod with no ContainerStatuses (Pending) defaults to not-ready.
		if len(p.Status.ContainerStatuses) == 0 {
			ready = false
		}

		containers := make([]string, 0, len(p.Spec.Containers))
		images := make([]string, 0, len(p.Spec.Containers))
		for _, c := range p.Spec.Containers {
			containers = append(containers, c.Name)
			images = append(images, c.Image)
		}

		out = append(out, clusterPodView{
			Namespace:      p.Namespace,
			Name:           p.Name,
			Phase:          string(p.Status.Phase),
			Ready:          ready,
			Restarts:       restarts,
			Age:            humanAge(now.Sub(p.CreationTimestamp.Time)),
			Node:           p.Spec.NodeName,
			Containers:     containers,
			Images:         images,
			LastTermReason: lastTermReason,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": ns,
		"pods":      out,
		"counts":    podPhaseCounts(out),
	})
}

func podPhaseCounts(pods []clusterPodView) map[string]int {
	out := map[string]int{}
	for _, p := range pods {
		out[p.Phase]++
	}
	return out
}

// handleClusterPodLogs serves a one-shot tail of the pod logs. SSE-style
// streaming exists already on the Logs page (for the agent itself); for
// arbitrary cluster pods a simple tail is sufficient and keeps the UI
// painless to reason about.
func (s *Server) handleClusterPodLogs(w http.ResponseWriter, r *http.Request) {
	ns := strings.TrimSpace(r.URL.Query().Get("namespace"))
	pod := strings.TrimSpace(r.URL.Query().Get("pod"))
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	previous := r.URL.Query().Get("previous") == "true"

	if ns == "" || pod == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("namespace and pod are required"))
		return
	}
	if !s.namespaceAllowed(ns) {
		writeJSONError(w, http.StatusForbidden, fmt.Errorf("namespace %q not in INCLUDE_NAMESPACES", ns))
		return
	}

	tail := s.opts.PodLogTailLines
	if t := strings.TrimSpace(r.URL.Query().Get("tail")); t != "" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 && n <= 5000 {
			tail = n
		}
	}
	if tail <= 0 {
		tail = 200
	}

	req := s.opts.Clientset.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  previous,
	})
	stream, err := req.Stream(r.Context())
	if err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, err)
			return
		}
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 32*1024)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}

// namespaceAllowed enforces the INCLUDE_NAMESPACES allowlist for cluster
// inspection. We deliberately do NOT widen to "anything the SA can list"
// because the operator's intent (encoded in INCLUDE_NAMESPACES) is the
// canonical "what is the agent meant to see" boundary.
func (s *Server) namespaceAllowed(ns string) bool {
	for _, n := range s.opts.IncludeNamespaces {
		if n == ns {
			return true
		}
	}
	return false
}

// handleRecentDecisions returns the in-memory ring buffer of the latest
// decisions made by the agent. Empty when the agent has just started and
// has not produced any decisions yet.
func (s *Server) handleRecentDecisions(w http.ResponseWriter, _ *http.Request) {
	if s.opts.Decisions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"decisions": []DecisionRecord{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decisions": s.opts.Decisions.Snapshot(),
	})
}
