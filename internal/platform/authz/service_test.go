package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes for authz.Service (§8.2 decision matrix) ---

type fakeAssetRepo struct {
	assets map[uuid.UUID]AssetInfo
	err    error // force not-found
}

func (f *fakeAssetRepo) Get(_ context.Context, id uuid.UUID) (AssetInfo, error) {
	if f.err != nil {
		return AssetInfo{}, f.err
	}
	a, ok := f.assets[id]
	if !ok {
		return AssetInfo{}, errors.New("not found")
	}
	return a, nil
}

type fakeAgentRepo struct {
	agents map[uuid.UUID]AgentInfo
}

func (f *fakeAgentRepo) Get(_ context.Context, id uuid.UUID) (AgentInfo, error) {
	a, ok := f.agents[id]
	if !ok {
		return AgentInfo{}, errors.New("not found")
	}
	return a, nil
}

type fakeBindingRepo struct {
	binds map[uuid.UUID][]domain.AgentBinding // agentID -> bindings
}

func (f *fakeBindingRepo) ActiveForAgent(_ context.Context, agentID, _ uuid.UUID) ([]domain.AgentBinding, error) {
	return f.binds[agentID], nil
}

type fakeRevisionRepo struct{ rev int64 }

func (f *fakeRevisionRepo) Current(_ context.Context, _ uuid.UUID) (int64, error) { return f.rev, nil }

// fakeVersionRepo backs the pinned-version-revocation gate (§8.2 用例 5).
// versions maps a pinned version id to its usable state; a missing entry
// returns a not-found error the service maps to a deny (no existence leak).
type fakeVersionRepo struct {
	versions map[uuid.UUID]AssetVersionInfo
	err     error // force a read failure (fail-closed)
}

func (f *fakeVersionRepo) Get(_ context.Context, id uuid.UUID) (AssetVersionInfo, error) {
	if f.err != nil {
		return AssetVersionInfo{}, f.err
	}
	v, ok := f.versions[id]
	if !ok {
		return AssetVersionInfo{}, errors.New("not found")
	}
	return v, nil
}

// usableVersion is the healthy pinned version: build ready + governance
// published (the same invariant current_version_id enforces).
func usableVersion() AssetVersionInfo {
	return AssetVersionInfo{BuildStatus: domain.VersionBuildReady, GovernanceStatus: domain.VersionGovPublished}
}

type fakeDecisionRepo struct{ recorded DecisionRecord }

func (f *fakeDecisionRepo) Record(_ context.Context, d DecisionRecord) (uuid.UUID, error) {
	f.recorded = d
	return uuid.New(), nil
}

// fakeRBACRepo implements rbac.Repository with a grant table keyed by
// {subjectID, targetType, targetID} -> actions+effect, so the legacy engine
// makes deterministic decisions the Service can assert against.
type fakeRBACRepo struct {
	grants []domain.Grant
}

func (r *fakeRBACRepo) GrantsFor(_ context.Context, subject uuid.UUID, _ []uuid.UUID, _ uuid.UUID) ([]domain.Grant, error) {
	var out []domain.Grant
	for _, g := range r.grants {
		if g.SubjectType == domain.SubjectUser && g.SubjectID == subject {
			out = append(out, g)
		}
	}
	return out, nil
}
func (r *fakeRBACRepo) DirectoryAncestors(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *fakeRBACRepo) DocumentLocation(_ context.Context, _ uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}
func (r *fakeRBACRepo) DocumentsInDirectorySubtree(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// newTestService builds a Service with fakes and a real rbac.Engine over
// fakeRBACRepo. The Service's rbac.Engine uses its BUILT-IN doc-family
// resolution (no SetLocator): the Service owns the CompositeLocator for
// asset/agent targets, and delegates workspace/doc checks to the engine's
// internal locate/targetChain — so a [asset, workspace] chain can be
// evaluated node-by-node without re-resolving the workspace node through
// the asset-only CompositeLocator.
//
// versions feeds the pinned-version-revocation gate; callers with no pinned
// binding pass an empty repo (the gate is then a no-op).
func newTestService(t *testing.T, rbacRepo *fakeRBACRepo, assets *fakeAssetRepo, agents *fakeAgentRepo, binds *fakeBindingRepo, versions *fakeVersionRepo) *Service {
	t.Helper()
	eng := rbac.NewEngine(rbacRepo) // built-in doc-family path (no locator)
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)})
	return NewService(comp, eng, binds, agents, assets, versions, &fakeRevisionRepo{rev: 1}, &fakeDecisionRepo{})
}

// Test_UseCase1_UserWithoutUseDenied: user lacks 'use' grant on asset → deny,
// surfaced as ErrNotFound (existence not leaked). §8.2 用例 1.
func Test_UseCase1_UserWithoutUseDenied(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		// user has read on workspace — but read does NOT imply use.
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user}}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.ErrorIs(t, err, ErrNotFound, "use without 'use' grant must deny as not-found (no existence leak)")
	assert.False(t, dec.Allowed)
}

// Test_UseCase2_AgentOnBehalfNoUserRead: agent represents a user who lacks
// read on the asset's workspace — even with a binding allow → deny (intersection
// failure). §8.2 用例 2.
func Test_UseCase2_AgentOnBehalfNoUserRead(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	agent := uuid.New()
	user := uuid.New() // user has NO grants at all

	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user}}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeAsset, AssetID: &asset, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, &fakeRBACRepo{}, assets, agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "acting user has no RBAC use → intersection fails → deny as not-found")
	assert.False(t, dec.Allowed)
	assert.Contains(t, dec.Reason, "default deny")
}

// Test_UseCase3_AgentSelfBindingDoesNotEnlarge: agent (self, service account
// with no grants) + binding allow → deny. Binding only narrows. §8.2 用例 3.
func Test_UseCase3_AgentSelfBindingDoesNotEnlarge(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	agent := uuid.New()
	sa := uuid.New() // service account with NO grants

	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: agent}}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive, ServiceAccountID: &sa}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeAsset, AssetID: &asset, Effect: domain.BindingAllow}},
	}}
	svc := newTestService(t, &fakeRBACRepo{}, assets, agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, // no ActingUserID → self
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "binding allow cannot enlarge a service account with no RBAC use → deny as not-found")
	assert.False(t, dec.Allowed)
}

// Test_UseCase4_BindingDenyBeatsRBACAllow: principal has 'use' on workspace
// (RBAC allow) but a binding deny on the asset → deny. §8.2 用例 4.
func Test_UseCase4_BindingDenyBeatsRBACAllow(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	agent := uuid.New()
	user := uuid.New() // acting user WITH use on workspace
	assetPtr := asset

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user}}}
	agents := &fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}}
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow},
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws, ScopeKind: domain.BindingScopeAsset, AssetID: &assetPtr, Effect: domain.BindingDeny},
		},
	}}
	svc := newTestService(t, rbacRepo, assets, agents, binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "binding deny must beat RBAC allow → deny as not-found")
	assert.False(t, dec.Allowed)
	assert.Contains(t, dec.Reason, "binding explicit deny")
}

// Test_UseCase10_CrossWorkspaceAssetDenied: asset lives in another workspace
// → deny as ErrNotFound (no leak). §8.2 用例 10.
func Test_UseCase10_CrossWorkspaceAssetDenied(t *testing.T) {
	ws := uuid.New()
	otherWS := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: otherWS, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user}}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	_, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "cross-workspace asset must deny as not-found")
}

// Test_Lifecycle_ArchivedAssetBlocksUse: archived asset blocks use even when
// RBAC + binding allow. §8.2 用例 5 (status gate).
func Test_Lifecycle_ArchivedAssetBlocksUse(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetArchived, OwnerType: domain.SubjectUser, OwnerID: user}}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "archived asset blocks use → deny as not-found")
	assert.False(t, dec.Allowed)
	assert.Contains(t, dec.Reason, "lifecycle")
}

// Test_ReadDoesNotImplyUse: user with 'use' grant is allowed; user with only
// 'read' grant is denied for use (read does not imply use). §4.1 / D4.
func Test_ReadDoesNotImplyUse(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	userWithUse := uuid.New()
	userWithRead := uuid.New()

	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: userWithUse}}}
	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: userWithUse, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
		{SubjectType: domain.SubjectUser, SubjectID: userWithRead, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	// user with 'use' grant → allowed.
	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: userWithUse,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "user with explicit 'use' grant must be allowed")

	// user with only 'read' → denied (read does not imply use).
	_, err = svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: userWithRead,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound)
}

// Test_VisibleAssets_FiltersByAuthorize: only assets passing Authorize for
// use are returned; others are absent (no existence leak).
func Test_VisibleAssets_FiltersByAuthorize(t *testing.T) {
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
		allowed:   {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user},
		archived:  {WorkspaceID: ws, Status: domain.AssetArchived, OwnerType: domain.SubjectUser, OwnerID: user},
	}}
	svc := newTestService(t, rbacRepo, assets, &fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	out, err := svc.VisibleAssets(context.Background(), ListScope{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
	}, []uuid.UUID{allowed, archived, nonexistent})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{allowed}, out, "only the visible asset is returned")
}

// Test_IssueDecision_RecordsAndReturnsCapability: a positive decision yields a
// recorded authorization_decision + DecisionCapability carrying the ID/revision.
func Test_IssueDecision_RecordsAndReturnsCapability(t *testing.T) {
	ws := uuid.New()
	asset := uuid.New()
	user := uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: user, Actions: []domain.Action{domain.ActionUse},
			TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
	}}
	assets := &fakeAssetRepo{assets: map[uuid.UUID]AssetInfo{asset: {WorkspaceID: ws, Status: domain.AssetPublished, OwnerType: domain.SubjectUser, OwnerID: user}}}
	decRepo := &fakeDecisionRepo{}
	svc := NewService(
		NewCompositeLocator(struct {
			Type TargetType
			Loc  ResourceLocator
		}{Type: domain.TargetAsset, Loc: NewAssetLocator(assets)}),
		rbac.NewEngine(rbacRepo), &fakeBindingRepo{}, &fakeAgentRepo{}, assets, nil, &fakeRevisionRepo{rev: 7}, decRepo,
	)

	cap, err := svc.IssueDecision(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	}, "mcp-server")
	require.NoError(t, err)
	assert.Equal(t, int64(7), cap.AuthzRevision, "capability carries the authz revision")
	assert.NotEqual(t, uuid.Nil, cap.DecisionID)
	assert.Equal(t, "mcp-server", decRepo.recorded.Audience)
	assert.Equal(t, domain.ActionUse, decRepo.recorded.Action)
}
