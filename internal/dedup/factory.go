package dedup

import (
	"fmt"
	"strings"
)

// BackendConfig is the subset of AgentConfig that the factory needs.
// Kept local to this package to avoid pulling config into dedup.
type BackendConfig struct {
	Backend        string
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string
}

// NewStore picks an implementation based on cfg.Backend. Accepted values:
//   - "" or "memory": in-process MemoryStore (no external dependency)
//   - "redis": Redis-backed RedisStore (state survives agent restarts)
func NewStore(cfg BackendConfig) (Store, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Backend)) {
	case "", "memory":
		return NewMemoryStore(), nil
	case "redis":
		return NewRedisStore(RedisConfig{
			Addr:      cfg.RedisAddr,
			Password:  cfg.RedisPassword,
			DB:        cfg.RedisDB,
			KeyPrefix: cfg.RedisKeyPrefix,
		})
	default:
		return nil, fmt.Errorf("dedup: unknown backend %q (want memory|redis)", cfg.Backend)
	}
}
