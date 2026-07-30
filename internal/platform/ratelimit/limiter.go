package ratelimit

// Package ratelimit implements a simple sliding-window counter rate limiter
// keyed by user/token. Used by middleware to enforce per-domain limits
// (04-api-contract.md §16). Backed by an in-memory map; in production this
// would use Valkey for cross-replica consistency.

import (
	"sync"
	"time"
)

// Limiter is a per-key sliding-window rate limiter.
type Limiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	hits   map[string][]time.Time
}

func New(limitPerMin int) *Limiter {
	return &Limiter{window: time.Minute, limit: limitPerMin, hits: map[string][]time.Time{}}
}

// Allow reports whether key is within the limit, advancing the window.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	hits := l.hits[key]
	// drop expired
	keep := hits[:0]
	for _, h := range hits {
		if h.After(cutoff) {
			keep = append(keep, h)
		}
	}
	if len(keep) >= l.limit {
		l.hits[key] = keep
		return false
	}
	l.hits[key] = append(keep, now)
	return true
}
