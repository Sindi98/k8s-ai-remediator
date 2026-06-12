package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		image, host, repo, tag string
	}{
		{"busybox:broken", "registry-1.docker.io", "library/busybox", "broken"},
		{"busybox", "registry-1.docker.io", "library/busybox", ""},
		{"myorg/app:v1", "registry-1.docker.io", "myorg/app", "v1"},
		{"docker.io/library/nginx:1.25", "registry-1.docker.io", "library/nginx", "1.25"},
		{"host.docker.internal:5050/busybox:1.36", "host.docker.internal:5050", "busybox", "1.36"},
		{"ghcr.io/owner/app:tag", "ghcr.io", "owner/app", "tag"},
		{"localhost/app", "localhost", "app", ""},
		{"nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000", "registry-1.docker.io", "library/nginx", ""},
	}
	for _, c := range cases {
		host, repo, tag, err := ParseRef(c.image)
		if err != nil {
			t.Errorf("ParseRef(%q) err=%v", c.image, err)
			continue
		}
		if host != c.host || repo != c.repo || tag != c.tag {
			t.Errorf("ParseRef(%q)=(%q,%q,%q) want (%q,%q,%q)", c.image, host, repo, tag, c.host, c.repo, c.tag)
		}
	}
	if _, _, _, err := ParseRef(""); err == nil {
		t.Error("expected error for empty image")
	}
}

func TestPickNewest(t *testing.T) {
	cases := []struct {
		tags    []string
		exclude string
		want    string
	}{
		{[]string{"1.35", "1.36", "latest", "broken"}, "broken", "1.36"},
		// Numeric, not lexical: 1.10 > 1.9.
		{[]string{"1.9", "1.10"}, "", "1.10"},
		// v-prefix and deeper versions.
		{[]string{"v2.0.9", "v2.1"}, "", "v2.1"},
		// A bare release outranks a suffixed variant of the same version.
		{[]string{"1.36-musl", "1.36"}, "", "1.36"},
		// No version-like tags → latest.
		{[]string{"latest", "stable"}, "broken", "latest"},
		// Only the broken tag → nothing usable.
		{[]string{"broken"}, "broken", ""},
		// The broken tag itself is never picked even if version-like.
		{[]string{"1.36"}, "1.36", ""},
		{nil, "x", ""},
	}
	for _, c := range cases {
		if got := PickNewest(c.tags, c.exclude); got != c.want {
			t.Errorf("PickNewest(%v, %q)=%q want %q", c.tags, c.exclude, got, c.want)
		}
	}
}

func TestNewestTag_PlainRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/busybox/tags/list" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "busybox",
			"tags": []string{"1.35", "1.36", "latest"},
		})
	}))
	defer srv.Close()

	c := &Client{Schemes: []string{"http"}}
	host := strings.TrimPrefix(srv.URL, "http://")
	tag, err := c.NewestTag(context.Background(), host+"/busybox:this-tag-does-not-exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "1.36" {
		t.Errorf("expected 1.36, got %q", tag)
	}
}

func TestNewestTag_BearerAuthDance(t *testing.T) {
	// Simulates the anonymous Docker Hub flow: 401 + WWW-Authenticate
	// pointing at a token endpoint, then the authorized retry.
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.URL.Query().Get("scope") != "repository:library/busybox:pull" {
				t.Errorf("unexpected scope %q", r.URL.Query().Get("scope"))
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon-token"})
		case "/v2/library/busybox/tags/list":
			if r.Header.Get("Authorization") != "Bearer anon-token" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+srvURL+`/token",service="registry.test",scope="repository:library/busybox:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"1.34", "1.36.1", "latest"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	c := &Client{Schemes: []string{"http"}}
	host := strings.TrimPrefix(srv.URL, "http://")
	tag, err := c.NewestTag(context.Background(), host+"/library/busybox:broken")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag != "1.36.1" {
		t.Errorf("expected 1.36.1, got %q", tag)
	}
}

func TestNewestTag_NoCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"broken"}})
	}))
	defer srv.Close()

	c := &Client{Schemes: []string{"http"}}
	host := strings.TrimPrefix(srv.URL, "http://")
	if _, err := c.NewestTag(context.Background(), host+"/repo:broken"); err == nil {
		t.Error("expected an error when no usable tag exists")
	}
}

func TestNewestTag_RegistryDown(t *testing.T) {
	c := &Client{Schemes: []string{"http"}}
	if _, err := c.NewestTag(context.Background(), "127.0.0.1:1/repo:broken"); err == nil {
		t.Error("expected an error when the registry is unreachable")
	}
}
