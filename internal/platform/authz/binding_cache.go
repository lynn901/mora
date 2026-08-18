package authz

// binding_cache.go implements a revision-keyed cache for the agent effective
// binding set (design-docs/19 §5.4 / Phase 5-2 YS-162). The cache key is
// (agent_id, workspace_id, authz_revision): a binding change (revoke, new
// exclusion, batch upsert) bumps workspace_authz_revisions.revision in the
// same transaction, so the next authz request reads a new revision → new key →
// cache miss → the fresh effective set is loaded. Old entries age out at the
// TTL (≤ 60s per §5.4). The cache only NARROWS the read path — it returns the
// same []AgentBinding the underlying BindingRepo would; the decision pipeline
// is the authoritative gate.
//
// Design notes:
//   - TTL ≤ 60s (§5.4 hard cap). A stale entry under an old revision is never
//     served: the revision is part of the key, so a request that read rev=N
//     never matches an entry cached under rev=N-1.
//   - The revision is read ONCE per Authorize call (Service.Authorize already
//     reads s.revisions.Current). The cache wrapper re-reads the revision
//     too — a single Authorize does at most one revisions.Current (the
//     Service's read) + one cache lookup; the cache re-fetch of revision is
//     avoided by having the Service thread its already-read revision into the
//     cache via RevisionScopedBindings. To keep the BindingRepo interface
//     unchanged (ActiveForAgent has no revision param), the cache is a
//     separate opt-in port; production wiring wraps NewBindingCache over the
//     repo and passes it as the BindingRepo, then the Service uses the cache's
//     ActiveForAgent (which fetches the current revision itself). The extra
//     revision read is one cheap indexed SELECT — acceptable; the savings
//     from skipping the bindings query on a hit dominate.
//   - Concurrency: a sync.Map of entries; each entry is immutable after
//     construction (replaced, not mutated). No lock on read; a mutex guards
//     only the janitor sweep.
//   - No existence leak: a cache miss falls through to the repo (which is
//     the leak-safe path); the cache never stores "not found" as a positive
//     entry.

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// bindingCacheTTL is the max age of a cached effective set (§5.4: ≤ 60s).
const bindingCacheTTL = 60 * time.Second

// bindingCache is a revision-keyed TTL cache over an agent's active
// bindings. It wraps a BindingRepo + RevisionRepo (the same RevisionRepo the
// Service uses for the linearization point). Production wiring passes
// NewBindingCache(repo, revisions) as the Service's BindingRepo.
type bindingCache struct {
	binding   BindingRepo
	revisions RevisionRepo

	// entryKey = agentID|workspaceID|revision (decimal). Immutable value.
	entries  sync.Map // map[string]cacheEntry
	stopOnce sync.Once
	stop     chan struct{}

	// now is overridable in tests; production uses time.Now.
	now func() time.Time
}

type cacheEntry struct {
	bindings []domain.AgentBinding
	expires  time.Time
}

// NewBindingCache wraps a BindingRepo with a revision-keyed TTL cache. The
// returned value implements BindingRepo; pass it to NewService in place of the
// raw repo. The janitor goroutine is started lazily and stops when the process
// exits (or via Stop, for tests).
func NewBindingCache(repo BindingRepo, revisions RevisionRepo) BindingRepo {
	c := &bindingCache{
		binding:   repo,
		revisions: revisions,
		now:       time.Now,
		stop:      make(chan struct{}),
	}
	go c.janitor()
	return c
}

// ActiveForAgent returns the cached effective set for the agent when the
// (agent, workspace, current-revision) key is live; otherwise it loads from the
// underlying repo and caches the result. The revision is part of the key, so a
// bumped revision (same-tx with a binding change, §5.4) always produces a miss
// → the fresh set is loaded → the next request is effective against the new
// effective set.
func (c *bindingCache) ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error) {
	rev, err := c.revisions.Current(ctx, workspaceID)
	if err != nil {
		// Fail open to the repo: the revision read failing must not block the
		// decision (the Service already fails closed on revision-read errors
		// in Authorize; this path is only reached when the cache wraps the
		// repo used by evalBindings/pinnedVersionGate, which tolerate a
		// fallthrough). Fall back to the raw repo without caching.
		return c.binding.ActiveForAgent(ctx, agentID, workspaceID)
	}
	key := cacheKey(agentID, workspaceID, rev)
	if e, ok := c.entries.Load(key); ok {
		ce := e.(cacheEntry)
		if ce.expires.After(c.now()) {
			// Return a copy so a caller mutating its slice can't poison the
			// cached entry (the stored slice is shared across hits).
			out := make([]domain.AgentBinding, len(ce.bindings))
			copy(out, ce.bindings)
			return out, nil
		}
		// Expired: drop and fall through to reload.
		c.entries.Delete(key)
	}
	b, err := c.binding.ActiveForAgent(ctx, agentID, workspaceID)
	if err != nil {
		return nil, err
	}
	// Defensive copy so callers can't mutate the cached slice. The stored
	// entry holds its own copy; each call returns a fresh copy too, so a
	// caller mutating its returned slice never poisons the cache.
	stored := make([]domain.AgentBinding, len(b))
	copy(stored, b)
	c.entries.Store(key, cacheEntry{bindings: stored, expires: c.now().Add(bindingCacheTTL)})
	out := make([]domain.AgentBinding, len(b))
	copy(out, b)
	return out, nil
}

// Invalidate drops a specific (agent, workspace, revision) entry. Not required
// for correctness (revision bump → new key), but exposed for tests that want
// deterministic eviction without waiting for the TTL.
func (c *bindingCache) Invalidate(agentID, workspaceID uuid.UUID, rev int64) {
	c.entries.Delete(cacheKey(agentID, workspaceID, rev))
}

// Stop halts the janitor goroutine (for tests; production lets it die with the
// process). Idempotent.
func (c *bindingCache) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// janitor sweeps expired entries periodically so the map doesn't grow
// unboundedly across revisions. The TTL already bounds staleness; the janitor
// only bounds memory.
func (c *bindingCache) janitor() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.sweepExpired()
		}
	}
}

func (c *bindingCache) sweepExpired() {
	now := c.now()
	c.entries.Range(func(k, v any) bool {
		if v.(cacheEntry).expires.Before(now) {
			c.entries.Delete(k)
		}
		return true
	})
}

func cacheKey(agentID, workspaceID uuid.UUID, rev int64) string {
	return agentID.String() + "|" + workspaceID.String() + "|" + itoa(rev)
}

// itoa is a strconv-free int64→string (avoids pulling strconv for one call;
// keeps the hot path allocation-light). rev is non-negative in practice
// (workspace_authz_revisions starts at 1 and only increments).
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
