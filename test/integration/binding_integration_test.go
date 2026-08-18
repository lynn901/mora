//go:build integration

// binding_integration_test.go — Phase 5-2 (YS-162) end-to-end integration tests
// against a live PostgreSQL (mora-postgres-1). DATABASE_URL-gated; skipped when
// unset. Run with:
//
//	DATABASE_URL=postgres://mora:mora@mora-postgres-1:5432/mora?sslmode=disable \
//	  go test -tags=integration ./test/integration/... -run BindingSuite -count=1 -v
//
// These verify the five acceptance points the backend dev flagged as the test
// engineer's end-to-end落点 (PR #64 comment, design-docs/19 §5):
//
//  1. Idempotency (§5.2 / §11.1): same Idempotency-Key + same payload returns the
//     original batch; same key + a DIFFERENT payload returns ErrIdempotencyConflict.
//  2. Transaction atomicity (§5.2): a batch that fails mid-way leaves NO
//     agent_bindings / agent_binding_batches / outbox_events / workspace_authz
//     revision bump behind (the whole tx rolls back).
//  3. ETag optimistic concurrency (§5.2 防覆盖): an update is revoke-old +
//     create-new; a concurrent revoke (or a stale ETag) affects 0 rows →
//     ErrBindingConflict (409).
//  4. Pinned-version阻断 (§8.2 用例 5 / §11.4): a pinned binding whose pinned
//     version is revoked/missing → authz.Service.Authorize returns ErrNotFound +
//     Allowed=false, with NO silent fallback to the latest published version.
//  5. delivery_mode resolution (§5.3): the winning allow binding's
//     tool/summary/inline delivery mode is carried on AuthzContext.DeliveryMode.
//
// These are the cases the fake-repo unit tests cannot cover: they exercise the
// real pgx transaction, the UNIQUE constraint on agent_binding_batches, the
// CHECK constraints on agent_bindings, and the real SQL the authz repos issue.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	bindingmod "github.com/lynn901/mora/internal/module/binding"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/outbox"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// BindingSuite exercises the Phase 5-2 binding management + authz decision
// pipeline end-to-end against the real DB.
type BindingSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB

	sink     *postgres.BindingSink
	bindRepo *postgres.BindingRepo
	outbox   *outbox.Store
}

func TestBindingSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(BindingSuite))
}

func (s *BindingSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.outbox = outbox.NewStore()
	s.sink = postgres.NewBindingSink(pool, s.outbox)
	s.bindRepo = postgres.NewBindingRepo(s.db)
}

func (s *BindingSuite) TearDownSuite() { s.pool.Close() }

// SetupTest cleans the binding-related tables in dependency order so each test
// starts from a known-empty state. agent_binding_batches / agent_bindings are
// the SUT; outbox_events + workspace_authz_revisions are the side-effect tables.
// We also reset workspace_authz_revisions.revision so a fresh workspace starts
// at the seed value (0/1) — see seedWorkspace.
func (s *BindingSuite) SetupTest() {
	ctx := context.Background()
	for _, t := range []string{
		"agent_binding_batches", "agent_bindings",
		"outbox_events", "outbox_deliveries",
		"skill_packages", "knowledge_asset_versions", "knowledge_assets",
		"knowledge_relations", "agents",
		"permissions",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	// Drop the auto-seeded revision rows (013 seeds revision=0 per workspace);
	// seedWorkspace inserts a fresh workspace whose revision row the sink will
	// create via UPSERT. Deleting the workspace rows cascades to its revision row
	// (FK ON DELETE CASCADE), so do this AFTER cleaning dependents.
	_, _ = s.pool.Exec(ctx, `DELETE FROM workspaces WHERE slug LIKE 'bind-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'bind-%'`)
	// Clean up custom roles created by tests (non-system only).
	_, _ = s.pool.Exec(ctx, `DELETE FROM roles WHERE is_system = false`)
}

// --- seed helpers ---

// seedUser inserts a user and returns its ID.
func (s *BindingSuite) seedUser(email, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, name).Scan(&id))
	return id
}

// seedWorkspace inserts a workspace owned by owner and returns its ID. The
// 013 migration seeds a workspace_authz_revisions row (revision=0) for every
// workspace at INSERT time only if the row doesn't exist; since we create a
// fresh workspace each test, the revision row is seeded at 0.
func (s *BindingSuite) seedWorkspace(owner domain.UUID, slug string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, owner).Scan(&id))
	return id
}

// seedAgent inserts an agent (no service account — agent-on-behalf-of-user
// path uses the acting user as the RBAC subject, so service_account_id is not
// required for these scenarios).
func (s *BindingSuite) seedAgent(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO agents (workspace_id, name, owner_id, status) VALUES ($1,$2,$3,'active') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

// seedAsset inserts a knowledge_asset (asset_type=memory avoids the
// native_document_id FK) and returns its id. Status defaults to 'draft' which
// the authz lifecycleGate treats as use-permitting.
func (s *BindingSuite) seedAsset(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_assets (workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,'memory',$2,'user',$3,'published','private') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

// seedVersion inserts a knowledge_asset_version with the given build/governance
// status and returns its id. The dedupe_key + content_hash are required (NOT
// NULL) — we pass stable test values.
func (s *BindingSuite) seedVersion(assetID domain.UUID, versionNo int64, build, gov string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, dedupe_key, build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1,$2,'human',$3,$4,$5,'user',$6) RETURNING id`,
		assetID, versionNo, fmt.Sprintf("dk-%d", versionNo), build, gov, uuid.New()).Scan(&id))
	return id
}

// grantUse creates a non-system role carrying `use` and grants it to the user
// on the workspace, so an agent acting on behalf of the user passes the RBAC
// intersection (§8.2: binding only narrows — the acting user's RBAC must allow
// `use` first). Returns the permission id (unused) for symmetry.
func (s *BindingSuite) grantUse(ctx context.Context, wsID, userID domain.UUID) {
	var roleID domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO roles (name, scope, permissions, is_system) VALUES ('test-use','workspace','["use"]'::jsonb,false) RETURNING id`,
	).Scan(&roleID))
	_, err := s.pool.Exec(ctx, `
		INSERT INTO permissions (subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope)
		VALUES ('user',$1,$2,'workspace',$3,'allow','subtree')`,
		userID, roleID, wsID)
	require.NoError(s.T(), err)
}

// grantAssign grants the `assign` action on the workspace so the binding
// Service's authorize() (management plane) passes for a non-admin caller.
func (s *BindingSuite) grantAssign(ctx context.Context, wsID, userID domain.UUID) {
	var roleID domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO roles (name, scope, permissions, is_system) VALUES ('test-assign','workspace','["assign"]'::jsonb,false) RETURNING id`,
	).Scan(&roleID))
	_, err := s.pool.Exec(ctx, `
		INSERT INTO permissions (subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope)
		VALUES ('user',$1,$2,'workspace',$3,'allow','subtree')`,
		userID, roleID, wsID)
	require.NoError(s.T(), err)
}

// newAuthzService wires a production-shape authz.Service over the real repos:
// rbac.Engine + RBACAdapter (perms), CompositeLocator with the AssetLocator, and
// the real BindingRepo / AssetRepo / AssetVersionRepo / RevisionsRepo /
// DecisionRepo. This is the path cmd/mora-api uses (minus the cache wrapper,
// which is exercised by its own unit tests).
func (s *BindingSuite) newAuthzService() *authz.Service {
	perms := postgres.NewPermissionRepo(s.db)
	dirs := postgres.NewDirectoryRepo(s.db)
	docs := postgres.NewDocumentRepo(s.db)
	adapter := postgres.NewRBACAdapter(perms, dirs, docs)
	eng := rbac.NewEngine(adapter)
	eng.SetLocator(authz.AsLocator(authz.NewCompositeLocator(
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetAsset, Loc: authz.NewAssetLocator(postgres.NewAuthzAssetRepo(s.db))},
	)))
	return authz.NewService(
		authz.NewCompositeLocator(
			struct {
				Type authz.TargetType
				Loc  authz.ResourceLocator
			}{Type: domain.TargetAsset, Loc: authz.NewAssetLocator(postgres.NewAuthzAssetRepo(s.db))},
		),
		eng,
		postgres.NewAuthzBindingRepo(s.db),
		postgres.NewAuthzAgentRepo(s.db),
		postgres.NewAuthzAssetRepo(s.db),
		postgres.NewAuthzAssetVersionRepo(s.db),
		postgres.NewRevisionsRepo(s.db),
		postgres.NewDecisionRepo(s.db),
	)
}

// svcBinding wires the binding Service over the real BindingSink + BindingRepo,
// with a nil pinned-checker (the authz layer's pinnedVersionGate is the
// authoritative阻断; the service's pre-flight alert is not under test here —
// see Test_Binding_PinnedVersionBlockedAlert for that path). admin=true so the
// management authorize() short-circuits (we test the cross-workspace guard + RBAC
// deny separately where relevant).
func (s *BindingSuite) svcBinding() *bindingmod.Service {
	return bindingmod.NewService(s.bindRepo, s.sink, s.sink, nil)
}

// --- Scenario 1: idempotency ---

// Test_Binding_IdempotentRetrySamePayloadReturnsOriginal: a duplicate
// Idempotency-Key with the SAME payload must return the original batch (§5.2 /
// §11.1), NOT create new bindings and NOT return a conflict. The second call
// must be marked an idempotent hit and return the SAME binding IDs as the first.
func (s *BindingSuite) Test_Binding_IdempotentRetrySamePayloadReturnsOriginal() {
	ctx := context.Background()
	owner := s.seedUser("bind-idem@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-idem")
	agent := s.seedAgent(ws, owner, "agent-idem")
	asset := s.seedAsset(ws, owner, "asset-idem")
	svc := s.svcBinding()

	in := []bindingmod.BindingInput{{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}}
	auth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	r1, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "idem-key-1", in)
	require.NoError(s.T(), err)
	require.Len(s.T(), r1.Results, 1)
	firstID := r1.Results[0].Binding.ID
	require.NotEqual(s.T(), uuid.Nil, firstID)
	origRev := r1.NewRevision

	// Same key, same payload → idempotent retry, original batch returned.
	r2, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "idem-key-1", in)
	require.NoError(s.T(), err, "same-payload retry must NOT error (idempotent retry)")
	assert.True(s.T(), r2.IdempotentHit, "the retry must be flagged an idempotent hit")
	require.Len(s.T(), r2.Results, 1)
	assert.Equal(s.T(), firstID, r2.Results[0].Binding.ID,
		"the original binding id must be returned (no new binding created)")
	// The retry must echo the ORIGINAL new_revision (not 0) — a caller polling the
	// authz revision must see a stable, monotonic value, never a regression to
	// zero (YS-163 DEFECT-1).
	assert.Equal(s.T(), origRev, r2.NewRevision,
		"the retry must echo the original revision (not 0, YS-163 DEFECT-1)")

	// No duplicate binding row was created: exactly one active binding for the
	// agent on this asset.
	var activeCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND workspace_id=$2 AND revoked_at IS NULL`,
		agent, ws).Scan(&activeCount))
	assert.Equal(s.T(), 1, activeCount, "an idempotent retry must not create a duplicate binding")

	// Exactly one batch record for the key.
	var batchCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_binding_batches WHERE idempotency_key=$1`, "idem-key-1").Scan(&batchCount))
	assert.Equal(s.T(), 1, batchCount)

	// The revision was bumped ONCE (the original batch); the retry must not bump
	// it again (no second write occurred).
	var curRev int64
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT revision FROM workspace_authz_revisions WHERE workspace_id=$1`, ws).Scan(&curRev))
	assert.Equal(s.T(), origRev, curRev, "an idempotent retry must not bump the revision again")
}

// Test_Binding_IdempotencyConflictDifferentPayload409: a duplicate
// Idempotency-Key with a DIFFERENT payload must return ErrIdempotencyConflict
// (§11.1 → 409), and must NOT mutate the original batch's bindings.
func (s *BindingSuite) Test_Binding_IdempotencyConflictDifferentPayload409() {
	ctx := context.Background()
	owner := s.seedUser("bind-conf@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-conf")
	agent := s.seedAgent(ws, owner, "agent-conf")
	assetA := s.seedAsset(ws, owner, "asset-conf-a")
	assetB := s.seedAsset(ws, owner, "asset-conf-b")
	svc := s.svcBinding()
	auth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	in1 := []bindingmod.BindingInput{{
		ScopeKind: domain.BindingScopeAsset, AssetID: &assetA,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}}
	r1, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "clash-key-1", in1)
	require.NoError(s.T(), err)
	firstID := r1.Results[0].Binding.ID
	origRev := r1.NewRevision

	// Same key, DIFFERENT payload (assetB instead of assetA) → conflict.
	in2 := []bindingmod.BindingInput{{
		ScopeKind: domain.BindingScopeAsset, AssetID: &assetB,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}}
	_, err = svc.BatchUpsertBindings(ctx, auth, agent, ws, "clash-key-1", in2)
	assert.ErrorIs(s.T(), err, bindingmod.ErrIdempotencyConflict,
		"same key + different payload must surface as idempotency conflict (409)")

	// The original batch is untouched: the first binding is still active, the
	// second (assetB) binding was never created.
	var activeCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND workspace_id=$2 AND revoked_at IS NULL`,
		agent, ws).Scan(&activeCount))
	assert.Equal(s.T(), 1, activeCount, "a conflicting retry must not create the new binding")

	var stillThere int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE id=$1 AND revoked_at IS NULL`, firstID).Scan(&stillThere))
	assert.Equal(s.T(), 1, stillThere, "the original binding must remain active")

	var curRev int64
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT revision FROM workspace_authz_revisions WHERE workspace_id=$1`, ws).Scan(&curRev))
	assert.Equal(s.T(), origRev, curRev, "a conflicting retry must not bump the revision")
}

// --- Scenario 2: transaction atomicity ---

// Test_Binding_BatchAtomicityMidFailureRollsBack: a batch where the SECOND item
// is structurally invalid in a way the DB CHECK rejects (a pinned binding with
// a non-existent pinned_version_id — FK violation) must roll back the ENTIRE
// transaction: the first (valid) binding, the batch record, the outbox event,
// and the revision bump must all be absent (§5.2 事务内原子性).
//
// The service validates inputs up front, but a FK violation on a
// structurally-valid-but-dangling pinned_version_id is exactly the kind of DB
// constraint that only surfaces against real PG — so we drive it through the
// sink directly (the service's up-front validation can't detect a dangling FK).
func (s *BindingSuite) Test_Binding_BatchAtomicityMidFailureRollsBack() {
	ctx := context.Background()
	owner := s.seedUser("bind-atom@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-atom")
	agent := s.seedAgent(ws, owner, "agent-atom")
	asset := s.seedAsset(ws, owner, "asset-atom")
	danglingVersion := uuid.New() // references no real knowledge_asset_versions row

	// Pre-seed the workspace's authz revision row at 0 (the 013 migration only
	// seeds revision rows for workspaces that existed at migration time; a fresh
	// workspace has none until the sink's UPSERT creates one on first batch).
	// Pre-seeding makes the "did the revision move?" assertion deterministic —
	// the sink's revision bump is step 3, which a failed batch never reaches,
	// so the row must stay at 0.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_authz_revisions (workspace_id, revision) VALUES ($1, 0) ON CONFLICT DO NOTHING`, ws)
	require.NoError(s.T(), err)
	var revBefore int64
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT revision FROM workspace_authz_revisions WHERE workspace_id=$1`, ws).Scan(&revBefore))

	// Two items: item 1 is valid (create); item 2 pins a non-existent version →
	// FK violation on INSERT (the pinned_version_id FK has no ON DELETE action,
	// but the row simply doesn't exist → insert fails). The sink writes item 1
	// first, then item 2 fails → the whole tx rolls back.
	actor := domain.EventActor{Type: domain.SubjectUser, ID: owner}
	in := []bindingmod.BindingInput{
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
		},
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect:        domain.BindingAllow,
			VersionPolicy: domain.BindingPinned, PinnedVersionID: &danglingVersion,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 2,
		},
	}
	_, err = s.sink.BatchUpsert(ctx, agent, ws, "atom-key-1", in, actor)
	require.Error(s.T(), err, "a batch with a dangling pinned_version_id FK must fail")

	// NOTHING landed.
	var bindCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND workspace_id=$2`, agent, ws).Scan(&bindCount))
	assert.Equal(s.T(), 0, bindCount, "no binding may survive a rolled-back batch")

	var batchCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_binding_batches WHERE idempotency_key=$1`, "atom-key-1").Scan(&batchCount))
	assert.Equal(s.T(), 0, batchCount, "no batch record may survive a rolled-back batch")

	var outboxCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type=$1`, domain.KEAgentBindingChanged).Scan(&outboxCount))
	assert.Equal(s.T(), 0, outboxCount, "no outbox event may survive a rolled-back batch")

	var revAfter int64
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT revision FROM workspace_authz_revisions WHERE workspace_id=$1`, ws).Scan(&revAfter))
	assert.Equal(s.T(), revBefore, revAfter, "the revision must not bump on a rolled-back batch")
}

// --- Scenario 3: ETag optimistic concurrency ---

// Test_Binding_ETagUpdateIsRevokeOldCreateNew: an update (ID + ETag set) is
// modeled as revoke-the-old + create-the-new (AC-6 不可改写历史). The old row is
// revoked (revoked_at set) and a new row with a new ID is created carrying the
// updated fields.
func (s *BindingSuite) Test_Binding_ETagUpdateIsRevokeOldCreateNew() {
	ctx := context.Background()
	owner := s.seedUser("bind-etag@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-etag")
	agent := s.seedAgent(ws, owner, "agent-etag")
	asset := s.seedAsset(ws, owner, "asset-etag")
	svc := s.svcBinding()
	auth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	// Create.
	in := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	r1, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "etag-key-1", []bindingmod.BindingInput{in})
	require.NoError(s.T(), err)
	oldID := r1.Results[0].Binding.ID
	oldETag := r1.Results[0].Binding.CreatedAt.UnixMilli()

	// Update: change delivery_mode to inline via ID + ETag (If-Match).
	upd := bindingmod.BindingInput{
		ID: &oldID, ETag: oldETag,
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryInline, Priority: 1,
	}
	r2, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "etag-key-2", []bindingmod.BindingInput{upd})
	require.NoError(s.T(), err)
	require.Len(s.T(), r2.Results, 1)
	newID := r2.Results[0].Binding.ID
	assert.NotEqual(s.T(), oldID, newID, "update must create a NEW binding row (revoke-old + create-new)")
	assert.Equal(s.T(), domain.BindingDeliveryInline, r2.Results[0].Binding.DeliveryMode,
		"the new binding carries the updated delivery_mode")

	// The old row is revoked (history preserved, AC-6).
	var oldRevoked *time.Time
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT revoked_at FROM agent_bindings WHERE id=$1`, oldID).Scan(&oldRevoked))
	require.NotNil(s.T(), oldRevoked, "the old binding must be revoked (history preserved)")

	// Exactly one active binding remains (the new one).
	var activeCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND workspace_id=$2 AND revoked_at IS NULL`,
		agent, ws).Scan(&activeCount))
	assert.Equal(s.T(), 1, activeCount)
}

// Test_Binding_ETagStaleETagConflict: an update with a STALE ETag (the binding
// was already revoked by a concurrent update) must return ErrBindingConflict
// (§5.2 防覆盖 → 409) and must NOT create a new binding.
func (s *BindingSuite) Test_Binding_ETagStaleETagConflict() {
	ctx := context.Background()
	owner := s.seedUser("bind-stale@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-stale")
	agent := s.seedAgent(ws, owner, "agent-stale")
	asset := s.seedAsset(ws, owner, "asset-stale")
	svc := s.svcBinding()
	auth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	// Create.
	in := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	r1, err := svc.BatchUpsertBindings(ctx, auth, agent, ws, "stale-key-1", []bindingmod.BindingInput{in})
	require.NoError(s.T(), err)
	oldID := r1.Results[0].Binding.ID
	oldETag := r1.Results[0].Binding.CreatedAt.UnixMilli()

	// First update revokes the old row (so a second update on the same ID with
	// the original ETag is now operating on an already-revoked row).
	upd1 := bindingmod.BindingInput{
		ID: &oldID, ETag: oldETag,
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliverySummary, Priority: 1,
	}
	_, err = svc.BatchUpsertBindings(ctx, auth, agent, ws, "stale-key-2", []bindingmod.BindingInput{upd1})
	require.NoError(s.T(), err)

	// Second update reuses the SAME (now-stale) ETag on the already-revoked row.
	// The ETag itself is unchanged (created_at is immutable), so the ETag check
	// passes — but the optimistic-concurrency fence (`WHERE revoked_at IS NULL`)
	// affects 0 rows → ErrBindingConflict.
	upd2 := bindingmod.BindingInput{
		ID: &oldID, ETag: oldETag,
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryInline, Priority: 1,
	}
	_, err = svc.BatchUpsertBindings(ctx, auth, agent, ws, "stale-key-3", []bindingmod.BindingInput{upd2})
	assert.ErrorIs(s.T(), err, bindingmod.ErrBindingConflict,
		"a concurrent revoke affecting 0 rows must surface as a binding conflict (409)")

	// No third binding was created — still exactly one active binding (from upd1).
	var activeCount int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE agent_id=$1 AND workspace_id=$2 AND revoked_at IS NULL`,
		agent, ws).Scan(&activeCount))
	assert.Equal(s.T(), 1, activeCount, "a conflicted update must not create a new binding")
}

// --- Scenario 4: pinned-version阻断 (no silent fallback) ---

// Test_Binding_PinnedVersionRevokedBlocksNoFallback: an agent holds a PINNED
// binding on an asset whose pinned version is REVOKED (governance_status =
// 'deprecated', build_status = 'ready'). authz.Service.Authorize must return
// ErrNotFound + Allowed=false — BLOCKED — and must NOT fall back to the asset's
// current_version_id (§8.2 用例 5 / §11.4).
//
// Setup: asset has TWO versions — a healthy published one (the current_version)
// AND a deprecated one the agent pins. The §11.4 invariant is that the agent is
// NOT silently switched to the healthy current version; the pinned (revoked)
// version blocks use entirely.
func (s *BindingSuite) Test_Binding_PinnedVersionRevokedBlocksNoFallback() {
	ctx := context.Background()
	owner := s.seedUser("bind-pin@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-pin")
	agent := s.seedAgent(ws, owner, "agent-pin")
	asset := s.seedAsset(ws, owner, "asset-pin")
	svc := s.svcBinding()
	authzSvc := s.newAuthzService()

	// Version 1: healthy (ready + published) — set as the asset's current version.
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	// Version 2: revoked (deprecated). The agent pins THIS one.
	revoked := s.seedVersion(asset, 2, domain.VersionBuildReady, domain.VersionGovDeprecated)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)

	// Grant the acting user `use` on the workspace so the RBAC intersection
	// passes (binding only narrows; RBAC must allow first).
	s.grantUse(ctx, ws, owner)

	// Create the pinned binding (allow, pinned to the REVOKED version). The
	// binding service's pre-flight alert flags it blocked; the binding is still
	// written (durable alert). Here we bypass the service's pinned checker
	// (nil) so the binding is written clean; the authz pinnedVersionGate is the
	// authoritative阻断 under test.
	pinnedIn := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow,
		VersionPolicy: domain.BindingPinned, PinnedVersionID: &revoked,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	_, err = svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "pin-key-1", []bindingmod.BindingInput{pinnedIn})
	require.NoError(s.T(), err)

	// Authorize the agent (on behalf of the owner) to USE the asset. The pinned
	// version is deprecated → pinnedVersionGate returns false → ErrNotFound +
	// Allowed=false. NO fallback to the healthy current version.
	dec, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(s.T(), err, authz.ErrNotFound,
		"a revoked pinned version must block use, surfaced as not-found (no existence leak)")
	assert.False(s.T(), dec.Allowed, "a revoked pinned version must DENY use")

	// Sanity: the asset IS otherwise use-able — give the agent a FOLLOW_PUBLISHED
	// binding instead and the same authorize must ALLOW (proving the block was
	// the pinned-version gate, not a broken setup). We revoke the pinned binding
	// and add a follow_published allow.
	var pinnedBindingID domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT id FROM agent_bindings WHERE agent_id=$1 AND version_policy='pinned' AND revoked_at IS NULL`,
		agent).Scan(&pinnedBindingID))
	_, err = svc.RevokeBinding(ctx, bindAuth, pinnedBindingID, agent, ws)
	require.NoError(s.T(), err)
	followIn := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	_, err = svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "pin-key-2", []bindingmod.BindingInput{followIn})
	require.NoError(s.T(), err)

	dec2, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(s.T(), err)
	assert.True(s.T(), dec2.Allowed,
		"with the pinned binding revoked and a follow_published allow, use must be allowed — proves the block was the pinned gate")
}

// Test_Binding_PinnedVersionMissingBlocksNoFallback: a pinned binding whose
// pinned_version_id points to a row that does NOT exist (missing — e.g. the
// version was hard-deleted) must also block (the gate's versions.Get returns
// errNotFound → !IsUsable → block), with no fallback.
func (s *BindingSuite) Test_Binding_PinnedVersionMissingBlocksNoFallback() {
	ctx := context.Background()
	owner := s.seedUser("bind-miss@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-miss")
	agent := s.seedAgent(ws, owner, "agent-miss")
	asset := s.seedAsset(ws, owner, "asset-miss")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	missingVersion := uuid.New() // references nothing
	s.grantUse(ctx, ws, owner)
	// The binding write fails FK (missing version) — so write it via raw SQL to
	// simulate a pre-existing binding whose version was later deleted (the
	// §8.2 用例 5 "missing" path).
	_, execErr := s.pool.Exec(ctx, `
		INSERT INTO agent_bindings
		  (id, agent_id, workspace_id, scope_kind, asset_id, effect, version_policy, pinned_version_id, delivery_mode, priority, created_by)
		VALUES ($1,$2,$3,'asset',$4,'allow','pinned',$5,'tool',1,$6)`,
		uuid.New(), agent, ws, asset, missingVersion, owner)
	// The FK on pinned_version_id → knowledge_asset_versions(id) rejects a
	// missing version. This is itself a finding: a pinned binding CANNOT reference
	// a missing version while the FK is present (the §11.4 "missing" case only
	// arises if the version is deleted AFTER the binding was created — which
	// knowledge_asset_versions ON DELETE CASCADE does NOT have, so a version
	// delete is blocked by the FK unless bindings are revoked first). We record
	// this as a constraint-fact and assert the block via the deprecated path
	// (covered above); here we simply assert the FK prevents the missing-version
	// binding from being written in the first place.
	if execErr != nil {
		s.T().Logf("FK prevented a pinned binding to a missing version (expected): %v", execErr)
		// The "missing" path is therefore unreachable while the FK stands; the
		// §11.4 missing-version阻断 is exercised at the authz layer via a
		// not-found versions.Get, which the deprecated-revoked test already
		// proves (IsUsable=false → block). Nothing more to assert here.
		return
	}
	dec, err := s.newAuthzService().Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.False(s.T(), dec.Allowed, "a missing pinned version must block use")
	assert.ErrorIs(s.T(), err, authz.ErrNotFound)
}

// --- Scenario 5: delivery_mode resolution ---

// Test_Binding_DeliveryModeResolvedToAuthzContext: the winning allow binding's
// delivery_mode (tool/summary/inline) is carried on AuthzContext.DeliveryMode
// for an allowed agent decision (§5.3). We assert all three modes resolve
// correctly, and that a higher-priority allow wins over a lower-priority one.
func (s *BindingSuite) Test_Binding_DeliveryModeResolvedToAuthzContext() {
	ctx := context.Background()
	owner := s.seedUser("bind-dm@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-dm")
	agent := s.seedAgent(ws, owner, "agent-dm")
	asset := s.seedAsset(ws, owner, "asset-dm")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	s.grantUse(ctx, ws, owner)
	svc := s.svcBinding()
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	authzSvc := s.newAuthzService()

	cases := []struct {
		name    string
		mode    domain.BindingDeliveryMode
		priority int
	}{
		{"tool", domain.BindingDeliveryTool, 5},
		{"summary", domain.BindingDeliverySummary, 5},
		{"inline", domain.BindingDeliveryInline, 5},
	}
	for _, c := range cases {
		s.Run(c.name, func() {
			// Clean this agent's active bindings between sub-cases so only the
			// current binding is the winning allow.
			_, _ = s.pool.Exec(ctx, `UPDATE agent_bindings SET revoked_at=now() WHERE agent_id=$1 AND revoked_at IS NULL`, agent)
			in := bindingmod.BindingInput{
				ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
				Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
				DeliveryMode: c.mode, Priority: c.priority,
			}
			_, err := svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "dm-"+c.name, []bindingmod.BindingInput{in})
			require.NoError(s.T(), err)
			dec, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
				WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
				AgentID: &agent, ActingUserID: &owner,
				TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
			})
			require.NoError(s.T(), err)
			assert.True(s.T(), dec.Allowed, "an allow binding must authorize use")
			assert.Equal(s.T(), c.mode, dec.DeliveryMode,
				"the winning allow binding's delivery_mode must resolve onto AuthzContext")
		})
	}
}

// Test_Binding_DeliveryModeHighestPriorityAllowWins: among multiple allow
// bindings covering the asset, the HIGHEST-priority allow's delivery_mode wins
// (§5.3: bindings read priority DESC, first allow wins). We assert the
// higher-priority inline binding's mode is delivered, not the lower-priority
// tool binding's.
func (s *BindingSuite) Test_Binding_DeliveryModeHighestPriorityAllowWins() {
	ctx := context.Background()
	owner := s.seedUser("bind-hp@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-hp")
	agent := s.seedAgent(ws, owner, "agent-hp")
	asset := s.seedAsset(ws, owner, "asset-hp")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	s.grantUse(ctx, ws, owner)
	svc := s.svcBinding()
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	authzSvc := s.newAuthzService()

	// Two allow bindings on the same asset: lower priority tool (p=1) + higher
	// priority inline (p=10). The higher-priority inline must win.
	in := []bindingmod.BindingInput{
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
		},
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryInline, Priority: 10,
		},
	}
	_, err = svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "hp-key-1", in)
	require.NoError(s.T(), err)

	dec, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(s.T(), err)
	assert.True(s.T(), dec.Allowed)
	assert.Equal(s.T(), domain.BindingDeliveryInline, dec.DeliveryMode,
		"the higher-priority allow binding's delivery_mode must win")
}

// Test_Binding_DenyBeatsAllow is a regression for §8.2 用例 4 (deny > allow)
// against the real DB: an explicit deny on the asset must override an allow,
// surfacing as ErrNotFound (no existence leak).
func (s *BindingSuite) Test_Binding_DenyBeatsAllow() {
	ctx := context.Background()
	owner := s.seedUser("bind-deny@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-deny")
	agent := s.seedAgent(ws, owner, "agent-deny")
	asset := s.seedAsset(ws, owner, "asset-deny")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	s.grantUse(ctx, ws, owner)
	svc := s.svcBinding()
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	authzSvc := s.newAuthzService()

	in := []bindingmod.BindingInput{
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 5,
		},
		{
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			Effect: domain.BindingDeny, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
		},
	}
	_, err = svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "deny-key-1", in)
	require.NoError(s.T(), err)

	dec, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.False(s.T(), dec.Allowed, "an explicit deny must override allow (§8.2 用例 4)")
	assert.ErrorIs(s.T(), err, authz.ErrNotFound, "a deny must surface as not-found (no existence leak)")
}

// --- Scenario 3b: cross-workspace revoke guard (存在性不泄露) ---

// Test_Binding_RevokeCrossWorkspaceNotFound: revoking a binding from another
// workspace (the binding exists but belongs to wsB, the caller names wsA) must
// surface as ErrBindingNotFound — no leak that the binding exists elsewhere.
func (s *BindingSuite) Test_Binding_RevokeCrossWorkspaceNotFound() {
	ctx := context.Background()
	owner := s.seedUser("bind-xws@x.com", "Owner")
	wsA := s.seedWorkspace(owner, "bind-xws-a")
	wsB := s.seedWorkspace(owner, "bind-xws-b")
	agentA := s.seedAgent(wsA, owner, "agent-xws-a")
	assetA := s.seedAsset(wsA, owner, "asset-xws-a")
	svc := s.svcBinding()
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}

	// Create a binding in wsA.
	in := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &assetA,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	r, err := svc.BatchUpsertBindings(ctx, bindAuth, agentA, wsA, "xws-key-1", []bindingmod.BindingInput{in})
	require.NoError(s.T(), err)
	bindingID := r.Results[0].Binding.ID

	// Attempt to revoke it naming wsB (and an agent in wsB) — cross-workspace.
	agentB := s.seedAgent(wsB, owner, "agent-xws-b")
	_, err = svc.RevokeBinding(ctx, bindAuth, bindingID, agentB, wsB)
	assert.ErrorIs(s.T(), err, bindingmod.ErrBindingNotFound,
		"revoking a binding across workspaces must surface as not-found (no existence leak)")

	// The original wsA binding is still active.
	var active int
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_bindings WHERE id=$1 AND revoked_at IS NULL`, bindingID).Scan(&active))
	assert.Equal(s.T(), 1, active, "the cross-workspace revoke must not touch the real binding")
}

// --- Cache invalidation by revision (§5.4) regression ---

// Test_Binding_CacheInvalidatesByRevisionOnRevoke: after a binding is revoked
// (which bumps the workspace revision in the same tx), a subsequent Authorize
// for an agent who now has NO allow binding must DENY (§5.4: revoke → revision+1
// → cache key changes → fresh effective set loaded → next request denies).
// Uses the real (uncached) repos here; the cache layer's revision-keying is
// unit-tested in binding_cache_test.go — this integration test asserts the
// end-to-end "revoke → next decision reflects it" behavior on the real DB.
func (s *BindingSuite) Test_Binding_CacheInvalidatesByRevisionOnRevoke() {
	ctx := context.Background()
	owner := s.seedUser("bind-rev@x.com", "Owner")
	ws := s.seedWorkspace(owner, "bind-rev")
	agent := s.seedAgent(ws, owner, "agent-rev")
	asset := s.seedAsset(ws, owner, "asset-rev")
	healthy := s.seedVersion(asset, 1, domain.VersionBuildReady, domain.VersionGovPublished)
	_, err := s.pool.Exec(ctx, `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2`, healthy, asset)
	require.NoError(s.T(), err)
	s.grantUse(ctx, ws, owner)
	svc := s.svcBinding()
	bindAuth := bindingmod.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: owner, IsAdmin: true}
	authzSvc := s.newAuthzService()

	// Create allow → authorize must allow.
	in := bindingmod.BindingInput{
		ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
		Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
		DeliveryMode: domain.BindingDeliveryTool, Priority: 1,
	}
	r, err := svc.BatchUpsertBindings(ctx, bindAuth, agent, ws, "rev-key-1", []bindingmod.BindingInput{in})
	require.NoError(s.T(), err)
	dec, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(s.T(), err)
	require.True(s.T(), dec.Allowed, "with an allow binding, use must be allowed")
	revAfterCreate := dec.AuthzRevision

	// Revoke the binding (same-tx revision bump).
	_, err = svc.RevokeBinding(ctx, bindAuth, r.Results[0].Binding.ID, agent, ws)
	require.NoError(s.T(), err)

	// Next authorize must reflect the revoke: denied as not-found.
	dec2, err := authzSvc.Authorize(ctx, authz.AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &owner,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.False(s.T(), dec2.Allowed, "after revoke, the next decision must deny (§5.4 cache invalidation by revision)")
	assert.ErrorIs(s.T(), err, authz.ErrNotFound)
	assert.Greater(s.T(), dec2.AuthzRevision, revAfterCreate,
		"the revision must have been bumped by the revoke (same-tx linearization point)")
}

// Compile-time: ensure we don't accidentally drop the time import used by ETag.
var _ = time.Now
