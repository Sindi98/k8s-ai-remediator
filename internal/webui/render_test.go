package webui

import (
	"bytes"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestPagesRenderTheirOwnContent guards against the regression where every
// page rendered the same {{define "content"}} block (the last-alphabetical
// one), because all templates were parsed into a shared namespace.
//
// Each page must contain a string unique to its own template — the page
// title shown in <h1> — and must NOT contain the unique marker of any
// other page.
func TestPagesRenderTheirOwnContent(t *testing.T) {
	s, err := New(Options{
		Addr:           ":0",
		Username:       "u",
		Password:       "p",
		Namespace:      "ai-remediator",
		DeploymentName: "ai-remediator-agent",
		ConfigMapName:  "ai-remediator-config",
		SecretName:     "ai-remediator-secrets",
		Clientset:      fake.NewSimpleClientset(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Each entry maps a template file to a substring that should appear
	// only in that page's rendered HTML.
	cases := map[string]string{
		"dashboard.html": `<h1>Dashboard</h1>`,
		"logs.html":      `<h1>Logs</h1>`,
		"config.html":    `<h1>Configuration</h1>`,
		"scenarios.html": `<h1>Scenarios</h1>`,
		"rbac.html":      `<h1>RBAC onboarding</h1>`,
	}

	for name, marker := range cases {
		t.Run(name, func(t *testing.T) {
			tmpl, ok := s.pages[name]
			if !ok {
				t.Fatalf("no template registered for %s", name)
			}
			var buf bytes.Buffer
			data := pageData{
				Title:      "T",
				Active:     "x",
				Namespace:  "ai-remediator",
				Deployment: "ai-remediator-agent",
			}
			if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
				t.Fatalf("execute %s: %v", name, err)
			}
			out := buf.String()
			if !strings.Contains(out, marker) {
				t.Errorf("%s did not render its own marker %q.\nGot:\n%s", name, marker, out)
			}
			// Make sure another page's marker did not bleed in (the
			// original bug rendered scenarios.html content everywhere).
			for otherName, otherMarker := range cases {
				if otherName == name {
					continue
				}
				if strings.Contains(out, otherMarker) {
					t.Errorf("%s leaked content from %s (found %q)", name, otherName, otherMarker)
				}
			}
		})
	}
}
