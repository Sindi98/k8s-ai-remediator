package webui

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// handleLogsStream proxies the live log stream of one of the agent pods to
// the browser using Server-Sent Events. Each agent log line becomes one
// "data:" record so the client can append it to a scrollable view.
//
// The stream terminates when either side disconnects or the pod logs end.
// Multiple replicas are exposed via the optional ?pod=<name> query param;
// without it the first pod of the Deployment is used.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	pod := strings.TrimSpace(r.URL.Query().Get("pod"))
	tail := s.opts.PodLogTailLines
	if tail <= 0 {
		tail = 200
	}

	ctx := r.Context()

	if pod == "" {
		pods, err := s.opts.Clientset.CoreV1().Pods(s.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + s.opts.DeploymentName,
		})
		if err != nil || len(pods.Items) == 0 {
			http.Error(w, "no agent pods found", http.StatusNotFound)
			return
		}
		pod = pods.Items[0].Name
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering on nginx-style ingresses

	req := s.opts.Clientset.CoreV1().Pods(s.opts.Namespace).GetLogs(pod, &corev1.PodLogOptions{
		Follow:    true,
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
		return
	}
	defer stream.Close()

	fmt.Fprintf(w, "event: ready\ndata: streaming logs from %s\n\n", pod)
	flusher.Flush()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024) // long JSON log lines fit comfortably
	for scanner.Scan() {
		line := scanner.Text()
		// SSE: a literal newline inside data: terminates the field, so
		// pre-emptively replace any embedded newline with a marker. Our
		// agent uses single-line JSON so this is a defensive guard.
		safe := strings.ReplaceAll(line, "\n", "\\n")
		fmt.Fprintf(w, "data: %s\n\n", safe)
		flusher.Flush()
		if ctx.Err() != nil {
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
	}
}
