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

func TestLoadFromEnv_Defaults(t *testing.T) {
	for _, k := range []string{"OLLAMA_BASE_URL", "OLLAMA_MODEL", "DRY_RUN", "SCALE_MIN", "SCALE_MAX",
		"POLL_INTERVAL_SECONDS", "ALLOW_IMAGE_UPDATES", "IMAGE_UPDATE_CONFIDENCE_THRESHOLD",
		"POD_LOG_TAIL_LINES", "OLLAMA_RPS", "METRICS_ADDR"} {
		os.Unsetenv(k)
	}

	cfg := LoadFromEnv()
	if cfg.Model != "gemma3" {
		t.Errorf("expected default model gemma3, got %s", cfg.Model)
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
}
