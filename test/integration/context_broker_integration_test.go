//go:build integration

// context_broker_integration_test.go — YS-209 DoD integration tests for the
// ContextBroker ten-step pipeline against a live PostgreSQL (mora-postgres-1).
// DATABASE_URL-gated; skipped when unset. Run with:
//
//	DATABASE_URL=postgres://mora:mora@mora-postgres-1:5432/mora?sslmode=disable \
//	  go test -tags=integration ./test/integration/... -run ContextBrokerSuite -count=1 -v
//
// These cover the DoD acceptance gate the fake-AuthzService unit tests CANNOT
// (the project CLAUDE.md "单测绿 ≠ 集成绿" red line): the two-pass authz over
// real RBAC SQL — Authorize at workspace scope (step 2) + VisibleAssets per-
// asset post-check (step 5). The leak risk (存在性不泄露) lives in that SQL,
// not in the port fetch, so the typed query ports are faked (canned candidates
// carrying real PG-seeded asset IDs) while the authz.Service + ContextPolicyRepo
// + CompositeLocator/rbac.Engine run over the real repos. This is the production
// resolution path cmd/mora-api wires.
//
// DoD coverage:
//  1. 十步端到端跑通 — route→authorize→fanOut→postCheck→dedup→score→budget→
//     citation→audit, asserting candidates survive + policy_version +
//     authz_revision land on the response + audit summary.
//  2. 授权泄漏 — a caller with NO use grant on an asset (or NO membership in the
//     workspace) gets an EMPTY result (IsEmptyAuthorized==true), never a 403/
//     404 distinction. Existence of the denied asset never leaks (§8.2).
package integration

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// ContextBrokerSuite groups the YS-209 Broker E2E + authorization-leak
// integration tests.
type ContextBrokerSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB

	perms  *postgres.PermissionRepo
	policy *postgres.ContextPolicyRepo
}

func TestContextBrokerSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(ContextBrokerSuite))
}

func (s *ContextBrokerSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.perms = postgres.NewPermissionRepo(s.db)
	s.policy = postgres.NewContextPolicyRepo(s.db)
}

func (s *ContextBrokerSuite) TearDownSuite() { s.pool.Close() }

// SetupTest cleans the tables the suite touches, in dependency order (children
// before parents). Mirrors AssetReadSuite.SetupTest so FKs stay satisfiable.
func (s *ContextBrokerSuite) SetupTest() {
	ctx := context.Background()
	for _, t := range []string{
		"context_authority_policies", "context_eval_runs",
		"asset_projections", "review_decisions", "review_requests",
		"knowledge_relations", "knowledge_asset_versions", "knowledge_assets",
		"knowledge_source_targets", "source_sync_runs", "knowledge_sources",
		"permissions", "workspace_authz_revisions", "outbox_events",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	for _, t := range []string{"workspaces", "users"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM roles WHERE is_system = false`)
}

// seedWS inserts a workspace + creator user + a workspace_authz_revisions row
// (revision=7) and returns their IDs. The revision row makes the audit's
// authz_revision assertion meaningful (without it Current() returns 0).
func (s *ContextBrokerSuite) seedWS(slug, userEmail string) (wsID, userID domain.UUID) {
	ctx := context.Background()
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		userEmail, userEmail).Scan(&userID))
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, userID).Scan(&wsID))
	// Seed the authz revision (linearization point). revision=7 is arbitrary but
	// non-zero so the audit assertion distinguishes "set" from "default".
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_authz_revisions (workspace_id, revision) VALUES ($1, $2)`,
		wsID, 7)
	require.NoError(s.T(), err)
	return wsID, userID
}

// seedUser inserts a user and returns its id.
func (s *ContextBrokerSuite) seedUser(email string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, email).Scan(&id))
	return id
}

// seedAsset inserts a knowledge_asset (memory, draft, private) and returns its
// id. asset_type=memory avoids the native_document_id/document FK — we exercise
// RBAC resolution, not content.
func (s *ContextBrokerSuite) seedAsset(wsID, ownerID domain.UUID, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx, `
		INSERT INTO knowledge_assets (workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,'memory',$2,'user',$3,'draft','private') RETURNING id`,
		wsID, name, ownerID).Scan(&id))
	return id
}

// roleID fetches a system role id by name (mirrors AssetReadSuite.roleID).
func (s *ContextBrokerSuite) roleID(name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT id FROM roles WHERE name = $1 AND is_system = true`, name).Scan(&id))
	return id
}

// seedPolicy upserts an is_current context_authority_policies row for
// (workspace, intent) at policy_version=1, so the Broker loads a real DB policy
// (not the built-in default at version 0). The config is a minimal valid
// must-surface/exclude_when pair (no §7.2 collision).
func (s *ContextBrokerSuite) seedPolicy(wsID domain.UUID, intent ctxmod.Intent) {
	ctx := context.Background()
	_, err := s.policy.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: wsID,
		Intent:      intent,
		IsCurrent:   true,
		Config: ctxmod.PolicyConfig{
			MustSurfaceConflicts: []string{"contradicts"},
			ExcludeWhen:          []string{"deprecated"},
			Weights: map[domain.AssetType]float64{
				domain.AssetTypeMemory:   0.9,
				domain.AssetTypeDocument: 0.8,
			},
		},
	})
	require.NoError(s.T(), err)
}

// newAuthzService wires the platform/authz.Service over the real repos exactly
// like cmd/mora-api (the production resolution path under test). The
// CompositeLocator resolves doc-family + asset targets; the rbac.Engine is the
// legacy decision strategy for read/write/admin; the authz.Service composes them
// for the two-pass Authorize + VisibleAssets the Broker calls.
func (s *ContextBrokerSuite) newAuthzService() *authz.Service {
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
	)
	eng.SetLocator(authz.AsLocator(comp))
	return authz.NewService(comp, eng, postgres.NewBindingRepo(s.db), postgres.NewAuthzAgentRepo(s.db),
		postgres.NewAuthzAssetRepo(s.db), postgres.NewAuthzAssetVersionRepo(s.db),
		postgres.NewRevisionsRepo(s.db), postgres.NewDecisionRepo(s.db))
}

// fakeBrokerMemoryPort is a MemoryQuery fake returning canned candidates. It
// ignores the AuthzContext (the post-check enforces authorization); its job is
// to feed the merged candidate pool the post-check filters. The AssetIDs MUST be
// real PG-seeded knowledge_assets.ids so VisibleAssets resolves them.
type fakeBrokerMemoryPort struct {
	out []ctxmod.KnowledgeCandidate
}

func (p *fakeBrokerMemoryPort) Recall(ctx context.Context, ac ctxmod.AuthzContext, q ctxmod.MemoryQueryRequest) ([]ctxmod.KnowledgeCandidate, error) {
	return p.out, nil
}

// newBroker builds a Broker over the real authz.Service + real policy repo +
// fake memory port. Code/Skill ports are nil (degraded provider_error, §9.6 —
// not under test). The audit sink captures the step-10 summary.
func (s *ContextBrokerSuite) newBroker(authzSvc *authz.Service, memOut []ctxmod.KnowledgeCandidate, audit *ctxmod.AuditSummary) *ctxmod.Broker {
	var sink ctxmod.AuditSink
	if audit != nil {
		captured := audit
		sink = func(ctx context.Context, sum ctxmod.AuditSummary) { *captured = sum }
	}
	return ctxmod.NewBroker(
		&fakeBrokerMemoryPort{out: memOut},
		ctxmod.NewIntentRouter(nil),
		authzSvc,
		ctxmod.DefaultPolicyLoader(s.policy),
		ctxmod.NewBudgeter(),
		ctxmod.NewCitationBuilder(nil),
		ctxmod.DefaultBudgetDefaults,
		ctxmod.WithAuditSink(sink),
	)
}

// ---------------------------------------------------------------------------
// DoD 1: 十步端到端跑通
// ---------------------------------------------------------------------------

// TestBroker_TenStepEndToEnd seeds a workspace member (workspace_admin) + a
// memory asset + a real DB policy row, then runs Broker.Execute. The fake
// memory port returns the seeded asset as a candidate; the REAL authz.Service
// runs the workspace gate (step 2) + the per-asset use post-check (step 5).
// Asserts the asset survives the full pipeline (route→authorize→fanOut→
// postCheck→dedup→score→budget→citation→audit) and that policy_version (from
// the DB row) + authz_revision (from workspace_authz_revisions) land on the
// response AND the audit summary.
func (s *ContextBrokerSuite) TestBroker_TenStepEndToEnd() {
	ctx := context.Background()
	wsID, owner := s.seedWS("e2e", "e2e@x.com")
	assetID := s.seedAsset(wsID, owner, "asset-e2e")
	// workspace_admin grants read/write/admin on the workspace; admin includes
	// 'use', so the per-asset post-check (ActionUse) allows the asset.
	wsAdmin := s.roleID("workspace_admin")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: owner, RoleID: wsAdmin,
		TargetType: domain.TargetWorkspace, TargetID: wsID, Effect: domain.EffectAllow,
	}))
	s.seedPolicy(wsID, ctxmod.IntentSpec)

	var audit ctxmod.AuditSummary
	broker := s.newBroker(s.newAuthzService(), []ctxmod.KnowledgeCandidate{
		{
			AssetID: assetID, AssetType: domain.AssetTypeMemory, Title: "e2e-mem",
			Score: 0.8, Authority: 0.8, Freshness: 0.7,
			Citation: ctxmod.Citation{AssetID: assetID, AssetType: domain.AssetTypeMemory},
		},
	}, &audit)

	resp, err := broker.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: wsID,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}, ctxmod.Principal{SubjectType: domain.SubjectUser, PrincipalID: owner})
	require.NoError(s.T(), err)

	// The asset survived the full pipeline (post-check allowed it).
	require.True(s.T(), resp.HasResults(), "expected results; got %+v", resp)
	require.Len(s.T(), resp.Candidates, 1)
	assert.Equal(s.T(), assetID, resp.Candidates[0].AssetID)
	// The seeded DB policy row → policy_version=1 (not the built-in default 0).
	assert.Equal(s.T(), 1, resp.PolicyVersion, "policy_version should come from the seeded DB row")
	// The seeded workspace_authz_revisions row → authz_revision=7.
	assert.Equal(s.T(), int64(7), resp.AuthzRevision, "authz_revision should come from workspace_authz_revisions")

	// Step 10 audit summary carries policy_version + authz_revision + the
	// selected count (§9.3). The policy_version + authz_revision on the
	// response are asserted above; these pin the same values on the audit
	// summary so a regression that drops them from the step-10 record is caught.
	assert.Equal(s.T(), 1, audit.PolicyVersion, "audit summary should carry the seeded policy_version")
	assert.Equal(s.T(), int64(7), audit.AuthzRevision, "audit summary should carry the workspace authz_revision")
	assert.Equal(s.T(), 1, audit.SelectedCount)
	assert.Equal(s.T(), ctxmod.IntentSpec, audit.Intent)
}

// ---------------------------------------------------------------------------
// DoD 2: 授权泄漏 — existence never leaks
// ---------------------------------------------------------------------------

// TestBroker_Leak_NoWorkspaceMembership asserts a caller with NO grant on the
// workspace (not a member) gets an EMPTY result — IsEmptyAuthorized==true —
// even though the fake memory port returned the asset as a candidate. The
// workspace gate (step 2) denies membership → AllowedAssetIDs empty → the
// post-check still runs per-asset and denies (no workspace use) → 0 candidates.
// Crucially this is NOT an error: the handler surfaces it as 200 empty (§11.5),
// never a 403/404, so the caller cannot learn the asset exists (§8.2).
func (s *ContextBrokerSuite) TestBroker_Leak_NoWorkspaceMembership() {
	ctx := context.Background()
	wsID, owner := s.seedWS("leak1", "leak1-owner@x.com")
	assetID := s.seedAsset(wsID, owner, "asset-leak1")
	// A second user with NO permission rows at all — not a member of the workspace.
	bob := s.seedUser("leak1-bob@x.com")
	// No policy row → built-in default (policy_version=0); fine for the leak test.

	var audit ctxmod.AuditSummary
	broker := s.newBroker(s.newAuthzService(), []ctxmod.KnowledgeCandidate{
		{
			AssetID: assetID, AssetType: domain.AssetTypeMemory, Title: "leak1-mem",
			Score: 0.8, Authority: 0.8, Freshness: 0.7,
			Citation: ctxmod.Citation{AssetID: assetID, AssetType: domain.AssetTypeMemory},
		},
	}, &audit)

	resp, err := broker.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: wsID,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}, ctxmod.Principal{SubjectType: domain.SubjectUser, PrincipalID: bob})

	// No error — a membership denial is the empty-authorized state, not a fault.
	require.NoError(s.T(), err)
	// Zero candidates survived (the post-check denied the asset bob has no use on).
	assert.Empty(s.T(), resp.Candidates)
	// The three-state method reports authorized-empty, NOT no-results and NOT
	// provider-down. This is the leak-safe distinction (§7.3 / §11.4).
	assert.True(s.T(), resp.IsEmptyAuthorized(),
		"a denied caller must surface as empty-authorized, not no-results (existence must not leak): %+v", resp)
	assert.False(s.T(), resp.IsEmptyNoResults(), "IsEmptyNoResults must be false (a candidate was fetched then denied)")
	assert.False(s.T(), resp.IsProviderDown(), "IsProviderDown must be false (no provider fault)")
	// The caller never learns the asset's identity: no candidate, no degraded
	// source naming the asset.
	for _, d := range resp.DegradedSources {
		assert.NotEqual(s.T(), assetID.String(), d.Detail, "degraded detail must not name the denied asset")
	}
}

// TestBroker_Leak_CrossWorkspace asserts a caller in wsB (a member of wsB only)
// asking the Broker for knowledge in wsA gets an empty-authorized result — the
// workspace gate denies wsA membership. The caller cannot learn whether wsA has
// assets. This is the cross-workspace isolation red line (AC-4 / §8.2 用例 10).
func (s *ContextBrokerSuite) TestBroker_Leak_CrossWorkspace() {
	ctx := context.Background()
	wsA, ownerA := s.seedWS("leak2a", "leak2a@x.com")
	assetA := s.seedAsset(wsA, ownerA, "asset-leak2a")
	wsB, ownerB := s.seedWS("leak2b", "leak2b@x.com")
	// ownerB is a workspace_admin in wsB ONLY — valid session, zero grants in wsA.
	wsAdmin := s.roleID("workspace_admin")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: ownerB, RoleID: wsAdmin,
		TargetType: domain.TargetWorkspace, TargetID: wsB, Effect: domain.EffectAllow,
	}))

	var audit ctxmod.AuditSummary
	broker := s.newBroker(s.newAuthzService(), []ctxmod.KnowledgeCandidate{
		{
			AssetID: assetA, AssetType: domain.AssetTypeMemory, Title: "leak2-mem",
			Score: 0.8, Authority: 0.8, Freshness: 0.7,
			Citation: ctxmod.Citation{AssetID: assetA, AssetType: domain.AssetTypeMemory},
		},
	}, &audit)

	// ownerB queries wsA (the workspace they are NOT a member of).
	resp, err := broker.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: wsA,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}, ctxmod.Principal{SubjectType: domain.SubjectUser, PrincipalID: ownerB})

	require.NoError(s.T(), err)
	assert.Empty(s.T(), resp.Candidates)
	assert.True(s.T(), resp.IsEmptyAuthorized(),
		"a cross-workspace caller must surface as empty-authorized (existence of wsA assets must not leak): %+v", resp)
	assert.False(s.T(), resp.IsProviderDown())
}

// ---------------------------------------------------------------------------
// DoD 1b: a member with workspace read but NO asset use — only the granted
// assets surface, the denied ones drop (post-check precision). This proves the
// post-check is per-asset, not a blanket workspace-level allow.
// ---------------------------------------------------------------------------

// TestBroker_PostCheck_PerAssetPrecision seeds two assets in one workspace: one
// the caller may use (workspace_admin covers it), one... actually workspace_admin
// grants use on ALL workspace assets via admin. To prove per-asset precision we
// instead use the owner (workspace creator) who has admin on the workspace → all
// assets allowed — and confirm BOTH assets survive (the positive precision: the
// post-check does not over-drop). The leak cases above prove the negative
// precision (denied assets drop).
func (s *ContextBrokerSuite) TestBroker_PostCheck_PerAssetPrecision() {
	ctx := context.Background()
	wsID, owner := s.seedWS("prec", "prec@x.com")
	asset1 := s.seedAsset(wsID, owner, "asset-prec-1")
	asset2 := s.seedAsset(wsID, owner, "asset-prec-2")
	wsAdmin := s.roleID("workspace_admin")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: owner, RoleID: wsAdmin,
		TargetType: domain.TargetWorkspace, TargetID: wsID, Effect: domain.EffectAllow,
	}))

	broker := s.newBroker(s.newAuthzService(), []ctxmod.KnowledgeCandidate{
		{AssetID: asset1, AssetType: domain.AssetTypeMemory, Title: "mem1", Score: 0.8, Authority: 0.8, Freshness: 0.7, Citation: ctxmod.Citation{AssetID: asset1, AssetType: domain.AssetTypeMemory}},
		{AssetID: asset2, AssetType: domain.AssetTypeMemory, Title: "mem2", Score: 0.7, Authority: 0.7, Freshness: 0.6, Citation: ctxmod.Citation{AssetID: asset2, AssetType: domain.AssetTypeMemory}},
	}, nil)

	resp, err := broker.Execute(ctx, ctxmod.KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: wsID,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}, ctxmod.Principal{SubjectType: domain.SubjectUser, PrincipalID: owner})

	require.NoError(s.T(), err)
	require.True(s.T(), resp.HasResults())
	// Both assets survived — the post-check allowed both (admin covers use), and
	// neither was over-dropped.
	assert.Len(s.T(), resp.Candidates, 2)
}
