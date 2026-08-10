package authz

// matrix_test.go fills the §8 authorization test-matrix gaps (design-docs/13
// §8). The existing service_test.go covers UC1-4, UC10, lifecycle, read≠use,
// VisibleAssets, IssueDecision; delegated_test.go covers the delegated happy /
// revoked / stale / forged / ttl / input / signature paths. This file adds:
//
//	§8.2 UC5 — pinned binding version revoked → block, no auto-fallback (spec gap)
//	§8.2 UC6 — revoke → next request synchronous deny (Service layer, no cache)
//	§8.2 UC7 — cache/projection convergence contract (unit level)
//	§8.2 UC8 — MCP missing delegated context → degrade to SA, no admin (spec gap)
//	§8.2 UC9 — delegated JWT expired → deny
//	§8.1     — access-path matrix (direct ID / list / MCP tool / Provider)
//	§8.3     — regression: doc-family delegation unchanged
//
// Fakes are shared with service_test.go / delegated_test.go (same package).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// §8.2 Use Case 5: pinned binding version revoked → block, no auto-fallback
// ---------------------------------------------------------------------------

// Test_UseCase5_PinnedVersionRevokedBlocks documents §8.2 用例 5: a binding with
// version_policy=pinned whose pinned_version_id has been revoked MUST block use
// and MUST NOT auto-fallback to the latest version (§11.4).
//
// STATUS: SKIPPED — implementation gap. The authz.Service.lifecycleGate currently
// checks only the asset's top-level Status (published/draft/reviewing/deprecated/
// archived/rejected); it does NOT consult the binding's VersionPolicy or
// PinnedVersionID, and the AssetInfo/AgentInfo projections carry no
// version-revocation status. The pinned-version-revocation gate is therefore
// unimplemented at the authz layer. Tracked as a separate defect issue.
func Test_UseCase5_PinnedVersionRevokedBlocks(t *testing.T) {
	t.Skip("UC5: pinned-version-revocation gate not yet implemented in authz.Service.lifecycleGate (defect filed)")

	ws := uuid.New()
	asset := uuid.New()
	pinnedVer := uuid.New()
	agent := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	revokedVerPtr := pinnedVer
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{
			ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &revokedVerPtr,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingPinned,
			PinnedVersionID: &pinnedVer,
		}},
	}}
	svc := newTestService(t, rbacRepo, assets, agents, binds)

	// When the pinned version is revoked, use MUST be blocked — not fall back to latest.
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "pinned version revoked must block use as not-found")
	assert.False(t, dec.Allowed, "must NOT auto-fallback to latest version")
}

// ---------------------------------------------------------------------------
// §8.2 Use Case 6: revoke → next request synchronous deny (Service layer)
// ---------------------------------------------------------------------------

// Test_UseCase6_RevokeThenNextRequestSynchronousDeny proves the §5.6 "撤权后下一
// 次请求同步拒绝" guarantee at the authz.Service layer: there is NO grant cache —
// rbacForTarget reads grants fresh from the repository on every call, so
// removing a grant is immediately effective on the next Authorize (no 60s wait,
// no stale window). The delegated-layer half (revision bump invalidating
// sessions) is covered by Test_Delegated_StaleRevisionRefused.
func Test_UseCase6_RevokeThenNextRequestSynchronousDeny(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{})

	// Before revoke: use is allowed.
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	require.True(t, dec.Allowed, "grant present → use allowed")

	// Revoke the grant (simulate the permission DELETE — no cache invalidation
	// needed because the Service reads fresh each call).
	rbacRepo.grants = nil

	// Next request: synchronously denied — no 60s convergence window at this layer.
	dec, err = svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "revoke must be effective on the very next request (no cache)")
	assert.False(t, dec.Allowed)
}

// Test_UseCase6_FullRevokeFlowBothLayers exercises the complete §5.6 linearization:
// Service.Authorize allows → grant revoked + revision bumped → Service.Authorize
// denies (fresh read) AND a previously-issued delegated session is refused
// (stale revision). Both layers synchronize on the same revision bump.
func Test_UseCase6_FullRevokeFlowBothLayers(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()
	agent := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	rev := newFakeMutableRevisionRepo()
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow}},
	}}

	// Wire the Service with a MUTABLE revision repo so the revoke can bump it.
	eng := rbac.NewEngine(rbacRepo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)})
	svc := NewService(comp, eng, binds, agents, assets, rev, &fakeDecisionRepo{})

	// Delegate manager shares the same revision repo (§5.6: one source of truth).
	dm := NewDelegatedManager("matrix-secret", 10*time.Second, newFakeSessionRepo(rev), rev)

	// 1. Initial Authorize: agent-on-behalf-of-user with use grant → allowed.
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	require.True(t, dec.Allowed, "grant present → use allowed")

	// 2. Issue a delegated session under the current revision (MCP-style flow).
	token, _, err := dm.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: dec.AuthzRevision, Audience: "mcp-server",
		AgentID: &agent, ActingUserID: &user,
	})
	require.NoError(t, err)

	// The session verifies while the revision is unchanged.
	_, err = dm.VerifyDelegated(context.Background(), token)
	require.NoError(t, err, "session valid before revoke")

	// 3. Revoke: remove the grant AND bump the workspace authz revision (same tx).
	rbacRepo.grants = nil
	rev.bump(ws)

	// 4a. Service layer: next Authorize synchronously denies (fresh grant read).
	dec, err = svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "Service denies on next request after revoke")
	assert.False(t, dec.Allowed)

	// 4b. Delegated layer: the previously-valid session is now stale (revision moved).
	_, err = dm.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedStaleRevision, "delegated session refused after revision bump")
}

// ---------------------------------------------------------------------------
// §8.2 Use Case 7: cache/projection convergence contract (unit level)
// ---------------------------------------------------------------------------

// Test_UseCase7_NoCacheAtServiceLayer proves the unit-level half of §8.2 用例 7:
// the authz.Service holds NO in-memory grant or decision cache — every
// Authorize reads grants fresh and stamps the current revision. So a revocation
// is reflected on the next call with zero convergence delay at this layer.
//
// The 60-second convergence window (Qdrant visible_to / FTS projection recompute)
// is an integration concern covered by tests/e2e/rbac_cross_layer_test.go, which
// asserts the projection converges within 60s using Eventually(60s, 3s).
func Test_UseCase7_NoCacheAtServiceLayer(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{})

	// Two consecutive calls with the same grant → both allowed (no cache poisoning).
	for i := 0; i < 2; i++ {
		dec, err := svc.Authorize(context.Background(), AuthzRequest{
			WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
			TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
		})
		require.NoError(t, err)
		assert.True(t, dec.Allowed, "call %d: grant present → allowed", i)
	}

	// Flip the grant to deny → next call immediately reflects it (no stale cache).
	rbacRepo.grants[0].Effect = domain.EffectDeny
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	// Deny on workspace → the asset is denied via the chain; existence not leaked.
	assert.False(t, dec.Allowed, "deny grant must be immediately effective (no cache)")
	// The deny surfaces as ErrNotFound (existence non-leak) per §8.2 用例 4 variant.
	_ = err
}

// ---------------------------------------------------------------------------
// §8.2 Use Case 8: MCP missing delegated context → degrade to SA, no admin
// ---------------------------------------------------------------------------

// Test_UseCase8_MissingDelegatedContextNoAdmin asserts the DelegatedManager-level
// contract for §8.2 用例 8 / §4.4: a request with no delegated JWT (or an invalid
// one) must NOT yield a valid capability — there is no "admin fallback". The
// caller stays at service-account authority (capability = nil, error). The
// middleware-level degradation (internal token without delegated JWT →
// service_account, not admin) is an implementation gap — see the skipped test
// below.
func Test_UseCase8_MissingDelegatedContextNoAdmin(t *testing.T) {
	m, _, _ := newDelegatedManager(t)

	// No token at all → verify fails (no admin elevation).
	_, err := m.VerifyDelegated(context.Background(), "")
	assert.Error(t, err, "empty delegated token must not verify (no admin fallback)")

	// Garbage token → signature parse fails.
	_, err = m.VerifyDelegated(context.Background(), "not-a-jwt")
	assert.Error(t, err, "malformed delegated token must not verify (no admin fallback)")
}

// Test_UseCase8_MiddlewareNoDelegatedDegradesToServiceAccount documents the
// TARGET behavior of §4.4: when the mora-api AuthMiddleware receives an internal
// service token WITHOUT a delegated JWT, the caller must be treated as a
// service_account with its own (limited) authority — IsAdmin MUST be false.
//
// STATUS: SKIPPED — implementation gap. The current AuthMiddleware
// (internal/module/mora/handler/middleware.go:37) sets IsAdmin=true unconditionally
// for INTERNAL_SERVICE_TOKEN bearers and does not consume a delegated context.
// The delegated-context consumption (§4.4 step 2) is not yet wired into the
// middleware. Tracked as a separate defect issue.
func Test_UseCase8_MiddlewareNoDelegatedDegradesToServiceAccount(t *testing.T) {
	t.Skip("UC8: AuthMiddleware does not yet consume delegated context; still falls back to admin (defect filed)")
}

// ---------------------------------------------------------------------------
// §8.2 Use Case 9: delegated JWT expired → deny
// ---------------------------------------------------------------------------

// Test_UseCase9_DelegatedExpiredRefused proves §8.2 用例 9: a delegated session
// whose server-side row has passed its ExpiresAt (≤30s, §5.6) is refused with
// ErrDelegatedExpired on the next verify. The signature is still valid; the
// expiry check on the authoritative row is what denies.
func Test_UseCase9_DelegatedExpiredRefused(t *testing.T) {
	m, sessions, _ := newDelegatedManager(t)
	ws := uuid.New()

	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Verify passes while fresh.
	_, err = m.VerifyDelegated(context.Background(), token)
	require.NoError(t, err)

	// Force the server-side row into the past (simulate TTL elapse).
	var sessionID uuid.UUID
	sessions.mu.Lock()
	for id, s := range sessions.sessions {
		sessionID = id
		s.ExpiresAt = time.Now().UTC().Add(-1 * time.Second)
		sessions.sessions[id] = s
	}
	sessions.mu.Unlock()
	require.NotEqual(t, uuid.Nil, sessionID, "found the session row")

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedExpired, "expired session must be refused with ErrDelegatedExpired")
}

// ---------------------------------------------------------------------------
// §8.1 Access-path matrix: direct ID / list / MCP tool / Provider / async
// ---------------------------------------------------------------------------

// Test_AccessPath_DirectIDQuery covers the 直接 ID 查询 path (§8.1): a principal
// requests a specific asset by ID through Service.Authorize. This is the path
// used by asset_read / asset_use endpoints. Existence non-leak on deny.
func Test_AccessPath_DirectIDQuery(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{})

	// Direct ID: allowed principal → Authorized.
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)

	// Direct ID: nonexistent asset → ErrNotFound (no existence leak).
	_, err = svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: uuid.New(), Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "nonexistent asset via direct ID → not-found (no leak)")
}

// Test_AccessPath_ListPath covers the 列表 path (§8.1): VisibleAssets filters a
// candidate set down to those the principal may use. Non-visible / nonexistent
// candidates are absent from the result (存在性不泄露). This is the path used by
// list_assets / directory-tree endpoints.
func Test_AccessPath_ListPath(t *testing.T) {
	ws := uuid.New()
	allowed := uuid.New()
	archived := uuid.New()
	nonexistent := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		allowed:  {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
		archived: {WorkspaceID: ws, Status: domain.AssetArchived, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{})

	out, err := svc.VisibleAssets(context.Background(), ListScope{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
	}, []uuid.UUID{allowed, archived, nonexistent})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{allowed}, out, "list path returns only visible assets; archived + nonexistent absent")

	// Empty candidate set → empty result (no leak).
	out, err = svc.VisibleAssets(context.Background(), ListScope{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// Test_AccessPath_MCPToolFlow covers the MCP 工具 path (§8.1): the full delegated
// chain an MCP tool call walks — Authorize → IssueDecision → IssueDelegated →
// VerifyDelegated → (re-)Authorize under the delegated context. This is the path
// mcp-server uses to call mora-api on behalf of a principal (§4.4).
func Test_AccessPath_MCPToolFlow(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()
	agent := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	rev := newFakeMutableRevisionRepo()
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow}},
	}}

	eng := rbac.NewEngine(rbacRepo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)})
	svc := NewService(comp, eng, binds, agents, assets, rev, &fakeDecisionRepo{})
	dm := NewDelegatedManager("mcp-flow-secret", 10*time.Second, newFakeSessionRepo(rev), rev)

	req := AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	}

	// Step 1: Authorize the agent-on-behalf-of-user.
	dec, err := svc.Authorize(context.Background(), req)
	require.NoError(t, err)
	require.True(t, dec.Allowed, "MCP flow: initial authorize must allow")

	// Step 2: Issue a decision capability (Provider seam).
	cap, err := svc.IssueDecision(context.Background(), req, "mcp-server")
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, cap.DecisionID)

	// Step 3: Issue a delegated session under the decision's revision.
	token, _, err := dm.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: cap.AuthzRevision, Audience: "mcp-server",
		AgentID: &agent, ActingUserID: &user,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	// Step 4: Verify the delegated session (the mora-api middleware would do this).
	claims, err := dm.VerifyDelegated(context.Background(), token)
	require.NoError(t, err)
	assert.Equal(t, ws.String(), claims.WorkspaceID)
	assert.Equal(t, agent.String(), claims.AgentID)
	assert.Equal(t, user.String(), claims.ActingUserID)

	// Step 5: re-Authorize under the verified identity — still allowed.
	dec, err = svc.Authorize(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "MCP flow: re-authorize after delegated verify must allow")
}

// Test_AccessPath_ProviderCall covers the 内部 Provider 调用 path (§8.1):
// Service.IssueDecision records an authorization_decision and returns a signed
// capability (DecisionID + AuthzRevision) a Provider validates. A denied
// request yields no capability.
func Test_AccessPath_ProviderCall(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	// No use grant → deny.
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	decRepo := &fakeDecisionRepo{}
	svc := NewService(
		NewCompositeLocator(struct {
			Type TargetType
			Loc  ResourceLocator
		}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)}),
		rbac.NewEngine(rbacRepo), &fakeBindingRepo{}, &fakeAgentRepo{}, assets,
		&fakeRevisionRepo{rev: 1}, decRepo,
	)

	// Denied request → no capability issued.
	_, err := svc.IssueDecision(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	}, "mcp-server")
	assert.Error(t, err, "denied decision must not yield a capability")
}
