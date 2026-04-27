// Package webui implements an admin web GUI for ai-remediator. It runs
// alongside the polling loop and lets operators inspect cluster status,
// stream logs, change configuration, apply scenario fault injections and
// roll out namespace-scoped RBAC without leaving the browser.
//
// The webui never holds its own state: the source of truth for every
// mutating operation is a Kubernetes object (Deployment, ConfigMap,
// Secret, Role/RoleBinding). This keeps the GUI safe to run with multiple
// replicas and survives pod restarts without bookkeeping.
package webui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

//go:embed scenarios/*.yaml
var scenariosFS embed.FS

// Options bundles everything the webui needs to run. Constructed in
// cmd/agent/main.go from AgentConfig + the in-cluster Kubernetes clients.
type Options struct {
	Addr     string
	Username string
	Password string

	// Self refers to the Deployment + namespace that hosts the agent.
	// Used to scale replicas, patch env vars and read pod logs.
	Namespace      string
	DeploymentName string
	ConfigMapName  string
	SecretName     string

	// SandboxNamespaces caps where scenario fault injection is allowed.
	// Empty list disables the feature entirely.
	SandboxNamespaces []string

	// PodLogTailLines bounds the SSE log stream initial buffer.
	PodLogTailLines int64

	Clientset     kubernetes.Interface
	DynamicClient dynamic.Interface
	RESTMapper    *restmapper.DeferredDiscoveryRESTMapper
	RESTConfig    *rest.Config
}

// Server is the HTTP server that exposes the admin GUI.
type Server struct {
	opts  Options
	mux   *http.ServeMux
	pages map[string]*template.Template
}

// pageNames lists the per-page templates the GUI knows how to render.
// Each page file defines its own {{define "content"}}; parsing them all
// into the same namespace would let later definitions clobber earlier
// ones, so we build one template tree per page (each with its own copy
// of "layout") and look up by filename at render time.
var pageNames = []string{
	"dashboard.html",
	"logs.html",
	"config.html",
	"scenarios.html",
	"rbac.html",
}

// pageNamesNoLayout lists templates that render WITHOUT the standard
// nav/header layout (only login.html today). Parsed standalone so they
// don't pull in {{template "layout" .}}.
var pageNamesNoLayout = []string{
	"login.html",
}

// New constructs a ready-to-serve Server.
func New(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.Username) == "" || strings.TrimSpace(opts.Password) == "" {
		return nil, fmt.Errorf("webui: username and password are required")
	}
	if opts.Clientset == nil {
		return nil, fmt.Errorf("webui: kubernetes clientset is required")
	}

	funcs := template.FuncMap{
		"join": strings.Join,
	}

	// Parse the layout once into a base template. Each page then clones
	// the base (so it starts with "layout" already defined) and overlays
	// its own {{define "content"}}. This keeps the layout DRY without the
	// shared-namespace overwrite bug that caused every page to render the
	// last-alphabetical "content" block.
	base, err := template.New("base").Funcs(funcs).ParseFS(templatesFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("webui: parse layout: %w", err)
	}

	pages := make(map[string]*template.Template, len(pageNames)+len(pageNamesNoLayout))
	for _, name := range pageNames {
		t, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("webui: clone base for %s: %w", name, err)
		}
		t, err = t.ParseFS(templatesFS, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("webui: parse %s: %w", name, err)
		}
		pages[name] = t
	}
	// No-layout templates (login) parsed standalone so they don't fight
	// with the navbar layout that assumes an authenticated user.
	for _, name := range pageNamesNoLayout {
		t, err := template.New(name).Funcs(funcs).ParseFS(templatesFS, "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("webui: parse %s: %w", name, err)
		}
		pages[name] = t
	}

	s := &Server{
		opts:  opts,
		mux:   http.NewServeMux(),
		pages: pages,
	}
	s.routes()
	return s, nil
}

// ListenAndServe starts the HTTP server. Returns when ctx is cancelled or
// the listener fails. Caller is responsible for running it in a goroutine.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("webui: listening", "addr", s.opts.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// routes wires the HTTP handlers. Static assets and the login bypass auth;
// every /api/* and / handler is wrapped by basicAuth.
func (s *Server) routes() {
	staticSub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	auth := s.requireAuth

	// Login flow: GET shows the form, POST validates and sets cookie.
	// Both are deliberately OUTSIDE the auth middleware.
	s.mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleLoginPage(w, r)
		case http.MethodPost:
			s.handleLoginSubmit(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	s.mux.HandleFunc("/logout", s.handleLogout)

	s.mux.Handle("/", auth(http.HandlerFunc(s.handleDashboard)))
	s.mux.Handle("/logs", auth(http.HandlerFunc(s.handleLogsPage)))
	s.mux.Handle("/config", auth(http.HandlerFunc(s.handleConfigPage)))
	s.mux.Handle("/scenarios", auth(http.HandlerFunc(s.handleScenariosPage)))
	s.mux.Handle("/rbac", auth(http.HandlerFunc(s.handleRBACPage)))

	s.mux.Handle("/api/status", auth(http.HandlerFunc(s.handleStatus)))
	s.mux.Handle("/api/logs/stream", auth(http.HandlerFunc(s.handleLogsStream)))
	s.mux.Handle("/api/config/llm", auth(http.HandlerFunc(s.handleUpdateLLM)))
	s.mux.Handle("/api/config/mail", auth(http.HandlerFunc(s.handleUpdateMail)))
	s.mux.Handle("/api/config/mail/test", auth(http.HandlerFunc(s.handleTestMail)))
	s.mux.Handle("/api/config/replicas", auth(http.HandlerFunc(s.handleScaleReplicas)))
	s.mux.Handle("/api/scenarios/apply", auth(http.HandlerFunc(s.handleScenarioApply)))
	s.mux.Handle("/api/scenarios/cleanup", auth(http.HandlerFunc(s.handleScenarioCleanup)))
	s.mux.Handle("/api/rbac/apply", auth(http.HandlerFunc(s.handleRBACApply)))

	s.mux.Handle("/api/config/general", auth(http.HandlerFunc(s.handleUpdateGeneral)))
	s.mux.Handle("/api/config/redis-password", auth(http.HandlerFunc(s.handleUpdateRedisPassword)))
}
