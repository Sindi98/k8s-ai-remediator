package webui

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dns1123 matches the strict subset of valid Kubernetes namespace names so
// the GUI cannot be coerced into creating "../../etc" objects.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// remediatorRoleRules mirrors the namespace-scoped Role in
// deploy/rbac-namespaced.yaml. Kept inline (rather than parsing the YAML)
// so the GUI never silently goes out of sync with the YAML — adding a
// rule must be a deliberate code change reviewed alongside the manifest.
var remediatorRoleRules = []rbacv1.PolicyRule{
	{APIGroups: []string{""}, Resources: []string{"pods", "pods/log", "events"}, Verbs: []string{"get", "list", "watch", "delete"}},
	{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list"}},
	{APIGroups: []string{"apps"}, Resources: []string{"deployments", "replicasets"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
}

// handleRBACApply onboards a target namespace by creating (or updating)
// the namespace, the Role and the RoleBinding required by the agent's
// ServiceAccount. The agent SA itself lives in s.opts.Namespace.
func (s *Server) handleRBACApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	ns := strings.ToLower(formValueTrim(r, "namespace"))
	if !dns1123.MatchString(ns) {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("invalid namespace name %q", ns))
		return
	}
	createNS := r.FormValue("create_namespace") == "true"

	if createNS {
		if err := s.ensureNamespace(r.Context(), ns); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := s.ensureRole(r.Context(), ns); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.ensureRoleBinding(r.Context(), ns); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":    "ok",
		"namespace": ns,
		"message":   "Role and RoleBinding applied; the agent can now manage workloads in this namespace",
	})
}

func (s *Server) ensureNamespace(ctx context.Context, name string) error {
	_, err := s.opts.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = s.opts.Clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"ai-remediator/managed": "true",
			},
		},
	}, metav1.CreateOptions{})
	return err
}

func (s *Server) ensureRole(ctx context.Context, ns string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ai-remediator",
			Namespace: ns,
			Labels: map[string]string{
				"ai-remediator/managed": "true",
			},
		},
		Rules: remediatorRoleRules,
	}
	existing, err := s.opts.Clientset.RbacV1().Roles(ns).Get(ctx, role.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.opts.Clientset.RbacV1().Roles(ns).Create(ctx, role, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	existing.Rules = role.Rules
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels["ai-remediator/managed"] = "true"
	_, err = s.opts.Clientset.RbacV1().Roles(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *Server) ensureRoleBinding(ctx context.Context, ns string) error {
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ai-remediator",
			Namespace: ns,
			Labels: map[string]string{
				"ai-remediator/managed": "true",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "ai-remediator",
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: "ai-remediator", Namespace: s.opts.Namespace},
		},
	}
	existing, err := s.opts.Clientset.RbacV1().RoleBindings(ns).Get(ctx, rb.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = s.opts.Clientset.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	// RoleRef is immutable: if it already points elsewhere the only safe
	// option is to delete and recreate. In practice that should never
	// happen for a binding we manage.
	if existing.RoleRef != rb.RoleRef {
		if err := s.opts.Clientset.RbacV1().RoleBindings(ns).Delete(ctx, rb.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		_, err = s.opts.Clientset.RbacV1().RoleBindings(ns).Create(ctx, rb, metav1.CreateOptions{})
		return err
	}
	existing.Subjects = rb.Subjects
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels["ai-remediator/managed"] = "true"
	_, err = s.opts.Clientset.RbacV1().RoleBindings(ns).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}
