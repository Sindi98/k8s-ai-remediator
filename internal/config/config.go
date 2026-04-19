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
	// OllamaHTTPTimeoutSec caps the per-request HTTP timeout toward Ollama.
	// Local LLMs can take > 90s on the first call, so the default is 180s.
	OllamaHTTPTimeoutSec int
	// PollContextTimeoutSec bounds the entire poll cycle (listing events
	// plus all downstream Ollama calls for that iteration). Must be larger
	// than OllamaHTTPTimeoutSec, otherwise the context expires mid-request
	// and produces "context deadline exceeded" before the HTTP client can
	// fail with its own timeout. Defaults to 300s.
	PollContextTimeoutSec int
	MetricsAddr          string
	LeaderElection       bool
	LeaseName            string
	LeaseNamespace       string
	MinSeverity          string
	// DedupeTTLSec suppresses repeated decisions for the same
	// (namespace, kind, name, reason) signal within the given window.
	// Prevents event storms (e.g. flaky readiness probes) from saturating Ollama.
	DedupeTTLSec int
	// MaxEventsPerPoll caps how many Warning events trigger an Ollama
	// call per poll cycle; excess events are skipped and picked up next poll.
	MaxEventsPerPoll int

	// AllowPatchProbe enables the patch_probe action that tunes probe
	// timing fields on a Deployment. Opt-in required also via the
	// "ai-remediator/allow-patch: probe" annotation on the target Deployment.
	AllowPatchProbe bool
	// AllowPatchResources enables the patch_resources action.
	// Requires the "resources" entry in the allow-patch annotation.
	AllowPatchResources bool
	// AllowPatchRegistry enables the patch_registry action that rewrites
	// the registry prefix of container images.
	// Requires the "registry" entry in the allow-patch annotation.
	AllowPatchRegistry bool
	// PatchConfidenceThreshold gates the three patch_* actions on the
	// LLM confidence field. Defaults to 0.85.
	PatchConfidenceThreshold float64

	// IncludeNamespaces, when non-empty, restricts the agent to events
	// originating from one of the listed namespaces. ExcludeNamespaces
	// always wins: even an included namespace is skipped if also listed
	// in ExcludeNamespaces. Defaults to empty (all namespaces allowed).
	IncludeNamespaces []string
	// ExcludeNamespaces drops events from the listed namespaces. Defaults
	// to the standard system namespaces so that the agent never reacts to
	// CoreDNS, kube-scheduler, local-path-provisioner and friends.
	ExcludeNamespaces []string

	// NotifySMTPHost, Port, User, Password, From, To configure the SMTP
	// notifier that emails a short report after every executeDecision.
	// If host/user/to are empty the notifier is disabled (no-op).
	NotifySMTPHost     string
	NotifySMTPPort     int
	NotifySMTPUser     string
	NotifySMTPPassword string
	NotifyFrom         string
	NotifyTo           string
	// NotifyMinSeverity filters which decisions trigger an email.
	// Uses the same severity vocabulary as MinSeverity. Defaults to "medium".
	NotifyMinSeverity string
}

// LoadFromEnv reads agent configuration from environment variables.
func LoadFromEnv() AgentConfig {
	return AgentConfig{
		BaseURL:              Getenv("OLLAMA_BASE_URL", "http://ollama.ollama.svc.cluster.local:11434/api"),
		Model:                Getenv("OLLAMA_MODEL", "qwen2.5:14b"),
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
		OllamaHTTPTimeoutSec:  Getint("OLLAMA_HTTP_TIMEOUT_SECONDS", 180),
		PollContextTimeoutSec: Getint("POLL_CONTEXT_TIMEOUT_SECONDS", 300),
		MetricsAddr:          Getenv("METRICS_ADDR", ":9090"),
		LeaderElection:       Getbool("LEADER_ELECTION", false),
		LeaseName:            Getenv("LEASE_NAME", "ai-remediator-leader"),
		LeaseNamespace:       Getenv("LEASE_NAMESPACE", "ai-remediator"),
		MinSeverity:              Getenv("MIN_SEVERITY", "medium"),
		DedupeTTLSec:             Getint("DEDUPE_TTL_SECONDS", 300),
		MaxEventsPerPoll:         Getint("MAX_EVENTS_PER_POLL", 10),
		AllowPatchProbe:          Getbool("ALLOW_PATCH_PROBE", false),
		AllowPatchResources:      Getbool("ALLOW_PATCH_RESOURCES", false),
		AllowPatchRegistry:       Getbool("ALLOW_PATCH_REGISTRY", false),
		PatchConfidenceThreshold: Getfloat("PATCH_CONFIDENCE_THRESHOLD", 0.85),
		IncludeNamespaces:        ParseCSV(Getenv("INCLUDE_NAMESPACES", "")),
		ExcludeNamespaces:        ParseCSV(Getenv("EXCLUDE_NAMESPACES", "kube-system,kube-public,kube-node-lease,local-path-storage")),
		NotifySMTPHost:           Getenv("NOTIFY_SMTP_HOST", ""),
		NotifySMTPPort:           Getint("NOTIFY_SMTP_PORT", 587),
		NotifySMTPUser:           Getenv("NOTIFY_SMTP_USER", ""),
		NotifySMTPPassword:       Getenv("NOTIFY_SMTP_PASSWORD", ""),
		NotifyFrom:               Getenv("NOTIFY_FROM", ""),
		NotifyTo:                 Getenv("NOTIFY_TO", ""),
		NotifyMinSeverity:        Getenv("NOTIFY_MIN_SEVERITY", "medium"),
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

// ParseCSV trims whitespace, drops empty entries and returns nil for an
// empty input. Useful for namespace lists where "ns1, ns2 ,,ns3" should
// become {"ns1", "ns2", "ns3"}.
func ParseCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
