package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
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

// generalKeys lists every ConfigMap key the "General" form may persist.
// Restricting writes to this allowlist is what keeps clients from sneaking
// in self-trapping settings (WEBUI_*, AGENT_NAMESPACE, METRICS_ADDR, etc.)
// just by adding extra form fields. Each entry maps the form field name
// (which equals the ConfigMap key) to the kind of validation it needs.
var generalKeys = map[string]configKind{
	"OLLAMA_RPS":                       kindFloat,
	"OLLAMA_MAX_RETRIES":               kindInt,
	"OLLAMA_HTTP_TIMEOUT_SECONDS":      kindInt,
	"POLL_CONTEXT_TIMEOUT_SECONDS":     kindInt,
	"OLLAMA_TLS_SKIP_VERIFY":           kindBool,
	"MIN_SEVERITY":                     kindSeverity,
	"POLL_INTERVAL_SECONDS":            kindInt,
	"MAX_EVENTS_PER_POLL":              kindInt,
	"POD_LOG_TAIL_LINES":               kindInt,
	"DRY_RUN":                          kindBool,
	"SCALE_MIN":                        kindInt,
	"SCALE_MAX":                        kindInt,
	"INCLUDE_NAMESPACES":               kindCSV,
	"EXCLUDE_NAMESPACES":               kindCSV,
	"SCENARIO_SANDBOX_NAMESPACES":      kindCSV,
	"ALLOW_IMAGE_UPDATES":              kindBool,
	"IMAGE_UPDATE_CONFIDENCE_THRESHOLD": kindUnitFloat,
	"ALLOW_PATCH_PROBE":                kindBool,
	"ALLOW_PATCH_RESOURCES":            kindBool,
	"ALLOW_PATCH_REGISTRY":             kindBool,
	"PATCH_CONFIDENCE_THRESHOLD":       kindUnitFloat,
	"DEDUP_BACKEND":                    kindEnum, // "memory" | "redis"
	"DEDUPE_TTL_SECONDS":               kindInt,
	"EVENT_SEEN_TTL_SECONDS":           kindInt,
	"REDIS_ADDR":                       kindString,
	"REDIS_DB":                         kindInt,
	"REDIS_KEY_PREFIX":                 kindString,
}

type configKind int

const (
	kindString configKind = iota
	kindInt
	kindFloat
	kindUnitFloat // float in [0,1]
	kindBool
	kindCSV
	kindSeverity
	kindEnum
)

// boolFieldsAlwaysSent enumerates checkbox-backed fields. HTML forms omit
// unchecked checkboxes entirely, so without this list a saved form would
// never be able to TURN OFF a flag — the absent field would just leave
// the existing ConfigMap value in place. We translate "field missing in
// the POST" into "value=false".
var boolFieldsAlwaysSent = []string{
	"OLLAMA_TLS_SKIP_VERIFY",
	"DRY_RUN",
	"ALLOW_IMAGE_UPDATES",
	"ALLOW_PATCH_PROBE",
	"ALLOW_PATCH_RESOURCES",
	"ALLOW_PATCH_REGISTRY",
}

// handleUpdateGeneral persists the subset of ConfigMap keys listed in
// generalKeys. Any field whose value is an empty string is treated as
// "leave unchanged"; checkbox fields are coerced to "true"/"false" based
// on whether they appear in the form.
func (s *Server) handleUpdateGeneral(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}

	// Determine which boolean fields participate in this submit by looking
	// at a hidden marker that the JS sends. Without the marker we cannot
	// tell "checkbox not on this form" from "checkbox unchecked".
	checkboxesPresent := map[string]bool{}
	for _, name := range r.Form["__bool_fields"] {
		for _, n := range strings.Split(name, ",") {
			n = strings.TrimSpace(n)
			if n != "" {
				checkboxesPresent[n] = true
			}
		}
	}

	updates := map[string]string{}
	for key, kind := range generalKeys {
		raw := r.FormValue(key)
		isBoolField := false
		for _, b := range boolFieldsAlwaysSent {
			if b == key {
				isBoolField = true
				break
			}
		}
		if isBoolField {
			if !checkboxesPresent[key] {
				continue // checkbox not on the submitted form
			}
			if raw == "true" || raw == "on" || raw == "1" {
				updates[key] = "true"
			} else {
				updates[key] = "false"
			}
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if err := validateConfigValue(kind, raw); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Errorf("%s: %w", key, err))
			return
		}
		updates[key] = raw
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"message": "No changes to persist",
		})
		return
	}

	if err := s.patchConfigMap(r.Context(), updates); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bumpDeploymentRollout(r.Context(), "general"); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Updated %d settings; Deployment rollout triggered", len(updates)),
		"updated": keysOf(updates),
	})
}

// handleUpdateRedisPassword stores the Redis password in the Secret. Empty
// password is rejected here (unlike the SMTP form) because there is no
// good reason to "leave it unchanged" via this dedicated endpoint — the
// user explicitly clicked Save on the password row.
func (s *Server) handleUpdateRedisPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	password := r.FormValue("password")
	if password == "" {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"message": "Empty password ignored",
		})
		return
	}
	if err := s.patchSecret(r.Context(), map[string][]byte{
		"REDIS_PASSWORD": []byte(password),
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bumpDeploymentRollout(r.Context(), "redis-password"); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"message": "Redis password saved; Deployment rollout triggered",
	})
}

func validateConfigValue(kind configKind, raw string) error {
	switch kind {
	case kindString:
		return nil
	case kindInt:
		_, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("not an integer: %v", err)
		}
		return nil
	case kindFloat:
		_, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("not a number: %v", err)
		}
		return nil
	case kindUnitFloat:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("not a number: %v", err)
		}
		if f < 0 || f > 1 {
			return fmt.Errorf("must be in [0,1]")
		}
		return nil
	case kindBool:
		switch strings.ToLower(raw) {
		case "true", "false", "1", "0", "yes", "no", "on", "off":
			return nil
		}
		return fmt.Errorf("not a boolean")
	case kindCSV:
		return nil // ParseCSV is lenient by design
	case kindSeverity:
		switch strings.ToLower(raw) {
		case "low", "medium", "high", "critical":
			return nil
		}
		return fmt.Errorf("must be one of low, medium, high, critical")
	case kindEnum:
		switch raw {
		case "memory", "redis":
			return nil
		}
		return fmt.Errorf("invalid backend, expected memory or redis")
	}
	return nil
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
