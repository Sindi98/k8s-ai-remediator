package webui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
)

// scenarioMeta describes one available fault-injection scenario as listed
// on the Scenarios page.
type scenarioMeta struct {
	Name        string
	Filename    string
	Description string
}

// listScenarios reads the embedded scenarios FS and parses the leading
// comment block of each YAML as a human description.
func (s *Server) listScenarios() []scenarioMeta {
	entries, err := fs.ReadDir(scenariosFS, "scenarios")
	if err != nil {
		return nil
	}
	out := make([]scenarioMeta, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := scenariosFS.ReadFile("scenarios/" + e.Name())
		if err != nil {
			continue
		}
		out = append(out, scenarioMeta{
			Name:        strings.TrimSuffix(e.Name(), ".yaml"),
			Filename:    e.Name(),
			Description: extractLeadingComment(raw),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// extractLeadingComment returns the contiguous block of "# ..." lines at
// the top of the YAML, stripped of the leading "# " prefix.
func extractLeadingComment(raw []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	var out []string
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "#") {
			break
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ")))
	}
	return strings.Join(out, "\n")
}

// handleScenarioApply parses the requested scenario YAML and creates each
// document via the dynamic client. The target namespace must be present
// in the sandbox allowlist; this is the single load-bearing safety check
// preventing the GUI from breaking arbitrary workloads.
func (s *Server) handleScenarioApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	name := formValueTrim(r, "scenario")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("scenario is required"))
		return
	}
	objs, err := s.loadScenario(name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.assertSandbox(objs); err != nil {
		writeJSONError(w, http.StatusForbidden, err)
		return
	}

	applied := make([]string, 0, len(objs))
	for _, obj := range objs {
		if err := s.applyUnstructured(r.Context(), obj); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err))
			return
		}
		applied = append(applied, fmt.Sprintf("%s/%s in %s", obj.GetKind(), obj.GetName(), obj.GetNamespace()))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"applied": applied,
	})
}

// handleScenarioCleanup deletes the resources defined in the scenario YAML
// using the same name+namespace mapping as apply.
func (s *Server) handleScenarioCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	name := formValueTrim(r, "scenario")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Errorf("scenario is required"))
		return
	}
	objs, err := s.loadScenario(name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.assertSandbox(objs); err != nil {
		writeJSONError(w, http.StatusForbidden, err)
		return
	}

	deleted := make([]string, 0, len(objs))
	for _, obj := range objs {
		if err := s.deleteUnstructured(r.Context(), obj); err != nil && !apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusInternalServerError, fmt.Errorf("delete %s/%s: %w", obj.GetKind(), obj.GetName(), err))
			return
		}
		deleted = append(deleted, fmt.Sprintf("%s/%s in %s", obj.GetKind(), obj.GetName(), obj.GetNamespace()))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"deleted": deleted,
	})
}

// loadScenario reads and parses the named scenario into a slice of
// unstructured objects. Multi-document YAML is supported via the standard
// "---" separator.
func (s *Server) loadScenario(name string) ([]*unstructured.Unstructured, error) {
	if strings.ContainsAny(name, "/.\\") {
		return nil, fmt.Errorf("invalid scenario name")
	}
	raw, err := scenariosFS.ReadFile("scenarios/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("scenario %q not found", name)
	}
	return decodeYAMLDocs(raw)
}

func decodeYAMLDocs(raw []byte) ([]*unstructured.Unstructured, error) {
	dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err.Error() == "EOF" {
				break
			}
			// Standard decoders signal end of stream with io.EOF; the
			// version in apimachinery wraps it. Compare by string to
			// avoid a hard dep on the underlying error type.
			if strings.Contains(err.Error(), "EOF") {
				break
			}
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{Object: m}
		out = append(out, obj)
	}
	return out, nil
}

// assertSandbox rejects scenarios whose objects target a namespace outside
// the configured sandbox allowlist. Cluster-scoped objects (no namespace)
// are also rejected, since the GUI never authorises cluster-wide changes
// from the scenarios feature.
func (s *Server) assertSandbox(objs []*unstructured.Unstructured) error {
	if len(s.opts.SandboxNamespaces) == 0 {
		return fmt.Errorf("scenarios disabled (SCENARIO_SANDBOX_NAMESPACES is empty)")
	}
	allowed := map[string]bool{}
	for _, n := range s.opts.SandboxNamespaces {
		allowed[n] = true
	}
	for _, obj := range objs {
		ns := obj.GetNamespace()
		if ns == "" {
			return fmt.Errorf("cluster-scoped object %s/%s rejected: scenarios must be namespaced", obj.GetKind(), obj.GetName())
		}
		if !allowed[ns] {
			return fmt.Errorf("namespace %q is not in the sandbox allowlist %v", ns, s.opts.SandboxNamespaces)
		}
	}
	return nil
}

// applyUnstructured creates or updates a single object via server-side
// apply on the dynamic client. The "ai-remediator-webui" field manager
// claims ownership so subsequent applies are idempotent.
func (s *Server) applyUnstructured(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := s.gvrFor(obj)
	if err != nil {
		return err
	}
	body, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	_, err = s.opts.DynamicClient.Resource(gvr).Namespace(obj.GetNamespace()).
		Patch(ctx, obj.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{
			FieldManager: "ai-remediator-webui",
			Force:        boolPtr(true),
		})
	return err
}

func (s *Server) deleteUnstructured(ctx context.Context, obj *unstructured.Unstructured) error {
	gvr, err := s.gvrFor(obj)
	if err != nil {
		return err
	}
	return s.opts.DynamicClient.Resource(gvr).Namespace(obj.GetNamespace()).
		Delete(ctx, obj.GetName(), metav1.DeleteOptions{})
}

// gvrFor resolves the GroupVersionResource for an unstructured object via
// the discovery-backed RESTMapper. The mapper is cached and refreshes on
// "no match" errors, so newly-installed CRDs are picked up without a
// pod restart.
func (s *Server) gvrFor(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := s.opts.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			s.opts.RESTMapper.Reset()
			mapping, err = s.opts.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		}
		if err != nil {
			return schema.GroupVersionResource{}, err
		}
	}
	return mapping.Resource, nil
}

func boolPtr(b bool) *bool { return &b }
