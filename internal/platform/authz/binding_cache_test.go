package authz

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRevisionRepoBumpable is a RevisionRepo whose Current can be advanced
// mid-test to simulate the same-tx revision bump a binding revoke/exclusion
// change performs (§5.4). It also counts Current calls so the cache test can
// assert the repo was skipped on a hit and re-read on a miss.
type fakeRevisionRepoBumpable struct {
	rev   atomic.Int64
	calls atomic.Int64
}

func (f *fakeRevisionRepoBumpable) Current(_ context.Context, _ uuid.UUID) (int64, error) {
	f.calls.Add(1)
	return f.rev.Load(), nil
}

func (f *fakeRevisionRepoBumpable) Bump() { f.rev.Add(1) }

// countingBindingRepo is a BindingRepo that counts ActiveForAgent calls so
// the cache test can assert the underlying repo was skipped on a hit and
// re-read on a miss (revision bump → new key → miss).
type countingBindingRepo struct {
	binds map[uuid.UUID][]domain.AgentBinding
	calls atomic.Int64
}

func (c *countingBindingRepo) ActiveForAgent(_ context.Context, agentID, _ uuid.UUID) ([]domain.AgentBinding, error) {
	c.calls.Add(1)
	return c.binds[agentID], nil
}

// Test_BindingCache_HitSkipsRepo (§5.4): under the same revision, the
// resolved effective set is cached — ActiveForAgent on the underlying repo
// is called once; the second ActiveForAgent (via the cache) skips the repo.
func Test_BindingCache_HitSkipsRepo(t *testing.T) {
	ws, agent := uuid.New(), uuid.New()
	asset := uuid.New()
	revisions := &fakeRevisionRepoBumpable{}
	revisions.rev.Store(1)
	repo := &countingBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliveryTool}},
	}}
	cache := NewBindingCache(repo, revisions)
	defer cache.(*bindingCache).Stop()

	first, err := cache.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	require.Len(t, first, 1)

	// Mutate the returned slice — the cache must hold its own copy (§5.4:
	// callers can't mutate the cached entry).
	first[0].Effect = domain.BindingDeny

	second, err := cache.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	assert.Equal(t, domain.BindingAllow, second[0].Effect,
		"cache must return a copy; caller mutation must not poison the cache")
	assert.Equal(t, int64(1), repo.calls.Load(),
		"underlying repo called once (cache hit on 2nd call)")
}

// Test_BindingCache_RevisionBumpInvalidates (§5.4, DoD "缓存按 revision 失效
// 生效"): a binding revoke/exclusion change bumps the workspace revision in
// the same tx. The next ActiveForAgent reads a new revision → new cache key
// → miss → the fresh effective set is loaded. The OLD entry under the prior
// revision is never served. This is the "撤销 use → 下一次请求拒绝（缓存按
// revision 失效生效）" invariant at the cache layer.
func Test_BindingCache_RevisionBumpInvalidates(t *testing.T) {
	ws, agent := uuid.New(), uuid.New()
	asset := uuid.New()
	revisions := &fakeRevisionRepoBumpable{}
	revisions.rev.Store(1)
	// Initial effective set: one allow binding.
	repo := &countingBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliveryTool}},
	}}
	cache := NewBindingCache(repo, revisions)
	defer cache.(*bindingCache).Stop()

	first, err := cache.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	assert.Len(t, first, 1)
	assert.Equal(t, int64(1), repo.calls.Load())

	// Simulate the same-tx effect of a binding revoke / exclusion change:
	// the workspace revision bumps, and the effective set changes (the
	// binding is gone / an explicit deny now covers the asset).
	revisions.Bump()
	repo.binds[agent] = nil // revoke removed the only allow → no allow covers
	// (A real revoke sets revoked_at; the repo's ActiveForAgent would no
	// longer return it. nil models that the effective set is now empty.)

	second, err := cache.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	assert.Empty(t, second, "after the revision bump the fresh (now-empty) set must be served")
	assert.Equal(t, int64(2), repo.calls.Load(),
		"new revision → cache miss → repo re-read")

	// The old revision's entry is NOT served on a read under the old revision
	// either, because the revision advanced: a stale caller reading rev=1
	// would still hit, but the linearization point (Service.Authorize) always
	// reads Current, which is now 2. So no request ever observes the stale set
	// under the bumped revision.
	assert.Equal(t, int64(2), revisions.rev.Load())
}

// Test_BindingCache_TTLExpiry (§5.4: TTL ≤ 60s): an expired entry is dropped
// on read and the repo is re-read. Guards against unbounded staleness if a
// revision somehow did not bump (belt-and-suspenders; the revision key is
// the primary invalidation signal).
func Test_BindingCache_TTLExpiry(t *testing.T) {
	ws, agent := uuid.New(), uuid.New()
	asset := uuid.New()
	revisions := &fakeRevisionRepoBumpable{}
	revisions.rev.Store(1)
	repo := &countingBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow}},
	}}
	c := &bindingCache{
		binding:   repo,
		revisions: revisions,
		now:       time.Now,
		stop:      make(chan struct{}),
	}
	// Inject a controllable clock.
	frozen := time.Now()
	c.now = func() time.Time { return frozen }
	defer close(c.stop)

	_, err := c.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	assert.Equal(t, int64(1), repo.calls.Load())

	// Advance the clock past the TTL.
	frozen = frozen.Add(bindingCacheTTL + time.Second)

	_, err = c.ActiveForAgent(context.Background(), agent, ws)
	require.NoError(t, err)
	assert.Equal(t, int64(2), repo.calls.Load(),
		"an entry past its TTL must miss and re-read")
}
