package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// handleUpdateLLM persists the chosen Ollama model into the agent
// ConfigMap and forces a Deployment rollout so the new value is read by a
// fresh pod. We update the ConfigMap (single source of truth) and bump an
// annotation on the Deployment template; Kubernetes does the rest.
func (s *Server) handleUpdateLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	model := formValueTrim(r, "model")
	if model == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("model is required"))
		return
	}
	baseURL := formValueTrim(r, "base_url")

	updates := map[string]string{"OLLAMA_MODEL": model}
	if baseURL != "" {
		updates["OLLAMA_BASE_URL"] = baseURL
	}

	if err := s.patchConfigMap(r.Context(), updates); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bumpDeploymentRollout(r.Context(), "llm"); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "ConfigMap updated; Deployment rollout triggered",
	})
}

// handleUpdateMail writes SMTP fields to the ConfigMap (host/port/from/to/user)
// and, when a non-empty password is provided, to the Secret. Empty password
// is treated as "leave unchanged" so editing the form doesn't accidentally
// blank out a working credential.
func (s *Server) handleUpdateMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	cmUpdates := map[string]string{}
	for _, f := range []struct{ form, key string }{
		{"host", "NOTIFY_SMTP_HOST"},
		{"port", "NOTIFY_SMTP_PORT"},
		{"user", "NOTIFY_SMTP_USER"},
		{"from", "NOTIFY_FROM"},
		{"to", "NOTIFY_TO"},
		{"min_severity", "NOTIFY_MIN_SEVERITY"},
	} {
		if v := formValueTrim(r, f.form); v != "" {
			cmUpdates[f.key] = v
		}
	}
	if len(cmUpdates) > 0 {
		if err := s.patchConfigMap(r.Context(), cmUpdates); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if password := r.FormValue("password"); password != "" {
		if err := s.patchSecret(r.Context(), map[string][]byte{
			"NOTIFY_SMTP_PASSWORD": []byte(password),
		}); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := s.bumpDeploymentRollout(r.Context(), "mail"); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "Mail config saved; Deployment rollout triggered",
	})
}

// handleTestMail sends a one-shot SMTP message using the credentials
// currently stored in ConfigMap+Secret. Useful to validate a freshly-saved
// configuration without waiting for a real remediation event.
func (s *Server) handleTestMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cm, err := s.opts.Clientset.CoreV1().ConfigMaps(s.opts.Namespace).Get(r.Context(), s.opts.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	sec, err := s.opts.Clientset.CoreV1().Secrets(s.opts.Namespace).Get(r.Context(), s.opts.SecretName, metav1.GetOptions{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}

	host := cm.Data["NOTIFY_SMTP_HOST"]
	user := cm.Data["NOTIFY_SMTP_USER"]
	from := cm.Data["NOTIFY_FROM"]
	to := cm.Data["NOTIFY_TO"]
	port := cm.Data["NOTIFY_SMTP_PORT"]
	if port == "" {
		port = "587"
	}
	password := string(sec.Data["NOTIFY_SMTP_PASSWORD"])

	if host == "" || user == "" || to == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("SMTP configuration incomplete (need host, user, to)"))
		return
	}
	if from == "" {
		from = user
	}

	addr := host + ":" + port
	body := "Subject: ai-remediator SMTP test\r\n" +
		"From: " + from + "\r\n" +
		"To: " + to + "\r\n" +
		"\r\n" +
		"Test message sent from the ai-remediator admin GUI at " +
		time.Now().UTC().Format(time.RFC3339) + ".\r\n"

	auth := smtp.PlainAuth("", user, password, host)
	if err := smtp.SendMail(addr, auth, from, []string{to}, []byte(body)); err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "Test message accepted by " + addr,
	})
}

// handleScaleReplicas updates the replica count on the agent Deployment.
// Leader election (already wired in cmd/agent) prevents two replicas from
// processing the same event, so any value >= 1 is safe.
func (s *Server) handleScaleReplicas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	rawReplicas := formValueTrim(r, "replicas")
	n, err := strconv.Atoi(rawReplicas)
	if err != nil || n < 0 || n > 10 {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("replicas must be an integer in [0,10]"))
		return
	}

	scale, err := s.opts.Clientset.AppsV1().Deployments(s.opts.Namespace).GetScale(r.Context(), s.opts.DeploymentName, metav1.GetOptions{})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	scale.Spec.Replicas = int32(n)
	if _, err := s.opts.Clientset.AppsV1().Deployments(s.opts.Namespace).UpdateScale(r.Context(), s.opts.DeploymentName, scale, metav1.UpdateOptions{}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"replicas": n,
	})
}

// patchConfigMap merges updates into the ConfigMap data, retrying on
// resourceVersion conflicts so kubectl-edit running concurrently can't
// silently lose our changes.
func (s *Server) patchConfigMap(ctx context.Context, updates map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.opts.Clientset.CoreV1().ConfigMaps(s.opts.Namespace).Get(ctx, s.opts.ConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      s.opts.ConfigMapName,
					Namespace: s.opts.Namespace,
				},
				Data: map[string]string{},
			}
			for k, v := range updates {
				cm.Data[k] = v
			}
			_, err := s.opts.Clientset.CoreV1().ConfigMaps(s.opts.Namespace).Create(ctx, cm, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		for k, v := range updates {
			cm.Data[k] = v
		}
		_, err = s.opts.Clientset.CoreV1().ConfigMaps(s.opts.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

// patchSecret merges updates into the Secret data the same way patchConfigMap
// does for non-secret config. Values are stored as raw bytes; the Secret type
// remains Opaque.
func (s *Server) patchSecret(ctx context.Context, updates map[string][]byte) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sec, err := s.opts.Clientset.CoreV1().Secrets(s.opts.Namespace).Get(ctx, s.opts.SecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			sec = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      s.opts.SecretName,
					Namespace: s.opts.Namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{},
			}
			for k, v := range updates {
				sec.Data[k] = v
			}
			_, err := s.opts.Clientset.CoreV1().Secrets(s.opts.Namespace).Create(ctx, sec, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		for k, v := range updates {
			sec.Data[k] = v
		}
		_, err = s.opts.Clientset.CoreV1().Secrets(s.opts.Namespace).Update(ctx, sec, metav1.UpdateOptions{})
		return err
	})
}

// bumpDeploymentRollout patches an annotation on the Deployment pod template
// so that a new ReplicaSet is created and pods pick up the fresh ConfigMap
// or Secret on startup. Strategic merge patch is used so we never need to
// hold the full Deployment spec.
func (s *Server) bumpDeploymentRollout(ctx context.Context, reason string) error {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{
						"webui/config-revision": time.Now().UTC().Format(time.RFC3339Nano),
						"webui/config-reason":   reason,
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = s.opts.Clientset.AppsV1().Deployments(s.opts.Namespace).Patch(ctx, s.opts.DeploymentName, types.StrategicMergePatchType, body, metav1.PatchOptions{})
	return err
}
