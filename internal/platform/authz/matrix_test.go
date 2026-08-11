package authz

// matrix_test.go is the Phase 0 authorization test matrix (design-docs/13 §8).
//
// Coverage map (each row ties to a §8 acceptance criterion):
//
//	§8.1 positive paths  — admin implies use; agent+binding; service_account;
//	                       asset_type scope; workspace-scope allow; VisibleAssets.
//	§8.2 over-permit deny — see service_test.go for use cases 1/2/3/4/10 +
//	                       lifecycle archived. This file adds:
//	                         UC5  pinned binding version revoked → block
//	                         UC6  revocation → next request sync-deny
//	                         UC7  60s projection convergence (batch authoritative)
//	                         UC8  MCP no delegated context (handler/middleware_matrix_test.go)
//	                         UC9  delegated JWT expired/revoked → refuse
//	§8.3 regression       — Service doc-family delegation agrees with engine;
//	                       doc read/write/admin via Service unchanged;
//	                       VisibleDocuments behavior unchanged; locator agree.
//
// The fakes (fakeRBACRepo / fakeAssetRepo / fakeAgentRepo / fakeBindingRepo /
// fakeRevisionRepo / fakeDecisionRepo / miniRepo) live in service_test.go and
// locator_test.go and are reused here — they are package-internal.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---

// grantOn is a compact grant builder for the matrix.
func grantOn(sub domain.SubjectType, subID uuid.UUID, actions []domain.Action,
	tt domain.TargetType, id uuid.UUID, effect domain.Effect) domain.Grant {
	return domain.Grant{
		SubjectType: sub, SubjectID: subID, Actions: actions,
		TargetType: tt, TargetID: id, Effect: effect,
	}
}

// workspaceUseGrant is the most common positive RBAC precondition: a principal
// with explicit 'use' on the workspace (the asset chain's least-specific node).
func workspaceUseGrant(subID uuid.UUID, ws uuid.UUID) domain.Grant {
	return grantOn(domain.SubjectUser, subID, []domain.Action{domain.ActionUse},
		domain.TargetWorkspace, ws, domain.EffectAllow)
}

// publishedAsset builds a fakeAssetRepo with one published asset in ws.
func publishedAsset(id, ws, owner uuid.UUID) *fakeAssetRepo {
	return &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		id: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: owner},
	}}
}

// assetWithStatus is like publishedAsset but lets the caller pin a status.
func assetWithStatus(id, ws, owner uuid.UUID, status domain.AssetStatus) *fakeAssetRepo {
	return &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{
		id: {WorkspaceID: ws, Status: status, OwnerType: domain.SubjectUser, OwnerID: owner},
	}}
}

// =====================================================================
// §8.1 Positive paths — the happy-path counterpart to the deny matrix.
// A decision that SHOULD allow. If any of these flip to deny, the matrix
// is over-restrictive (a regression of its own).
// =====================================================================

// Test_Positive_AdminImpliesUse: a workspace admin (admin grant) may use an
// asset — admin still implies use at the engine layer (hasAction admin path).
func Test_Positive_AdminImpliesUse(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		grantOn(domain.SubjectUser, user, []domain.Action{domain.ActionAdmin},
			domain.TargetWorkspace, ws, domain.EffectAllow),
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user), &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "workspace admin must imply use on a published asset")
}

// Test_Positive_UserWithUseAllowed: the canonical positive path — user has
// explicit 'use' on the workspace, asset published → allowed.
func Test_Positive_UserWithUseAllowed(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user), &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
}

// Test_Positive_AgentOnBehalfWithUserUse: agent represents a user who HAS use
// on the workspace + a binding allow on the asset → allowed (intersection holds).
// This is the positive counterpart to UC2 (intersection failure).
func Test_Positive_AgentOnBehalfWithUserUse(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user), agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "agent with user use + binding allow must be allowed")
}

// Test_Positive_AgentSelfWithServiceAccountUse: agent (self) whose service
// account HAS use on the workspace + binding allow → spec says allowed.
//
// authz.Service.rbacSubject resolves agent-self to its ServiceAccountID and
// asks rbac.Engine.Check for ActionUse. The engine's GrantsFor matches
// subject_type='service_account' (PermissionRepo.GrantsFor in
// internal/infra/postgres/rbac.go, plus the in-memory fakes), so the grant
// row is returned and the decision is allow. UC3 asserts binding-cannot-enlarge;
// this test asserts the flip side: a legitimately authorized service account
// is permitted. Covers YS-106 / PR2 defect #1.
func Test_Positive_AgentSelfWithServiceAccountUse(t *testing.T) {
	ws, asset, agent, sa := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		grantOn(domain.SubjectServiceAccount, sa, []domain.Action{domain.ActionUse},
			domain.TargetWorkspace, ws, domain.EffectAllow),
	}}
	assets := publishedAsset(asset, ws, agent)
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive, ServiceAccountID: &sa}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, rbacRepo, assets, agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID:    &agent, // no ActingUserID → self
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "agent self with service_account use + binding allow must be allowed")
}

// Test_Positive_BindingScopeAssetTypeCoversAsset: an asset_type-scoped binding
// allow covers the target asset (§4.3 scope matching).
func Test_Positive_BindingScopeAssetTypeCoversAsset(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAssetType, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user), agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "asset_type scoped binding allow must cover the asset")
}

// Test_Positive_BindingScopeWorkspaceCoversAsset: a workspace-scoped binding
// allow covers any asset in the workspace.
func Test_Positive_BindingScopeWorkspaceCoversAsset(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user), agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
}

// Test_Positive_DeprecatedAllowsSyncOnly: a deprecated asset blocks use but
// ALLOWS sync (lifecycle gate: deprecated → only sync permitted). This guards
// both the deny and the one allowed action on a deprecated asset.
func Test_Positive_DeprecatedAllowsSyncOnly(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		grantOn(domain.SubjectUser, user, []domain.Action{domain.ActionUse, domain.ActionSync},
			domain.TargetWorkspace, ws, domain.EffectAllow),
	}}
	svc := newTestService(t, rbacRepo, assetWithStatus(asset, ws, user, domain.AssetDeprecated),
		&fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	// use on deprecated → blocked.
	_, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "use on deprecated asset must deny")

	// sync on deprecated → allowed (re-publish path).
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionSync,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "sync on deprecated asset must be allowed")
}

// Test_Positive_LifecycleReviewingDraftAllowUse: reviewing + draft + published
// assets all permit use (the non-terminal lifecycle states).
func Test_Positive_LifecycleReviewingDraftAllowUse(t *testing.T) {
	ws, user := uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	for _, st := range []domain.AssetStatus{domain.AssetPublished, domain.AssetReviewing, domain.AssetDraft} {
		asset := uuid.New()
		svc := newTestService(t, rbacRepo, assetWithStatus(asset, ws, user, st), &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})
		dec, err := svc.Authorize(context.Background(), AuthzRequest{
			WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
			TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
		})
		require.NoError(t, err)
		assert.Truef(t, dec.Allowed, "status %s must permit use", st)
	}
}

// Test_Lifecycle_RejectedBlocksUse: a rejected asset blocks use (terminal state
// alongside archived — the other half of UC5's lifecycle gate).
func Test_Lifecycle_RejectedBlocksUse(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	svc := newTestService(t, rbacRepo, assetWithStatus(asset, ws, user, domain.AssetRejected),
		&fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	_, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "rejected asset must block use")
}

// =====================================================================
// §8.2 UC5 — pinned binding version revocation.
// A pinned binding (VersionPolicy=pinned, PinnedVersionID set) whose pinned
// version is revoked must block use — it must NOT silently fall back to the
// latest published version (§11.4). A version is usable iff build_status=
// 'ready' AND governance_status='published' (the same invariant the asset's
// current_version_id FK enforces, design-docs/12 §4.2); any other state
// (deprecated/superseded/rejected/failed/in-flight) counts as revoked.
// =====================================================================

// Test_UseCase5_PinnedVersionRevokedBlocks: pinned binding's version is
// deprecated (governance_status=deprecated → no longer usable) → use must
// block as ErrNotFound, no auto-fallback to the latest published version.
// The asset itself is published, so only the pinned-version gate blocks.
func Test_UseCase5_PinnedVersionRevokedBlocks(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pinnedVersion := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	assets := publishedAsset(asset, ws, user)
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			VersionPolicy: domain.BindingPinned, PinnedVersionID: &pinnedVersion,
			Effect: domain.BindingAllow}},
	}}
	// Pinned version: build ready but governance deprecated → revoked.
	versions := &fakeVersionRepo{versions: map[uuid.UUID]AssetVersionInfo{
		pinnedVersion: {BuildStatus: domain.VersionBuildReady, GovernanceStatus: domain.VersionGovDeprecated},
	}}
	svc := newTestService(t, rbacRepo, assets, agents, binds, versions)

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "pinned version revoked must block use (no auto-fallback)")
	assert.False(t, dec.Allowed)
	assert.Contains(t, dec.Reason, "pinned version revoked")
}

// Test_UseCase5_PinnedVersionSupersededBlocks: a superseded pinned version
// (build_status=superseded) also blocks — every non-ready/non-published
// state is treated as revoked by the gate, not only 'deprecated'.
func Test_UseCase5_PinnedVersionSupersededBlocks(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pinnedVersion := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			VersionPolicy: domain.BindingPinned, PinnedVersionID: &pinnedVersion,
			Effect: domain.BindingAllow}},
	}}
	versions := &fakeVersionRepo{versions: map[uuid.UUID]AssetVersionInfo{
		pinnedVersion: {BuildStatus: domain.VersionBuildSuperseded, GovernanceStatus: domain.VersionGovPublished},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, versions)

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "superseded pinned version must block use")
	assert.False(t, dec.Allowed)
}

// Test_UseCase5_PinnedVersionMissingBlocks: the pinned version row is gone
// (e.g. retention deleted it — §12.2 "Asset Version ... 固定绑定阻断并告警").
// A missing version must block, not fall through to the asset's latest.
func Test_UseCase5_PinnedVersionMissingBlocks(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pinnedVersion := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			VersionPolicy: domain.BindingPinned, PinnedVersionID: &pinnedVersion,
			Effect: domain.BindingAllow}},
	}}
	// versions map deliberately omits pinnedVersion → not-found.
	versions := &fakeVersionRepo{versions: map[uuid.UUID]AssetVersionInfo{}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, versions)

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "missing pinned version must block use")
	assert.False(t, dec.Allowed)
}

// Test_UseCase5_PinnedVersionHealthyAllows: the positive counterpart — a
// pinned binding whose version is still usable (ready+published) does NOT
// block; the request proceeds to RBAC+binding narrowing and is allowed.
// Guards against the gate over-restricting (regression of its own).
func Test_UseCase5_PinnedVersionHealthyAllows(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	pinnedVersion := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			VersionPolicy: domain.BindingPinned, PinnedVersionID: &pinnedVersion,
			Effect: domain.BindingAllow}},
	}}
	versions := &fakeVersionRepo{versions: map[uuid.UUID]AssetVersionInfo{
		pinnedVersion: usableVersion(),
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, versions)

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "a usable pinned version must not block use")
}

// Test_UseCase5_FollowPublishedIgnoresVersionGate: a follow_published
// binding (the default) has no pinned version, so the gate is a no-op and
// the asset's own lifecycle (published) governs — the gate must not trip
// on follow_published bindings even when the versions repo is empty.
func Test_UseCase5_FollowPublishedIgnoresVersionGate(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
			ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
			VersionPolicy: domain.BindingFollowPublished,
			Effect:        domain.BindingAllow}},
	}}
	versions := &fakeVersionRepo{versions: map[uuid.UUID]AssetVersionInfo{}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, versions)

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "follow_published binding must not trip the pinned-version gate")
}

// =====================================================================
// §8.2 UC6 — revocation → next request synchronous deny.
// After a delegated session is revoked (which bumps workspace_authz_revisions),
// the very next VerifyDelegated must refuse with ErrDelegatedStaleRevision
// (or ErrDelegatedRevoked for the same session). This is the §5.6
// "撤权后下一次请求同步拒绝" linearization guarantee.
// =====================================================================

func Test_UseCase6_RevokeSyncDenyNextRequest(t *testing.T) {
	m, sessions, rev := newDelegatedManager(t)
	ws := uuid.New()

	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Before revoke: verifies.
	_, err = m.VerifyDelegated(context.Background(), token)
	require.NoError(t, err, "fresh session must verify before revocation")

	// Locate the session row to revoke it.
	var sessionID uuid.UUID
	for id := range sessions.sessions {
		sessionID = id
	}
	newRev, err := m.Revoke(context.Background(), sessionID, ws)
	require.NoError(t, err)
	assert.Equal(t, int64(1), newRev, "Revoke must bump the workspace revision to 1")

	// NEXT request — the same token must be refused. The revoked row OR the
	// bumped revision is enough; either error is the sync-deny signal.
	_, err = m.VerifyDelegated(context.Background(), token)
	assert.True(t, errors.Is(err, ErrDelegatedRevoked) || errors.Is(err, ErrDelegatedStaleRevision),
		"next request after revoke must be refused (got %v)", err)

	// A DIFFERENT session issued under the old revision is also stale.
	_ = rev // rev was bumped by Revoke; the stale check applies to any pre-rev session.
}

// Test_UseCase6_PermissionYankedSyncDeny: when a permission is yanked (revision
// bumps without a session revoke), any session issued under the old revision
// is refused on the next verify. This is the "撤权同步拒绝" path for a
// non-session revoke (e.g. an RBAC permission delete).
func Test_UseCase6_PermissionYankedSyncDeny(t *testing.T) {
	m, _, rev := newDelegatedManager(t)
	ws := uuid.New()

	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Verify passes at revision 0.
	_, err = m.VerifyDelegated(context.Background(), token)
	require.NoError(t, err)

	// A permission is yanked elsewhere → revision bumps.
	rev.bump(ws)

	// Next verify must refuse — stale revision.
	_, err = m.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedStaleRevision,
		"a revision bump after a permission yank must refuse the next verify synchronously")
}

// =====================================================================
// §8.2 UC7 — 60s projection convergence.
// After a permission yank, the Qdrant/FTS projection may lag; the spec says
// Mora's batch check is authoritative in the interim and the projection MUST
// converge (invisible) within 60s. At the authz layer the "batch check" is
// Authorize/VisibleAssets reading the current revision — so the decision flips
// to deny immediately (UC6) and the visible set no longer contains the asset.
// This test asserts the VisibleAssets flip; the 60s wall-clock bound is a
// deployment/RAG-layer property exercised in integration, not here.
// =====================================================================

func Test_UseCase7_VisibleAssetsConvergesAfterYank(t *testing.T) {
	ws, allowed, user := uuid.New(), uuid.New(), uuid.New()

	// rev lets the test bump the workspace revision, modeling a permission
	// yank that takes effect on the next Authorize read.
	rev := newFakeMutableRevisionRepo()
	assets := publishedAsset(allowed, ws, user)

	// RBAC grants keyed by subject; we mutate the grant set to model the yank.
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	eng := rbac.NewEngine(rbacRepo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, assets, nil, rev, &fakeDecisionRepo{})

	// t0: asset is visible.
	out, err := svc.VisibleAssets(context.Background(), ListScope{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
	}, []uuid.UUID{allowed})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{allowed}, out, "asset visible before yank")

	// Yank: remove the use grant AND bump the revision. The next read sees
	// the new grant set (no use) → not visible. This is the batch-authoritative
	// decision the projection must converge to within 60s.
	rbacRepo.grants = nil
	rev.bump(ws)

	out, err = svc.VisibleAssets(context.Background(), ListScope{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
	}, []uuid.UUID{allowed})
	require.NoError(t, err)
	assert.Empty(t, out, "asset must be invisible after the permission yank (projection converges)")
}

// =====================================================================
// §8.2 UC9 — delegated JWT expired or session revoked → refuse.
// Expired (>30s) or revoked session → VerifyDelegated returns an error.
// =====================================================================

func Test_UseCase9_ExpiredDelegatedRefused(t *testing.T) {
	// Build a manager with a 1s TTL so we can observe expiry within the test.
	rev := newFakeMutableRevisionRepo()
	sessions := newFakeSessionRepo(rev)
	m := NewDelegatedManager("test-secret", 1*time.Second, sessions, rev)

	ws := uuid.New()
	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	// Wait for the row to expire past the 1s TTL.
	time.Sleep(1500 * time.Millisecond)

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.Error(t, err, "an expired delegated JWT must be refused")
}

func Test_UseCase9_RevokedDelegatedRefused(t *testing.T) {
	m, sessions, _ := newDelegatedManager(t)
	ws := uuid.New()
	token, _, err := m.IssueDelegated(context.Background(), DelegatedRequest{
		WorkspaceID: ws, Actions: []domain.Action{domain.ActionUse},
		AuthzRevision: 0, Audience: "mcp-server",
	})
	require.NoError(t, err)

	var sessionID uuid.UUID
	for id := range sessions.sessions {
		sessionID = id
	}
	_, err = m.Revoke(context.Background(), sessionID, ws)
	require.NoError(t, err)

	_, err = m.VerifyDelegated(context.Background(), token)
	assert.ErrorIs(t, err, ErrDelegatedRevoked, "a revoked session must be refused on next verify")
}

// =====================================================================
// §8.3 Regression — existing doc / RAG / MCP behavior unchanged.
// =====================================================================

// Test_Regression_ServiceDocFamilyDelegatesToEngine: a doc-family Authorize
// (read/write/admin on document/directory/workspace) delegates to the legacy
// rbac.Engine and produces the engine's decision unchanged. Guards the PR1
// red line (Check/VisibleDocuments behavior identical pre/post delegation).
//
// Group memberships are plumbed through AuthzRequest.GroupIDs (PR2 gap #2
// resolved), so a Service doc-family check now agrees with the engine for
// group-inherited grants as well as direct user grants. Group-inherited read
// is asserted directly in Test_Gap_ServiceDropsGroups; this test stays a
// direct-user-grant regression for the delegation seam.
func Test_Regression_ServiceDocFamilyDelegatesToEngine(t *testing.T) {
	ws, root, sub, doc, subj := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	repo := &miniRepo{
		grants: []domain.Grant{
			// DIRECT user read grant on root directory (subtree inheritance).
			grantOn(domain.SubjectUser, subj, []domain.Action{domain.ActionRead},
				domain.TargetDirectory, root, domain.EffectAllow),
		},
		ancestors: map[uuid.UUID][]uuid.UUID{sub: {root, sub}, root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}
	eng := rbac.NewEngine(repo) // built-in path, no locator
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetDocument, Loc: NewDocLocator(repo)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, &fakeAssetRepo{}, nil,
		&fakeRevisionRepo{rev: 1}, &fakeDecisionRepo{})

	ctx := context.Background()
	// read via the Service (which delegates doc-family to engine.Check).
	svcRead, err := svc.Authorize(ctx, AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: subj,
		TargetType: domain.TargetDocument, TargetID: doc, Action: domain.ActionRead,
	})
	require.NoError(t, err)
	assert.True(t, svcRead.Allowed, "direct user read on root dir must inherit to doc via Service→Engine")

	// write not granted → deny.
	svcWrite, err := svc.Authorize(ctx, AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: subj,
		TargetType: domain.TargetDocument, TargetID: doc, Action: domain.ActionWrite,
	})
	require.NoError(t, err)
	assert.False(t, svcWrite.Allowed)

	// The engine's direct Check must agree with the Service's decision for
	// the same direct-user-grant inputs (delegation does not alter the outcome).
	directRead, err := eng.Check(ctx, subj, nil, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.Equal(t, directRead.Allowed, svcRead.Allowed, "Service doc read must match engine Check")
}

// Test_Gap_ServiceDropsGroups: authz.Service plumbs the principal's group
// memberships through AuthzRequest.GroupIDs into rbac.Engine.Check, so
// group-inherited doc permissions are visible on the Service path. This was
// PR2 gap #2 (AuthzRequest had no GroupIDs field; rbacSubject returned nil
// groups); the fix added GroupIDs to AuthzRequest/ListScope and made
// rbacSubject forward them. The test now asserts the Service's decision
// agrees with the engine's for a group-inherited read — the gap is closed.
//
// Existing handlers call rbac.Engine.Check directly with groups from
// AuthState, so the §8.3 regression (handler behavior) was never affected;
// this gap only blocked authz.Service from serving as the unified doc-family
// entry point. Recorded as PR2 gap #2 in the YS-94 report.
func Test_Gap_ServiceDropsGroups(t *testing.T) {
	ws, root, sub, doc, subj, group := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := &miniRepo{
		grants: []domain.Grant{{
			SubjectType: domain.SubjectGroup, SubjectID: group,
			Actions:    []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow,
		}},
		ancestors: map[uuid.UUID][]uuid.UUID{sub: {root, sub}, root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}
	eng := rbac.NewEngine(repo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetDocument, Loc: NewDocLocator(repo)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, &fakeAssetRepo{}, nil,
		&fakeRevisionRepo{rev: 1}, &fakeDecisionRepo{})

	ctx := context.Background()
	groups := []uuid.UUID{group}

	// Engine directly: group read on root → doc visible (groups passed).
	engDec, err := eng.Check(ctx, subj, groups, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.True(t, engDec.Allowed, "engine with groups must allow group-inherited read")

	// Service: AuthzRequest now carries GroupIDs → the same group grant is
	// visible, and the Service's decision must agree with the engine's.
	svcDec, err := svc.Authorize(ctx, AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: subj,
		GroupIDs:   groups,
		TargetType: domain.TargetDocument, TargetID: doc, Action: domain.ActionRead,
	})
	require.NoError(t, err)
	assert.True(t, svcDec.Allowed, "Service with GroupIDs must allow group-inherited read")
	assert.Equal(t, engDec.Allowed, svcDec.Allowed,
		"Service doc read with groups must match engine Check")

	// Sanity: without groups the Service denies (group grant invisible) —
	// the gap behavior, now opt-in by omission rather than the only option.
	noGroupDec, err := svc.Authorize(ctx, AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: subj,
		TargetType: domain.TargetDocument, TargetID: doc, Action: domain.ActionRead,
	})
	require.NoError(t, err)
	assert.False(t, noGroupDec.Allowed, "omitting GroupIDs leaves the group grant invisible")
}

// Test_Regression_DocDenyAtSubdirBlocksViaService: the engine's
// deny-at-subdir-blocks-doc behavior is preserved when a doc-family check
// goes through authz.Service. Companion to rbac.TestEngine_Check_DenyAtSubdirBlocksDoc.
func Test_Regression_DocDenyAtSubdirBlocksViaService(t *testing.T) {
	ws, root, sub, doc, subj := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := &miniRepo{
		grants: []domain.Grant{
			grantOn(domain.SubjectUser, subj, []domain.Action{domain.ActionRead},
				domain.TargetDirectory, root, domain.EffectAllow),
			grantOn(domain.SubjectUser, subj, []domain.Action{domain.ActionRead},
				domain.TargetDirectory, sub, domain.EffectDeny),
		},
		ancestors: map[uuid.UUID][]uuid.UUID{sub: {root, sub}, root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}
	eng := rbac.NewEngine(repo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetDocument, Loc: NewDocLocator(repo)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, &fakeAssetRepo{}, nil,
		&fakeRevisionRepo{rev: 1}, &fakeDecisionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: subj,
		TargetType: domain.TargetDocument, TargetID: doc, Action: domain.ActionRead,
	})
	require.NoError(t, err)
	assert.False(t, dec.Allowed, "deny on sub must block doc even with allow on root, via Service")
}

// Test_Regression_AdminImpliesReadAndWrite: a doc-family admin grant implies
// read+write+admin through the Service path (engine hasAction admin path intact).
func Test_Regression_AdminImpliesReadAndWrite(t *testing.T) {
	ws, doc, admin := uuid.New(), uuid.New(), uuid.New()
	repo := &miniRepo{
		grants: []domain.Grant{
			grantOn(domain.SubjectUser, admin, []domain.Action{domain.ActionAdmin},
				domain.TargetDocument, doc, domain.EffectAllow),
		},
	}
	eng := rbac.NewEngine(repo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetDocument, Loc: NewDocLocator(repo)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, &fakeAssetRepo{}, nil,
		&fakeRevisionRepo{rev: 1}, &fakeDecisionRepo{})

	ctx := context.Background()
	for _, a := range []domain.Action{domain.ActionRead, domain.ActionWrite, domain.ActionAdmin} {
		dec, err := svc.Authorize(ctx, AuthzRequest{
			WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: admin,
			TargetType: domain.TargetDocument, TargetID: doc, Action: a,
		})
		require.NoError(t, err)
		assert.Truef(t, dec.Allowed, "admin must imply %s on a document", a)
	}
}

// Test_Regression_VisibleDocumentsUnchanged: the engine's VisibleDocuments
// contract is unchanged by the locator delegation — a workspace-wide read
// grant marks the workspace sentinel visible, a doc-level deny removes a doc.
func Test_Regression_VisibleDocumentsUnchanged(t *testing.T) {
	ws, dir, doc, allowed, denied, subj := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := &miniRepo{
		grants: []domain.Grant{
			grantOn(domain.SubjectUser, subj, []domain.Action{domain.ActionRead},
				domain.TargetWorkspace, ws, domain.EffectAllow),
			grantOn(domain.SubjectUser, subj, []domain.Action{domain.ActionRead},
				domain.TargetDocument, denied, domain.EffectDeny),
		},
		ancestors: map[uuid.UUID][]uuid.UUID{dir: {dir}},
		docLoc: map[uuid.UUID][2]uuid.UUID{
			allowed: {ws, dir}, denied: {ws, dir}, doc: {ws, dir},
		},
	}
	bare := rbac.NewEngine(repo)  // built-in path
	deleg := rbac.NewEngine(repo) // delegated path
	deleg.SetLocator(AsLocator(NewDocLocator(repo)))

	ctx := context.Background()
	visBare, err := bare.VisibleDocuments(ctx, subj, nil, ws)
	require.NoError(t, err)
	visDeleg, err := deleg.VisibleDocuments(ctx, subj, nil, ws)
	require.NoError(t, err)
	assert.Equal(t, visBare, visDeleg, "VisibleDocuments must be identical built-in vs delegated")
	// Workspace-wide allow → sentinel set; denied doc removed.
	assert.True(t, visBare[uuid.Nil], "workspace-wide read grants the sentinel")
	assert.False(t, visBare[denied], "denied doc must be invisible")
}

// Test_Regression_EngineLocatorAgreement is the cross-check that the engine's
// delegated path agrees with its built-in path for read AND write on a doc
// (companion to rbac.TestEngine_DelegatesToLocator, run through the authz
// adapter so the AsLocator seam is exercised, not just an in-package stub).
func Test_Regression_EngineLocatorAgreement(t *testing.T) {
	ws, root, sub, doc, group := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	repo := &miniRepo{
		grants: []domain.Grant{{
			SubjectType: domain.SubjectGroup, SubjectID: group,
			Actions:    []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow,
		}},
		ancestors: map[uuid.UUID][]uuid.UUID{sub: {root, sub}, root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}
	bare := rbac.NewEngine(repo)
	deleg := rbac.NewEngine(repo)
	deleg.SetLocator(AsLocator(NewDocLocator(repo)))

	ctx := context.Background()
	for _, a := range []domain.Action{domain.ActionRead, domain.ActionWrite} {
		d1, err := bare.Check(ctx, uuid.New(), []uuid.UUID{group}, domain.TargetDocument, doc, a)
		require.NoError(t, err)
		d2, err := deleg.Check(ctx, uuid.New(), []uuid.UUID{group}, domain.TargetDocument, doc, a)
		require.NoError(t, err)
		assert.Equal(t, d1.Allowed, d2.Allowed, "delegated %s must agree with built-in", a)
		assert.Equal(t, d1.Reason, d2.Reason, "delegated %s reason must agree", a)
	}
}

// Test_Regression_ServiceCarriesAuthzRevision: the AuthzContext returned by
// Authorize carries the workspace authz revision it read (the linearization
// point — §5.6). A bumped revision is observable on the next call.
func Test_Regression_ServiceCarriesAuthzRevision(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()
	rev := newFakeMutableRevisionRepo()
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	assets := publishedAsset(asset, ws, user)
	eng := rbac.NewEngine(rbacRepo)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)})
	svc := NewService(comp, eng, &fakeBindingRepo{}, &fakeAgentRepo{}, assets, nil, rev, &fakeDecisionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(0), dec.AuthzRevision, "initial revision is 0")

	rev.bump(ws) // permission yank → revision 1
	dec, err = svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	// Grant still present (we only bumped the revision), so still allowed — but
	// the decision must carry the NEW revision.
	require.NoError(t, err)
	assert.Equal(t, int64(1), dec.AuthzRevision, "bumped revision must be reflected in the next decision")
}

// Ensure the package compiles with the sync import used by fakeSessionRepo
// (declared in delegated_test.go) — this var is a no-op compile guard.
var _ = sync.Mutex{}

// Ensure errors is referenced (used by UC6's errors.Is).
var _ = errors.Is
