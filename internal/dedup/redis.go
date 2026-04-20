package dedup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig holds the knobs needed to connect to a Redis backend.
// Addr is the only required field; the rest have sensible defaults.
type RedisConfig struct {
	Addr      string
	Password  string
	DB        int
	KeyPrefix string
	// DialTimeout bounds the initial PING used to validate the connection.
	// Defaults to 3 seconds.
	DialTimeout time.Duration
	// OpTimeout caps every individual Mark*/IsSignalFresh call. Defaults
	// to 500 ms so that a Redis outage degrades performance but does not
	// block the poll loop.
	OpTimeout time.Duration
}

// RedisStore is a Store backed by a single Redis instance. State survives
// agent pod restarts, which is the whole point of the backend: without it,
// a restart-loop could make the remediator re-apply the same action over
// and over against the same incident.
//
// On any Redis error the store fails open — MarkSeen returns fresh=true
// and IsSignalFresh returns false — so that a Redis outage never silently
// blocks remediation. The trade-off is that during an outage the agent
// behaves like it did before this package existed (potential duplicates).
type RedisStore struct {
	client    *redis.Client
	prefix    string
	opTimeout time.Duration
}

// NewRedisStore connects to Redis, validates reachability with PING, and
// returns a Store ready to use. The returned *RedisStore also exposes
// Close() for graceful shutdown.
func NewRedisStore(cfg RedisConfig) (*RedisStore, error) {
	if cfg.Addr == "" {
		return nil, errors.New("dedup: RedisConfig.Addr is required")
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 3 * time.Second
	}
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 500 * time.Millisecond
	}

	client := redis.NewClient(&redis.Options{
		Addr:        cfg.Addr,
		Password:    cfg.Password,
		DB:          cfg.DB,
		DialTimeout: cfg.DialTimeout,
		ReadTimeout: cfg.OpTimeout,
	})

	pingCtx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("dedup: redis ping failed: %w", err)
	}

	return &RedisStore{
		client:    client,
		prefix:    cfg.KeyPrefix,
		opTimeout: cfg.OpTimeout,
	}, nil
}

// Close releases the underlying connection pool.
func (r *RedisStore) Close() error { return r.client.Close() }

func (r *RedisStore) seenKey(k string) string   { return r.prefix + "seen:" + k }
func (r *RedisStore) signalKey(s string) string { return r.prefix + "signal:" + s }

// MarkSeen uses SET NX EX which is atomic — concurrent agents never both
// observe fresh=true for the same key.
func (r *RedisStore) MarkSeen(key string, _ time.Time, ttl time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.opTimeout)
	defer cancel()

	ok, err := r.client.SetNX(ctx, r.seenKey(key), "1", ttl).Result()
	if err != nil {
		// Fail open: treat as fresh so the event still gets processed.
		// The alternative (fail closed) would silently drop events during
		// a Redis outage.
		slog.Warn("dedup redis MarkSeen failed, failing open", "error", err)
		return true
	}
	return ok
}

// IsSignalFresh ignores ttl: signal keys carry their TTL natively, so
// presence of the key equals freshness.
func (r *RedisStore) IsSignalFresh(signal string, _ time.Time, _ time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), r.opTimeout)
	defer cancel()

	n, err := r.client.Exists(ctx, r.signalKey(signal)).Result()
	if err != nil {
		slog.Warn("dedup redis IsSignalFresh failed, failing open", "error", err)
		return false
	}
	return n > 0
}

// MarkSignal stamps the signal with a TTL equal to the dedup window.
// Value is the timestamp (RFC3339) purely for human inspection via redis-cli.
func (r *RedisStore) MarkSignal(signal string, now time.Time, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), r.opTimeout)
	defer cancel()

	if err := r.client.Set(ctx, r.signalKey(signal), now.Format(time.RFC3339), ttl).Err(); err != nil {
		slog.Warn("dedup redis MarkSignal failed", "error", err)
	}
}

// Evict is a no-op: Redis expires keys natively once their TTL elapses.
func (r *RedisStore) Evict(_ time.Time, _, _ time.Duration) {}

// Compile-time check that RedisStore satisfies Store.
var _ Store = (*RedisStore)(nil)
