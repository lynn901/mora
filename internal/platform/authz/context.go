package authz

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// AuthzContext is the resolved authorization context for a request (12 §5.2).
// It carries the decision outcome and the narrowed scope under which the
// principal may act. AllowedAssetIDs empty means "workspace-level visible,
// resolved server-side via decision_id" rather than "no assets" — the
// distinction matters for search/listing so large sets are not transferred.
type AuthzContext struct {
	WorkspaceID      uuid.UUID
	AuthzRevision    int64
	PrincipalType    domain.SubjectType
	PrincipalID     uuid.UUID
	ActingUserID    *uuid.UUID // present when an agent acts on behalf of a user
	AgentID         *uuid.UUID // present when principal_type=agent
	Allowed         bool
	Reason          string
	AllowedAssetIDs []uuid.UUID // empty = workspace-level (server-resolved)
	DecisionID      *uuid.UUID  // present when issued as a signed capability
}

// AuthzRequest is the input to Service.Authorize (13 §3.4).
type AuthzRequest struct {
	WorkspaceID   uuid.UUID
	PrincipalType domain.SubjectType
	PrincipalID   uuid.UUID
	ActingUserID  *uuid.UUID // agent-on-behalf-of-user
	AgentID       *uuid.UUID // when principal is an agent
	TargetType    TargetType
	TargetID      uuid.UUID
	Action        domain.Action
}

// ListScope is the input to VisibleAssets: the principal and the workspace
// whose asset set is to be filtered (存在性不泄露).
type ListScope struct {
	WorkspaceID   uuid.UUID
	PrincipalType domain.SubjectType
	PrincipalID   uuid.UUID
	ActingUserID  *uuid.UUID
	AgentID       *uuid.UUID
}

// DecisionCapability is the signed short-lived capability returned by
// IssueDecision (13 §3.4 / §4). The Token is validated by a Provider/internal
// service via audience + nonce + expiry, recorded in authorization_decisions.
type DecisionCapability struct {
	DecisionID    uuid.UUID
	Token         string // signed capability (implementation-defined)
	AuthzRevision int64
	ExpiresAt     time.Time
}

// AssetInfo is the minimal projection authz.Service needs from knowledge_assets.
// Exported so the postgres implementation in internal/infra/postgres can satisfy
// AssetRepo from outside this package.
type AssetInfo struct {
	WorkspaceID      uuid.UUID
	Status           domain.AssetStatus
	OwnerType        domain.SubjectType
	OwnerID          uuid.UUID
	CurrentVersionID *uuid.UUID
}

// AssetVersionInfo is the minimal projection authz.Service needs from
// knowledge_asset_versions to evaluate the pinned-version-revocation gate
// (§8.2 用例 5 / §11.4). Only the two status axes the gate decides on are
// carried: a version is usable iff build_status='ready' AND
// governance_status='published' (the same invariant the asset's
// current_version_id FK enforces, design-docs/12 §4.2). Any other state
// (deprecated/superseded/rejected/failed/…) counts as revoked → block use,
// no auto-fallback to the latest published version.
type AssetVersionInfo struct {
	BuildStatus      string // domain.VersionBuild* — pending|building|ready|failed|superseded
	GovernanceStatus string // domain.VersionGov*  — candidate|approved|published|rejected|deprecated
}

// IsUsable reports whether this version is in a state the gate treats as
// authorized for use: build ready AND governance published. Revoked,
// superseded, deprecated, failed or in-flight versions return false.
func (v AssetVersionInfo) IsUsable() bool {
	return v.BuildStatus == domain.VersionBuildReady &&
		v.GovernanceStatus == domain.VersionGovPublished
}

// AgentInfo is the minimal projection authz.Service needs from agents.
type AgentInfo struct {
	WorkspaceID      uuid.UUID
	Status           domain.AgentStatus
	ServiceAccountID *uuid.UUID
}

// AssetRepo is the read port authz.Service needs over knowledge_assets.
type AssetRepo interface {
	Get(ctx context.Context, assetID uuid.UUID) (AssetInfo, error)
}

// AssetVersionRepo is the read port authz.Service needs over
// knowledge_asset_versions. It backs the pinned-version-revocation gate
// (§8.2 用例 5): when a binding pins a version whose state is no longer
// usable (revoked/deprecated/superseded/…), use must block — no silent
// fallback to the latest published version (§11.4). A missing version row
// returns an error the service maps to a deny (existence never leaks).
type AssetVersionRepo interface {
	Get(ctx context.Context, versionID uuid.UUID) (AssetVersionInfo, error)
}

// AgentRepo is the read port authz.Service needs over agents.
type AgentRepo interface {
	Get(ctx context.Context, agentID uuid.UUID) (AgentInfo, error)
}

// BindingRepo returns the active agent_bindings for an agent in a workspace
// (revoked_at IS NULL), used by the decision pipeline step 3/4.
type BindingRepo interface {
	ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error)
}

// RevisionRepo reads workspace_authz_revisions (the linearization point, §5.6).
type RevisionRepo interface {
	Current(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// DecisionRepo records authorization_decisions (Provider capability, §4).
type DecisionRepo interface {
	Record(ctx context.Context, d DecisionRecord) (uuid.UUID, error)
}

// DecisionRecord is the row written to authorization_decisions.
type DecisionRecord struct {
	WorkspaceID   uuid.UUID
	AuthzRevision int64
	PrincipalType domain.SubjectType
	PrincipalID  uuid.UUID
	ActingUserID *uuid.UUID
	AgentID      *uuid.UUID
	Action       domain.Action
	ScopeHash    string
	Audience     string
	NonceHash    string
	ExpiresAt    time.Time
}
