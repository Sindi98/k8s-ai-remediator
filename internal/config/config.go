package config

import (
	"os"
	"strconv"
	"strings"
)

// AgentConfig holds all configuration values for the remediation agent.
type AgentConfig struct {
	BaseURL              string
	Model                string
	DryRun               bool
	MinScale             int32
	MaxScale             int32
	PollSec              int
	AllowImageUpdates    bool
	ImageUpdateThreshold float64
	PodLogTailLines      int64
	OllamaRPS            float64
	OllamaMaxRetries     int
	OllamaTLSSkipVerify  bool
	MetricsAddr          string
	LeaderElection       bool
	LeaseName            string
	LeaseNamespace       string
	MinSeverity          string
}

// LoadFromEnv reads agent configuration from environment variables.
func LoadFromEnv() AgentConfig {
	return AgentConfig{
		BaseURL:              Getenv("OLLAMA_BASE_URL", "http://ollama.ollama.svc.cluster.local:11434/api"),
		Model:                Getenv("OLLAMA_MODEL", "gemma3"),
		DryRun:               Getbool("DRY_RUN", false),
		MinScale:             int32(Getint("SCALE_MIN", 1)),
		MaxScale:             int32(Getint("SCALE_MAX", 5)),
		PollSec:              Getint("POLL_INTERVAL_SECONDS", 30),
		AllowImageUpdates:    Getbool("ALLOW_IMAGE_UPDATES", false),
		ImageUpdateThreshold: Getfloat("IMAGE_UPDATE_CONFIDENCE_THRESHOLD", 0.92),
		PodLogTailLines:      int64(Getint("POD_LOG_TAIL_LINES", 200)),
		OllamaRPS:            Getfloat("OLLAMA_RPS", 2.0),
		OllamaMaxRetries:     Getint("OLLAMA_MAX_RETRIES", 3),
		OllamaTLSSkipVerify:  Getbool("OLLAMA_TLS_SKIP_VERIFY", false),
		MetricsAddr:          Getenv("METRICS_ADDR", ":9090"),
		LeaderElection:       Getbool("LEADER_ELECTION", false),
		LeaseName:            Getenv("LEASE_NAME", "ai-remediator-leader"),
		LeaseNamespace:       Getenv("LEASE_NAMESPACE", "ai-remediator"),
		MinSeverity:          Getenv("MIN_SEVERITY", "medium"),
	}
}

func Getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func Getbool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

func Getint(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func Getfloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
