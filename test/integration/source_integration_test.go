//go:build integration

// Phase 1 source-module end-to-end tests (design-docs/14 §4.2/§4.4).
// DATABASE_URL-gated; skipped when unset. These verify the ACs that span the
// postgres repos + the transactional SyncRunSink:
//   - AC-4 (workspace isolation): a source in wsA is invisible to wsB's List.
//   - Idempotency-Key: same payload → returns the original run; different
//     payload under a reused key → ErrIdempotencyConflict (§4.4).
//   - ETag optimistic concurrency: a PATCH with a stale If-Match → conflict.
package integration

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	srcsvc "github.com/lynn901/mora/internal/module/knowledge/source/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// SourceSuite groups the Phase 1 source-module integration tests.
type SourceSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB

	wsRepo  *postgres.WorkspaceRepo
	srcRepo *postgres.SourceRepo
	runRepo *postgres.SyncRunRepo
	runSink *postgres.SourceSyncSink
}

func TestSourceSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(SourceSuite))
}

func (s *SourceSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.wsRepo = postgres.NewWorkspaceRepo(s.db)
	s.srcRepo = postgres.NewSourceRepo(s.db)
	s.runRepo = postgres.NewSyncRunRepo(s.db)
	s.runSink = postgres.NewSourceSyncSink(pool, outbox.NewStore())
}

func (s *SourceSuite) TearDownSuite() { s.pool.Close() }

func (s *SourceSuite) SetupTest() {
	ctx := context.Background()
	// clean in dependency order (children before parents)
	for _, t := range []string{
		"asset_projections", "review_decisions", "review_requests",
		"knowledge_source_targets", "source_sync_runs", "knowledge_sources",
		"outbox_events",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	// workspaces + users are shared with the main Suite; clean them too so a
	// source's workspace_id FK is satisfiable without collisions.
	for _, t := range []string{"workspaces", "users"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM " + t)
	}
}

// seedWS inserts a workspace + creator user and returns their IDs.
func (s *SourceSuite) seedWS(slug, userEmail string) (wsID, userID domain.UUID) {
	ctx := context.Background()
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		userEmail, userEmail).Scan(&userID))
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, userID).Scan(&wsID))
	return wsID, userID
}

// TestAC4_SourceWorkspaceIsolation asserts a source in wsA does not appear in
// wsB's List (AC-4: multi-workspace isolation; no cross-workspace leak).
func (s *SourceSuite) TestAC4_SourceWorkspaceIsolation() {
	ctx := context.Background()
	wsA, userA := s.seedWS("iso-a", "a@x.com")
	wsB, userB := s.seedWS("iso-b", "b@x.com")

	srcA := &domain.KnowledgeSource{
		WorkspaceID: wsA, SourceType: domain.SourceGit, Name: "repo-a",
		URINormalized: "https://github.com/a/repo.git",
		TrustLevel: domain.TrustUntrusted,
		CreatedByType: domain.SubjectUser, CreatedByID: userA,
	}
	require.NoError(s.T(), s.srcRepo.Create(ctx, srcA))

	srcB := &domain.KnowledgeSource{
		WorkspaceID: wsB, SourceType: domain.SourceGit, Name: "repo-b",
		URINormalized: "https://github.com/b/repo.git",
		TrustLevel: domain.TrustUntrusted,
		CreatedByType: domain.SubjectUser, CreatedByID: userB,
	}
	require.NoError(s.T(), s.srcRepo.Create(ctx, srcB))

	aItems, _, err := s.srcRepo.List(ctx, srcsvc.SourceListQuery{WorkspaceID: wsA, PageSize: 50})
	require.NoError(s.T(), err)
	require.Len(s.T(), aItems, 1)
	assert.Equal(s.T(), srcA.ID, aItems[0].ID, "wsA list must contain only wsA's source")

	bItems, _, err := s.srcRepo.List(ctx, srcsvc.SourceListQuery{WorkspaceID: wsB, PageSize: 50})
	require.NoError(s.T(), err)
	require.Len(s.T(), bItems, 1)
	assert.Equal(s.T(), srcB.ID, bItems[0].ID, "wsB list must contain only wsB's source")
}

// TestSource_ETagConflict asserts a PATCH with a stale ETag returns
// ErrSourceConflict (§4.4 If-Match optimistic concurrency).
func (s *SourceSuite) TestSource_ETagConflict() {
	ctx := context.Background()
	wsID, userID := s.seedWS("etag", "etag@x.com")
	src := &domain.KnowledgeSource{
		WorkspaceID: wsID, SourceType: domain.SourceGit, Name: "etag-src",
		URINormalized: "https://github.com/c/repo.git",
		TrustLevel: domain.TrustUntrusted,
		CreatedByType: domain.SubjectUser, CreatedByID: userID,
	}
	require.NoError(s.T(), s.srcRepo.Create(ctx, src))

	// A stale ETag (off by one ms) must conflict, not silently overwrite.
	stale := src.ETagVersion - 1
	name := "renamed"
	_, err := s.srcRepo.Update(ctx, src.ID, stale, srcsvc.SourcePatch{Name: &name})
	assert.ErrorIs(s.T(), err, srcsvc.ErrSourceConflict)

	// The correct ETag succeeds and bumps the version.
	updated, err := s.srcRepo.Update(ctx, src.ID, src.ETagVersion, srcsvc.SourcePatch{Name: &name})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "renamed", updated.Name)
	assert.Greater(s.T(), updated.ETagVersion, src.ETagVersion)
}

// TestSyncRun_IdempotencyKey_SamePayloadReturnsOriginal asserts a duplicate
// Idempotency-Key for the SAME (source_id, requested_revision) surfaces as
// ErrIdempotentRetry and the original run is returned by GetByIdempotencyKey
// (§4.4 idempotent retry).
func (s *SourceSuite) TestSyncRun_IdempotencyKey_SamePayloadReturnsOriginal() {
	ctx := context.Background()
	wsID, userID := s.seedWS("idem", "idem@x.com")
	src := &domain.KnowledgeSource{
		WorkspaceID: wsID, SourceType: domain.SourceGit, Name: "idem-src",
		URINormalized: "https://github.com/d/repo.git",
		TrustLevel: domain.TrustInternal,
		CreatedByType: domain.SubjectUser, CreatedByID: userID,
	}
	require.NoError(s.T(), s.srcRepo.Create(ctx, src))

	mkRun := func(rev string) *domain.SourceSyncRun {
		return &domain.SourceSyncRun{
			SourceID: src.ID, RequestedByType: domain.SubjectUser, RequestedByID: userID,
			RequestedRevision: rev, RequestedAssetType: domain.RequestedAssetCodebase,
			IdempotencyKey: "dup-key-1", Status: domain.SyncRunQueued,
			SourceConfigSnapshot: map[string]any{"source_type": "git"},
		}
	}
	ev := domain.KnowledgeEvent{
		EventID: "dup-key-1", EventType: domain.KEAssetVersionRequested,
		EventVersion: 1, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: uuid.New(), WorkspaceID: &wsID,
		Actor: domain.EventActor{Type: domain.SubjectUser, ID: userID},
	}

	// First insert succeeds.
	run1 := mkRun("abc123")
	require.NoError(s.T(), s.runSink.CreateRun(ctx, run1, ev))

	// Second insert, SAME payload → ErrIdempotentRetry.
	run2 := mkRun("abc123")
	err := s.runSink.CreateRun(ctx, run2, ev)
	assert.ErrorIs(s.T(), err, srcsvc.ErrIdempotentRetry)

	// The original run is retrievable by key.
	orig, err := s.runRepo.GetByIdempotencyKey(ctx, "dup-key-1")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), run1.ID, orig.ID)
}

// TestSyncRun_IdempotencyKey_DifferentPayloadConflicts asserts a duplicate
// Idempotency-Key for a DIFFERENT (source_id, requested_revision) surfaces as
// ErrIdempotencyConflict (§4.4 → 409).
func (s *SourceSuite) TestSyncRun_IdempotencyKey_DifferentPayloadConflicts() {
	ctx := context.Background()
	wsID, userID := s.seedWS("conf", "conf@x.com")
	src := &domain.KnowledgeSource{
		WorkspaceID: wsID, SourceType: domain.SourceGit, Name: "conf-src",
		URINormalized: "https://github.com/e/repo.git",
		TrustLevel: domain.TrustInternal,
		CreatedByType: domain.SubjectUser, CreatedByID: userID,
	}
	require.NoError(s.T(), s.srcRepo.Create(ctx, src))

	ev := domain.KnowledgeEvent{
		EventID: "dup-key-2", EventType: domain.KEAssetVersionRequested,
		EventVersion: 1, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: uuid.New(), WorkspaceID: &wsID,
		Actor: domain.EventActor{Type: domain.SubjectUser, ID: userID},
	}
	mkRun := func(rev string) *domain.SourceSyncRun {
		return &domain.SourceSyncRun{
			SourceID: src.ID, RequestedByType: domain.SubjectUser, RequestedByID: userID,
			RequestedRevision: rev, RequestedAssetType: domain.RequestedAssetCodebase,
			IdempotencyKey: "dup-key-2", Status: domain.SyncRunQueued,
			SourceConfigSnapshot: map[string]any{"source_type": "git"},
		}
	}
	require.NoError(s.T(), s.runSink.CreateRun(ctx, mkRun("rev-1"), ev))
	err := s.runSink.CreateRun(ctx, mkRun("rev-2"), ev) // different revision
	assert.ErrorIs(s.T(), err, srcsvc.ErrIdempotencyConflict)
}
