package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// LimitBucket names a rate-limit category. Read tools share the read bucket;
// write tools share the stricter write bucket (design doc 06 §7.2).
type LimitBucket string

const (
	BucketRead  LimitBucket = "read"
	BucketWrite LimitBucket = "write"
)

// LimitDecision is the outcome of a rate-limit check.
type LimitDecision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// RateLimiter enforces per-token sliding-window limits. Implementations:
//   - MemoryRateLimiter: in-process sliding window (single-replica dev/test).
//   - ValkeyRateLimiter: shared sliding window via Valkey/Redis INCR+EXPIRE,
//     so multiple MCP replicas enforce a consistent limit (design doc 06 §7.2).
type RateLimiter interface {
	// Allow checks and consumes one unit for (tokenID, bucket). When the limit
	// is exceeded it returns Allowed=false with a RetryAfter hint.
	Allow(ctx context.Context, tokenID string, bucket LimitBucket, limit int) (LimitDecision, error)
}

// MemoryRateLimiter implements a sliding-window counter per (tokenID, bucket)
// using a rolling slice of timestamps. The window is 1 minute (req/min, per
// design doc 06 §7.2). It is safe for concurrent use.
type MemoryRateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string][]time.Time // key: tokenID|bucket
}

// NewMemoryRateLimiter returns an in-memory limiter with a 1-minute window.
func NewMemoryRateLimiter() *MemoryRateLimiter {
	return &MemoryRateLimiter{
		window: time.Minute,
		hits:   make(map[string][]time.Time),
	}
}

// Allow implements RateLimiter.
func (m *MemoryRateLimiter) Allow(_ context.Context, tokenID string, bucket LimitBucket, limit int) (LimitDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tokenID + "|" + string(bucket)
	now := time.Now()
	cutoff := now.Add(-m.window)
	hits := m.hits[key]
	// drop expired
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if limit > 0 && len(kept) >= limit {
		// earliest hit determines when a slot frees up.
		earliest := kept[0]
		retry := earliest.Add(m.window).Sub(now)
		if retry < 0 {
			retry = 0
		}
		m.hits[key] = kept
		return LimitDecision{Allowed: false, Remaining: 0, RetryAfter: retry}, nil
	}
	kept = append(kept, now)
	m.hits[key] = kept
	remaining := limit - len(kept)
	if remaining < 0 {
		remaining = 0
	}
	return LimitDecision{Allowed: true, Remaining: remaining}, nil
}

// ValkeyRateLimiter uses a Redis sorted-set sliding window shared across MCP
// replicas. Each request adds a unique member scored by now; members older than
// the window are removed; the count is checked against the limit.
type ValkeyRateLimiter struct {
	client *redis.Client
	window time.Duration
}

// NewValkeyRateLimiter returns a Valkey/Redis-backed limiter.
func NewValkeyRateLimiter(client *redis.Client) *ValkeyRateLimiter {
	return &ValkeyRateLimiter{client: client, window: time.Minute}
}

// Allow implements RateLimiter via a Lua-safe sequence of ZREMRANGEBYSCORE +
// ZCARD + ZADD. To keep this atomic without a Lua script dependency, it uses
// a pipeline; the small race window is acceptable for a soft rate limit and
// matches the design's "sliding window counter (INCR + EXPIRE)" intent.
func (v *ValkeyRateLimiter) Allow(ctx context.Context, tokenID string, bucket LimitBucket, limit int) (LimitDecision, error) {
	key := fmt.Sprintf("mcp:rl:%s:%s", tokenID, bucket)
	now := time.Now()
	nowMicro := float64(now.UnixNano()) / 1e6
	cutoffMicro := float64(now.Add(-v.window).UnixNano()) / 1e6
	member := fmt.Sprintf("%d", now.UnixNano())

	pipe := v.client.Pipeline()
	_ = pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%f", cutoffMicro))
	cardCmd := pipe.ZCard(ctx, key)
	_ = pipe.ZAdd(ctx, key, redis.Z{Score: nowMicro, Member: member})
	_ = pipe.Expire(ctx, key, v.window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return LimitDecision{}, err
	}
	count := int(cardCmd.Val())
	if limit > 0 && count >= limit {
		retry := v.window
		return LimitDecision{Allowed: false, Remaining: 0, RetryAfter: retry}, nil
	}
	remaining := limit - count - 1
	if remaining < 0 {
		remaining = 0
	}
	return LimitDecision{Allowed: true, Remaining: remaining}, nil
}
