package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusReport is the JSON shape returned by /api/status.
type statusReport struct {
	Generated    time.Time         `json:"generated"`
	Agent        agentStatus       `json:"agent"`
	Pods         []podStatus       `json:"pods"`
	Config       map[string]string `json:"config"`
	Sandbox      []string          `json:"sandbox_namespaces"`
	Dependencies dependencyStatus  `json:"dependencies"`
}

type agentStatus struct {
	Namespace      string `json:"namespace"`
	DeploymentName string `json:"deployment"`
	Replicas       int32  `json:"replicas"`
	ReadyReplicas  int32  `json:"ready_replicas"`
}

type podStatus struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Age      string `json:"age"`
}

type dependencyStatus struct {
	ConfigMap string         `json:"configmap"`
	Secret    string         `json:"secret"`
	Lease     string         `json:"lease"`
	Ollama    *probeResult   `json:"ollama,omitempty"`
	Redis     *probeResult   `json:"redis,omitempty"`
}

type probeResult struct {
	OK        bool          `json:"ok"`
	Detail    string        `json:"detail"`
	LatencyMs time.Duration `json:"latency_ms"`
}

// handleStatus fans out the per-component checks in parallel and returns a
// snapshot suitable for the dashboard. Any individual failure is reported
// as a string in the relevant field rather than failing the whole call,
// because a partially-degraded dashboard is far more useful than a 500.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rep := statusReport{
		Generated: time.Now().UTC(),
		Sandbox:   s.opts.SandboxNamespaces,
		Config:    map[string]string{},
		Agent: agentStatus{
			Namespace:      s.opts.Namespace,
			DeploymentName: s.opts.DeploymentName,
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		dep, err := s.opts.Clientset.AppsV1().Deployments(s.opts.Namespace).Get(ctx, s.opts.DeploymentName, metav1.GetOptions{})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			rep.Agent.DeploymentName = s.opts.DeploymentName + " (error: " + err.Error() + ")"
			return
		}
		if dep.Spec.Replicas != nil {
			rep.Agent.Replicas = *dep.Spec.Replicas
		}
		rep.Agent.ReadyReplicas = dep.Status.ReadyReplicas
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		pods, err := s.opts.Clientset.CoreV1().Pods(s.opts.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + s.opts.DeploymentName,
		})
		if err != nil {
			return
		}
		now := time.Now()
		out := make([]podStatus, 0, len(pods.Items))
		for _, p := range pods.Items {
			ready := false
			var restarts int32
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					ready = true
				}
				restarts += cs.RestartCount
			}
			out = append(out, podStatus{
				Name:     p.Name,
				Phase:    string(p.Status.Phase),
				Ready:    ready,
				Restarts: restarts,
				Age:      humanAge(now.Sub(p.CreationTimestamp.Time)),
			})
		}
		mu.Lock()
		rep.Pods = out
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		cm, err := s.opts.Clientset.CoreV1().ConfigMaps(s.opts.Namespace).Get(ctx, s.opts.ConfigMapName, metav1.GetOptions{})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			rep.Dependencies.ConfigMap = "missing: " + err.Error()
			return
		}
		rep.Dependencies.ConfigMap = "ok"
		// Surface a curated subset of the live config so the dashboard does
		// not leak secrets but still shows what the agent is using.
		for _, key := range []string{
			"OLLAMA_MODEL", "OLLAMA_BASE_URL", "DRY_RUN", "MIN_SEVERITY",
			"NOTIFY_SMTP_HOST", "NOTIFY_SMTP_PORT", "NOTIFY_FROM", "NOTIFY_TO",
			"DEDUP_BACKEND", "ALLOW_PATCH_PROBE", "ALLOW_PATCH_RESOURCES",
			"ALLOW_PATCH_REGISTRY",
		} {
			if v, ok := cm.Data[key]; ok {
				rep.Config[key] = v
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := s.opts.Clientset.CoreV1().Secrets(s.opts.Namespace).Get(ctx, s.opts.SecretName, metav1.GetOptions{})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			rep.Dependencies.Secret = "missing: " + err.Error()
			return
		}
		rep.Dependencies.Secret = "ok"
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		lease, err := s.opts.Clientset.CoordinationV1().Leases(s.opts.Namespace).Get(ctx, "ai-remediator-leader", metav1.GetOptions{})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			rep.Dependencies.Lease = "no leader lease (single-replica or election disabled)"
			return
		}
		holder := ""
		if lease.Spec.HolderIdentity != nil {
			holder = *lease.Spec.HolderIdentity
		}
		rep.Dependencies.Lease = "leader=" + holder
	}()

	if s.opts.OllamaBaseURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := probeOllama(ctx, s.opts.OllamaBaseURL)
			mu.Lock()
			rep.Dependencies.Ollama = res
			mu.Unlock()
		}()
	}

	if strings.EqualFold(s.opts.DedupBackend, "redis") && s.opts.RedisAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := probeRedis(ctx, s.opts.RedisAddr)
			mu.Lock()
			rep.Dependencies.Redis = res
			mu.Unlock()
		}()
	}

	wg.Wait()
	writeJSON(w, http.StatusOK, rep)
}

// probeOllama hits the model registry endpoint and returns a short OK/KO
// summary plus the list of locally-available model names. Useful to
// catch the "model not found" 404 before triggering an actual decision.
func probeOllama(ctx context.Context, baseURL string) *probeResult {
	start := time.Now()
	url := strings.TrimRight(baseURL, "/") + "/tags"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &probeResult{OK: false, Detail: err.Error()}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return &probeResult{OK: false, Detail: err.Error(), LatencyMs: time.Since(start) / time.Millisecond}
	}
	defer resp.Body.Close()
	latency := time.Since(start) / time.Millisecond
	if resp.StatusCode != http.StatusOK {
		return &probeResult{OK: false, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode), LatencyMs: latency}
	}
	var payload struct {
		Models []struct{ Name string } `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return &probeResult{OK: true, Detail: "ok (no model list)", LatencyMs: latency}
	}
	names := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		names = append(names, m.Name)
	}
	if len(names) == 0 {
		return &probeResult{OK: true, Detail: "ok, 0 models installed", LatencyMs: latency}
	}
	return &probeResult{OK: true, Detail: "models: " + strings.Join(names, ", "), LatencyMs: latency}
}

// probeRedis runs a TCP-level connectivity check. We deliberately avoid
// importing the redis client here just for a probe — full PING+AUTH would
// pull the same dependency the agent already has, but keeping the webui
// boundary thin pays off in unit-test isolation.
func probeRedis(ctx context.Context, addr string) *probeResult {
	start := time.Now()
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	latency := time.Since(start) / time.Millisecond
	if err != nil {
		return &probeResult{OK: false, Detail: err.Error(), LatencyMs: latency}
	}
	_ = conn.Close()
	return &probeResult{OK: true, Detail: "tcp connect ok", LatencyMs: latency}
}

// humanAge renders a duration as a compact human string ("3h", "2d").
// Designed for at-a-glance dashboards rather than precision.
func humanAge(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return d.Truncate(time.Minute).String()
	}
	if d < 24*time.Hour {
		return d.Truncate(time.Minute).String()
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if hours == 0 {
		return formatDays(days)
	}
	return formatDays(days) + formatHours(hours)
}

func formatDays(d int) string {
	if d == 1 {
		return "1d"
	}
	return itoa(d) + "d"
}

func formatHours(h int) string {
	return itoa(h) + "h"
}

// itoa avoids pulling strconv into a hot path that only formats small ints.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
