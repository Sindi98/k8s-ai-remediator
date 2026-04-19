package config

import (
	"os"
	"testing"
)

func TestGetenv(t *testing.T) {
	os.Setenv("TEST_GETENV_KEY", "val")
	defer os.Unsetenv("TEST_GETENV_KEY")

	if got := Getenv("TEST_GETENV_KEY", "def"); got != "val" {
		t.Errorf("expected val, got %s", got)
	}
	if got := Getenv("TEST_GETENV_MISSING", "def"); got != "def" {
		t.Errorf("expected def, got %s", got)
	}
}

func TestGetbool(t *testing.T) {
	tests := []struct {
		val  string
		def  bool
		want bool
	}{
		{"true", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"false", true, false},
		{"0", true, false},
		{"", true, true},
		{"", false, false},
	}
	for _, tt := range tests {
		os.Setenv("TEST_BOOL", tt.val)
		if got := Getbool("TEST_BOOL", tt.def); got != tt.want {
			t.Errorf("Getbool(%q, %v) = %v, want %v", tt.val, tt.def, got, tt.want)
		}
	}
	os.Unsetenv("TEST_BOOL")
}

func TestGetint(t *testing.T) {
	os.Setenv("TEST_INT", "42")
	defer os.Unsetenv("TEST_INT")

	if got := Getint("TEST_INT", 0); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := Getint("TEST_INT_MISSING", 99); got != 99 {
		t.Errorf("expected 99, got %d", got)
	}

	os.Setenv("TEST_INT", "not_a_number")
	if got := Getint("TEST_INT", 7); got != 7 {
		t.Errorf("expected 7 on parse error, got %d", got)
	}
}

func TestGetfloat(t *testing.T) {
	os.Setenv("TEST_FLOAT", "0.95")
	defer os.Unsetenv("TEST_FLOAT")

	if got := Getfloat("TEST_FLOAT", 0.0); got != 0.95 {
		t.Errorf("expected 0.95, got %f", got)
	}
	if got := Getfloat("TEST_FLOAT_MISSING", 0.5); got != 0.5 {
		t.Errorf("expected 0.5, got %f", got)
	}
}

func TestParseCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,, ,", nil},
		{"ns1", []string{"ns1"}},
		{"ns1,ns2,ns3", []string{"ns1", "ns2", "ns3"}},
		{" ns1 , ns2 ,, ns3 ", []string{"ns1", "ns2", "ns3"}},
	}
	for _, c := range cases {
		got := ParseCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("ParseCSV(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("ParseCSV(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestLoadFromEnv_NamespaceDefaults(t *testing.T) {
	for _, k := range []string{"INCLUDE_NAMESPACES", "EXCLUDE_NAMESPACES"} {
		os.Unsetenv(k)
	}
	cfg := LoadFromEnv()
	if len(cfg.IncludeNamespaces) != 0 {
		t.Errorf("expected empty include by default, got %v", cfg.IncludeNamespaces)
	}
	expected := map[string]bool{
		"kube-system":         true,
		"kube-public":         true,
		"kube-node-lease":     true,
		"local-path-storage":  true,
	}
	if len(cfg.ExcludeNamespaces) != len(expected) {
		t.Errorf("expected %d default exclude namespaces, got %v", len(expected), cfg.ExcludeNamespaces)
	}
	for _, ns := range cfg.ExcludeNamespaces {
		if !expected[ns] {
			t.Errorf("unexpected default exclude namespace %q", ns)
		}
	}
}

func TestLoadFromEnv_NamespaceOverride(t *testing.T) {
	os.Setenv("INCLUDE_NAMESPACES", "incident-lab,prod-app")
	os.Setenv("EXCLUDE_NAMESPACES", "noisy")
	defer os.Unsetenv("INCLUDE_NAMESPACES")
	defer os.Unsetenv("EXCLUDE_NAMESPACES")

	cfg := LoadFromEnv()
	if len(cfg.IncludeNamespaces) != 2 || cfg.IncludeNamespaces[0] != "incident-lab" {
		t.Errorf("unexpected include: %v", cfg.IncludeNamespaces)
	}
	if len(cfg.ExcludeNamespaces) != 1 || cfg.ExcludeNamespaces[0] != "noisy" {
		t.Errorf("unexpected exclude: %v", cfg.ExcludeNamespaces)
	}
}

func TestLoadFromEnv_AutoCorrectsTimeoutInvariant(t *testing.T) {
	// Poll context must be > Ollama HTTP timeout; misconfig should be
	// auto-corrected at startup with a warning.
	os.Setenv("OLLAMA_HTTP_TIMEOUT_SECONDS", "300")
	os.Setenv("POLL_CONTEXT_TIMEOUT_SECONDS", "120")
	defer os.Unsetenv("OLLAMA_HTTP_TIMEOUT_SECONDS")
	defer os.Unsetenv("POLL_CONTEXT_TIMEOUT_SECONDS")

	cfg := LoadFromEnv()
	if cfg.PollContextTimeoutSec <= cfg.OllamaHTTPTimeoutSec {
		t.Errorf("expected PollContextTimeoutSec > OllamaHTTPTimeoutSec, got %d vs %d",
			cfg.PollContextTimeoutSec, cfg.OllamaHTTPTimeoutSec)
	}
	if cfg.PollContextTimeoutSec != 360 { // 300 + 60
		t.Errorf("expected auto-correction to 360s, got %d", cfg.PollContextTimeoutSec)
	}
}

func TestLoadFromEnv_Defaults(t *testing.T) {
	for _, k := range []string{"OLLAMA_BASE_URL", "OLLAMA_MODEL", "DRY_RUN", "SCALE_MIN", "SCALE_MAX",
		"POLL_INTERVAL_SECONDS", "ALLOW_IMAGE_UPDATES", "IMAGE_UPDATE_CONFIDENCE_THRESHOLD",
		"POD_LOG_TAIL_LINES", "OLLAMA_RPS", "OLLAMA_MAX_RETRIES", "OLLAMA_TLS_SKIP_VERIFY",
		"METRICS_ADDR", "LEADER_ELECTION", "LEASE_NAME", "LEASE_NAMESPACE",
		"DEDUPE_TTL_SECONDS", "MAX_EVENTS_PER_POLL"} {
		os.Unsetenv(k)
	}

	cfg := LoadFromEnv()
	if cfg.Model != "qwen2.5:14b" {
		t.Errorf("expected default model qwen2.5:14b, got %s", cfg.Model)
	}
	if cfg.DryRun {
		t.Error("expected default dryRun=false")
	}
	if cfg.MinScale != 1 || cfg.MaxScale != 5 {
		t.Errorf("expected scale bounds 1-5, got %d-%d", cfg.MinScale, cfg.MaxScale)
	}
	if cfg.PollSec != 30 {
		t.Errorf("expected poll 30s, got %d", cfg.PollSec)
	}
	if cfg.ImageUpdateThreshold != 0.92 {
		t.Errorf("expected threshold 0.92, got %f", cfg.ImageUpdateThreshold)
	}
	if cfg.OllamaRPS != 2.0 {
		t.Errorf("expected ollamaRPS 2.0, got %f", cfg.OllamaRPS)
	}
	if cfg.MetricsAddr != ":9090" {
		t.Errorf("expected metricsAddr :9090, got %s", cfg.MetricsAddr)
	}
	if cfg.OllamaMaxRetries != 3 {
		t.Errorf("expected ollamaMaxRetries 3, got %d", cfg.OllamaMaxRetries)
	}
	if cfg.OllamaTLSSkipVerify {
		t.Error("expected ollamaTLSSkipVerify=false")
	}
	if cfg.LeaderElection {
		t.Error("expected leaderElection=false")
	}
	if cfg.LeaseName != "ai-remediator-leader" {
		t.Errorf("expected leaseName ai-remediator-leader, got %s", cfg.LeaseName)
	}
	if cfg.LeaseNamespace != "ai-remediator" {
		t.Errorf("expected leaseNamespace ai-remediator, got %s", cfg.LeaseNamespace)
	}
	if cfg.DedupeTTLSec != 300 {
		t.Errorf("expected dedupeTTLSec 300, got %d", cfg.DedupeTTLSec)
	}
	if cfg.MaxEventsPerPoll != 10 {
		t.Errorf("expected maxEventsPerPoll 10, got %d", cfg.MaxEventsPerPoll)
	}
}
