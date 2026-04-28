package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// pageData carries the common fields every HTML template needs.
type pageData struct {
	Title             string
	Active            string
	Namespace         string
	Deployment        string
	SandboxNamespaces []string
	Scenarios         []scenarioMeta
	Error             string
	Message           string
}

func (s *Server) renderPage(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, ok := s.pages[name]
	if !ok {
		slog.Error("webui: unknown template", "name", name)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("webui: render template", "name", name, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.renderPage(w, "dashboard.html", pageData{
		Title:      "Dashboard",
		Active:     "dashboard",
		Namespace:  s.opts.Namespace,
		Deployment: s.opts.DeploymentName,
	})
}

func (s *Server) handleLogsPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "logs.html", pageData{
		Title:      "Logs",
		Active:     "logs",
		Namespace:  s.opts.Namespace,
		Deployment: s.opts.DeploymentName,
	})
}

func (s *Server) handleConfigPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "config.html", pageData{
		Title:      "Configuration",
		Active:     "config",
		Namespace:  s.opts.Namespace,
		Deployment: s.opts.DeploymentName,
	})
}

func (s *Server) handleScenariosPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "scenarios.html", pageData{
		Title:             "Scenarios",
		Active:            "scenarios",
		Namespace:         s.opts.Namespace,
		Deployment:        s.opts.DeploymentName,
		SandboxNamespaces: s.opts.SandboxNamespaces,
		Scenarios:         s.listScenarios(),
	})
}

func (s *Server) handleRBACPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "rbac.html", pageData{
		Title:      "RBAC",
		Active:     "rbac",
		Namespace:  s.opts.Namespace,
		Deployment: s.opts.DeploymentName,
	})
}

func (s *Server) handleClusterPage(w http.ResponseWriter, _ *http.Request) {
	s.renderPage(w, "cluster.html", pageData{
		Title:      "Cluster",
		Active:     "cluster",
		Namespace:  s.opts.Namespace,
		Deployment: s.opts.DeploymentName,
	})
}

// writeJSON serialises v as JSON with the given status code. Slimmer than
// pulling a router library, kept inline so handler code stays linear.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// formValueTrim is a small helper around r.FormValue that trims whitespace.
func formValueTrim(r *http.Request, key string) string {
	return strings.TrimSpace(r.FormValue(key))
}
