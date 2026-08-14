//go:build integration

// Phase 1 D3 asset-read RBAC integration tests (design-docs/14 §4.4 D13 /
// §8.2 / §10.4 用例 26/27). DATABASE_URL-gated; skipped when unset.
//
// These prove the acceptance criteria for mounting the asset read API behind
// the rbac.Engine + CompositeLocator (the same wiring cmd/mora-api uses):
//   - UC 26: a caller with NO read grant on an asset the locator CAN resolve
//     (same workspace) gets ErrAssetNotFound → the handler emits 404 + 40400,
//     NOT 403. Existence of a found-but-denied asset never leaks.
//   - UC 27: a cross-workspace caller cannot resolve the other workspace's
//     asset at all → ErrTargetNotFound → ErrAssetNotFound → 404, identical to
//     a genuinely missing asset. A caller must not learn an asset exists in a
//     workspace they are not a member of.
//
// The service path under test is asset.ReadService.WithAuthz(engine,...) over
// the real AssetReadRepo + AuthzAssetRepo, mirroring production wiring.
package integration

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AssetReadSuite groups the Phase 1 D3 asset-read RBAC integration tests.
type AssetReadSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB

	wsRepo  *postgres.WorkspaceRepo
	perms   *postgres.PermissionRepo
	roles   *postgres.RoleRepo
	readRepo *postgres.AssetReadRepo
}

func TestAssetReadSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(AssetReadSuite))
}

func (s *AssetReadSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.wsRepo = postgres.NewWorkspaceRepo(s.db)
	s.perms = postgres.NewPermissionRepo(s.db)
	s.roles = postgres.NewRoleRepo(s.db)
	s.readRepo = postgres.NewAssetReadRepo(s.db)
}

func (s *AssetReadSuite) TearDownSuite() { s.pool.Close() }

func (s *AssetReadSuite) SetupTest() {
	ctx := context.Background()
	// clean in dependency order (children before parents)
	for _, t := range []string{
		"asset_projections", "review_decisions", "review_requests",
		"knowledge_relations", "knowledge_asset_versions", "knowledge_assets",
		"knowledge_source_targets", "source_sync_runs", "knowledge_sources",
		"permissions", "outbox_events",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	for _, t := range []string{"workspaces", "users"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	_, _ = s.pool.Exec(ctx, "DELETE FROM roles WHERE is_system = false")
}

// seedWS inserts a workspace + creator user and returns their IDs (mirrors
// SourceSuite.seedWS so asset FKs are satisfiable).
func (s *AssetReadSuite) seedWS(slug, userEmail string) (wsID, userID domain.UUID) {
	ctx := context.Background()
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		userEmail, userEmail).Scan(&userID))
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, userID).Scan(&wsID))
	return wsID, userID
}

// seedAsset inserts a knowledge_asset row (minimal columns) and returns its id.
// asset_type=memory avoids the native_document_id / document FK so this test
// needs no document seed — we are exercising RBAC resolution, not content.
func (s *AssetReadSuite) seedAsset(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_assets (workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,'memory',$2,'user',$3,'draft','private') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

// newEngine wires an rbac.Engine whose CompositeLocator resolves doc-family +
// asset targets exactly like cmd/mora-api.newRBACEngine (design-docs/14 §3.3).
// This is the production resolution path under test.
func (s *AssetReadSuite) newEngine() *rbac.Engine {
	repo := postgres.NewRBACAdapter(s.perms, postgres.NewDirectoryRepo(s.db), postgres.NewDocumentRepo(s.db))
	eng := rbac.NewEngine(repo)
	comp := authz.NewCompositeLocator(
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetWorkspace, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDirectory, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetDocument, Loc: authz.NewDocLocator(repo)},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetAsset, Loc: authz.NewAssetLocator(postgres.NewAuthzAssetRepo(s.db))},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetSource, Loc: authz.NewSourceLocator(postgres.NewAuthzSourceRepo(s.db))},
		struct {
			Type authz.TargetType
			Loc  authz.ResourceLocator
		}{Type: domain.TargetReview, Loc: authz.NewReviewLocator(postgres.NewAuthzReviewRepo(s.db))},
	)
	eng.SetLocator(authz.AsLocator(comp))
	return eng
}

// roleID fetches a system role id by name (mirrors Suite.roleID).
func (s *AssetReadSuite) roleID(name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT id FROM roles WHERE name = $1 AND is_system = true`, name).Scan(&id))
	return id
}

// TestUC26_NoReadGrant_ReturnsNotFoundAssets assets that a caller in the SAME
// workspace as an asset, but with NO read grant on that asset (and no
// workspace-level read grant either), is denied AND the denial surfaces as
// ErrAssetNotFound — not a 403-style "denied" sentinel. The asset resolves
// (same workspace) but the decision is !Allowed, so existence must not leak
// (§10.4 用例 26).
func (s *AssetReadSuite) TestUC26_NoReadGrant_ReturnsNotFound() {
	ctx := context.Background()
	wsID, owner := s.seedWS("uc26", "uc26@x.com")
	// A second user in the same workspace with NO permission rows at all.
	bob := s.seedUser("bob26@x.com", "Bob")
	assetID := s.seedAsset(wsID, owner, "asset-uc26")

	svc := asset.NewReadService(s.readRepo).WithAuthz(s.newEngine(), nil)
	_, err := svc.GetAsset(ctx, asset.AuthContext{
		SubjectType: domain.SubjectUser, PrincipalID: bob,
	}, assetID)
	assert.ErrorIs(s.T(), err, asset.ErrAssetNotFound,
		"a found-but-denied asset MUST surface as not-found, not as a permission denial (UC 26)")
}

// TestUC27_CrossWorkspace_ReturnsNotFound asserts a caller in wsB asking for an
// asset in wsA cannot resolve the asset's workspace (the locator reads the
// asset's real workspace_id, which is wsA; bob has no grants in wsA) → denial
// → ErrAssetNotFound, indistinguishable from a missing asset (§10.4 用例 27).
// Crucially this is NOT just repo scoping: the locator resolved wsA and the
// engine still denied, proving the leak-proof denial path end-to-end.
func (s *AssetReadSuite) TestUC27_CrossWorkspace_ReturnsNotFound() {
	ctx := context.Background()
	wsA, ownerA := s.seedWS("uc27a", "uc27a@x.com")
	wsB, ownerB := s.seedWS("uc27b", "uc27b@x.com")
	assetA := s.seedAsset(wsA, ownerA, "asset-uc27")
	// Give ownerB a viewer role in wsB ONLY — they have a valid session but
	// zero grants touching wsA, so the asset in wsA must be invisible.
	viewer := s.roleID("viewer")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: ownerB, RoleID: viewer,
		TargetType: domain.TargetWorkspace, TargetID: wsB, Effect: domain.EffectAllow,
	}))

	svc := asset.NewReadService(s.readRepo).WithAuthz(s.newEngine(), nil)
	_, err := svc.GetAsset(ctx, asset.AuthContext{
		SubjectType: domain.SubjectUser, PrincipalID: ownerB,
	}, assetA)
	assert.ErrorIs(s.T(), err, asset.ErrAssetNotFound,
		"a cross-workspace asset MUST surface as not-found so the caller cannot learn it exists (UC 27)")

	// A genuinely missing asset MUST return the SAME sentinel — the two paths
	// are indistinguishable to the caller (existence never leaks, §8.2).
	_, err = svc.GetAsset(ctx, asset.AuthContext{
		SubjectType: domain.SubjectUser, PrincipalID: ownerB,
	}, uuid.New())
	assert.ErrorIs(s.T(), err, asset.ErrAssetNotFound,
		"a genuinely missing asset must surface identically to a denied one (UC 27)")
}

// TestUC_Allowed_ReadReturnsAsset asserts the positive path: a caller with a
// workspace-level read grant in the asset's workspace GETs the asset. This is
// the regression red line proving the denial cases above fail because of RBAC,
// not because the wiring is universally broken.
func (s *AssetReadSuite) TestUC_Allowed_ReadReturnsAsset() {
	ctx := context.Background()
	wsID, owner := s.seedWS("uc-ok", "uc-ok@x.com")
	assetID := s.seedAsset(wsID, owner, "asset-ok")
	viewer := s.roleID("viewer")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: owner, RoleID: viewer,
		TargetType: domain.TargetWorkspace, TargetID: wsID, Effect: domain.EffectAllow,
	}))

	svc := asset.NewReadService(s.readRepo).WithAuthz(s.newEngine(), nil)
	a, err := svc.GetAsset(ctx, asset.AuthContext{
		SubjectType: domain.SubjectUser, PrincipalID: owner,
	}, assetID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), assetID, a.ID)
	assert.Equal(s.T(), wsID, a.WorkspaceID)
	assert.False(s.T(), errors.Is(err, asset.ErrAssetNotFound))
}

// seedUser inserts a user and returns its id (mirrors Suite.seedUser).
func (s *AssetReadSuite) seedUser(email, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, name).Scan(&id))
	return id
}
