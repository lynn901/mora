package authz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test_DeliveryMode_ResolvedFromWinningAllowBinding (Phase 5 §5.3): an
// allowed agent binding carries its delivery_mode (tool/summary/inline) onto
// the AuthzContext so the MCP/internal delivery layer knows what to deliver.
// The winning allow is the highest-priority allow covering the target.
func Test_DeliveryMode_ResolvedFromWinningAllowBinding(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	// Two allow bindings cover the asset: priority 1 (summary) and priority 0
	// (inline). The repo returns priority DESC, so summary wins.
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
				ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
				Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliverySummary, Priority: 1},
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
				ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
				Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliveryInline, Priority: 0},
		},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, domain.BindingDeliverySummary, dec.DeliveryMode,
		"delivery_mode must come from the highest-priority allow binding")
}

// Test_DeliveryMode_DenyOverridesDelivery (§5.3 §8.2 UC4): an explicit deny
// covering the target denies regardless of allow scope, and the decision
// surfaces ErrNotFound (no existence leak). DeliveryMode is left empty on a
// deny — no content is delivered to a denied principal, and the decision is
// the authoritative gate (§1.2: existence never leaks, so a deny must not
// reveal which binding/delivery was evaluated).
func Test_DeliveryMode_DenyOverridesDelivery(t *testing.T) {
	ws, asset, agent, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	// A deny at higher priority than an allow → deny wins (§8.2 用例 4).
	binds := &fakeBindingRepo{binds: map[uuid.UUID][]domain.AgentBinding{
		agent: {
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
				ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
				Effect: domain.BindingDeny, DeliveryMode: domain.BindingDeliveryInline, Priority: 5},
			{ID: uuid.New(), AgentID: agent, WorkspaceID: ws,
				ScopeKind: domain.BindingScopeAsset, AssetID: &asset,
				Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliverySummary, Priority: 1},
		},
	}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{agents: map[uuid.UUID]AgentInfo{agent: {WorkspaceID: ws, Status: domain.AgentActive}}},
		binds, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectAgent, PrincipalID: agent,
		AgentID: &agent, ActingUserID: &user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	assert.ErrorIs(t, err, ErrNotFound, "explicit deny must block (no existence leak)")
	assert.False(t, dec.Allowed)
	assert.Equal(t, domain.BindingDeliveryMode(""), dec.DeliveryMode,
		"a denied decision carries no delivery contract (nothing delivered)")
}

// Test_DeliveryMode_DefaultsToToolForNonAgent: a non-agent principal has no
// binding narrowing, so DeliveryMode stays at the default tool — the
// decision is the gate and no delivery contract is owed.
func Test_DeliveryMode_DefaultsToToolForNonAgent(t *testing.T) {
	ws, asset, user := uuid.New(), uuid.New(), uuid.New()

	rbacRepo := &fakeRBACRepo{grants: []domain.Grant{workspaceUseGrant(user, ws)}}
	svc := newTestService(t, rbacRepo, publishedAsset(asset, ws, user),
		&fakeAgentRepo{}, &fakeBindingRepo{}, &fakeVersionRepo{})

	dec, err := svc.Authorize(context.Background(), AuthzRequest{
		WorkspaceID: ws, PrincipalType: domain.SubjectUser, PrincipalID: user,
		TargetType: domain.TargetAsset, TargetID: asset, Action: domain.ActionUse,
	})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, domain.BindingDeliveryTool, dec.DeliveryMode)
}
