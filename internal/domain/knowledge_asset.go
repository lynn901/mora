package domain

import "time"

// Phase 0 knowledge-asset value objects (design-docs/13 §4.2–§4.3, §7).
// These are the in-memory value objects the authz layer reads to evaluate
// lifecycle gates and agent bindings. Persistence lives in infra/postgres
// (knowledge_core.go). No business logic here — just the data shapes.

// AssetStatus is the lifecycle state of a knowledge asset.
type AssetStatus string

const (
	AssetDraft     AssetStatus = "draft"
	AssetReviewing AssetStatus = "reviewing"
	AssetPublished AssetStatus = "published"
	AssetDeprecated AssetStatus = "deprecated"
	AssetArchived  AssetStatus = "archived"
	AssetRejected  AssetStatus = "rejected"
)

// AssetVisibility is the default visibility of an asset before bindings narrow it.
type AssetVisibility string

const (
	AssetPrivate  AssetVisibility = "private"
	AssetWorkspace AssetVisibility = "workspace"
	AssetPublic    AssetVisibility = "public"
)

// AssetType is the kind of knowledge asset.
type AssetType string

const (
	AssetTypeDocument AssetType = "document"
	AssetTypeCodebase AssetType = "codebase"
	AssetTypeMemory   AssetType = "memory"
	AssetTypeSkill    AssetType = "skill"
)

// KnowledgeAsset is the top-level asset aggregate (§4.2).
type KnowledgeAsset struct {
	ID                       UUID
	WorkspaceID              UUID
	AssetType                AssetType
	Name                     string
	Description              string
	OwnerType                SubjectType
	OwnerID                 UUID
	Status                   AssetStatus
	Visibility              AssetVisibility
	GovernanceProfileID      *UUID
	NativeDocumentID         *UUID
	CurrentVersionID         *UUID
	LatestRequestedVersionNo int64
	Confidence              *float64
	ValidFrom               *time.Time
	ExpiresAt               *time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// AssetVersion is an immutable version snapshot of an asset (§4.2).
type AssetVersion struct {
	ID                         UUID
	AssetID                    UUID
	VersionNo                  int64
	SourceID                   *UUID
	SourceRevision             string
	NativeDocumentVersionID    *UUID
	ContentOrigin              string // human|imported|generated|system
	GenerationRef              map[string]any
	ProviderRef                map[string]any
	ContentHash                string
	DedupeKey                  string
	BuildStatus                string // pending|building|succeeded|failed|dead
	GovernanceStatus           string // candidate|approved|rejected|superseded
	ActivationPolicySnapshot   map[string]any
	ApprovedByType             string
	ApprovedByID               *UUID
	ApprovedAt                *time.Time
	CreatedByType              SubjectType
	CreatedByID                UUID
	CreatedAt                  time.Time
}

// AgentStatus is the lifecycle state of an agent.
type AgentStatus string

const (
	AgentActive    AgentStatus = "active"
	AgentSuspended AgentStatus = "suspended"
	AgentRevoked   AgentStatus = "revoked"
)

// Agent is an AI agent principal in a workspace (§4.3).
type Agent struct {
	ID               UUID
	WorkspaceID     UUID
	Name            string
	Description     string
	OwnerID         UUID
	Status          AgentStatus
	RuntimeType     string
	ServiceAccountID *UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// BindingScopeKind is the scope shape of an agent binding.
type BindingScopeKind string

const (
	BindingScopeAsset     BindingScopeKind = "asset"
	BindingScopeWorkspace BindingScopeKind = "workspace"
	BindingScopeAssetType BindingScopeKind = "asset_type"
)

// BindingEffect is allow/deny for an agent binding.
type BindingEffect string

const (
	BindingAllow BindingEffect = "allow"
	BindingDeny  BindingEffect = "deny"
)

// BindingVersionPolicy is how a binding selects the asset version.
type BindingVersionPolicy string

const (
	BindingFollowPublished BindingVersionPolicy = "follow_published"
	BindingPinned          BindingVersionPolicy = "pinned"
)

// BindingDeliveryMode is how an asset is delivered to an agent.
type BindingDeliveryMode string

const (
	BindingDeliveryTool    BindingDeliveryMode = "tool"
	BindingDeliverySummary BindingDeliveryMode = "summary"
	BindingDeliveryInline  BindingDeliveryMode = "inline"
)

// AgentBinding binds an agent to a scope (asset/workspace/asset_type) with
// allow/deny and version policy (§4.3). A binding can only NARROW an agent's
// capability, never grant what the acting principal lacks (不变量 A #4).
type AgentBinding struct {
	ID              UUID
	AgentID         UUID
	WorkspaceID     UUID
	ScopeKind       BindingScopeKind
	AssetID         *UUID
	AssetType       *AssetType
	Effect          BindingEffect
	VersionPolicy   BindingVersionPolicy
	PinnedVersionID *UUID
	DeliveryMode    BindingDeliveryMode
	Priority        int
	CreatedBy       *UUID
	CreatedAt       time.Time
	RevokedAt       *time.Time
}
