package domain

import (
	"errors"
	"time"
)

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
	BuildStatus                string // VersionBuild* — pending|building|ready|failed|superseded
	GovernanceStatus           string // VersionGov* — candidate|approved|published|rejected|deprecated
	ActivationPolicySnapshot   map[string]any
	ApprovedByType             string
	ApprovedByID               *UUID
	ApprovedAt                *time.Time
	CreatedByType              SubjectType
	CreatedByID                UUID
	CreatedAt                  time.Time
}

// Version build_status values (design-docs/12 §4.2 knowledge_asset_versions).
// A version is build-complete only at 'ready'; every other state is treated
// as not-yet-usable by the authz lifecycle gate.
const (
	VersionBuildPending   = "pending"
	VersionBuildBuilding  = "building"
	VersionBuildReady     = "ready"
	VersionBuildFailed    = "failed"
	VersionBuildSuperseded = "superseded"
)

// Version governance_status values (design-docs/12 §4.2). 'published' is the
// only governance state the authz lifecycle gate treats as authorized; a
// version that has been deprecated or rejected counts as revoked.
const (
	VersionGovCandidate  = "candidate"
	VersionGovApproved   = "approved"
	VersionGovPublished  = "published"
	VersionGovRejected   = "rejected"
	VersionGovDeprecated  = "deprecated"
)

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

// --- §7 CAS activation sentinel errors (design-docs/14 §7) ---
//
// Activate is the async counterpart to the synchronous registration path: a
// connector-sourced version starts candidate/pending, projections build, then
// the worker CAS-activates current_version_id once build_status='ready' AND
// governance_status='published'. The CAS is the final authority — these
// sentinels let the worker classify the outcome (retry vs dead) without
// re-reading state. They are sentinel values, not wrapped DB errors.

var (
	// ErrCASVersionStale: the version's version_no is behind
	// latest_requested_version_no (a newer version was requested after this
	// build started). The barrier rejected the CAS — the old version is marked
	// ready but current_version_id is NOT switched (§7 失败不覆盖). Permanent:
	// retrying won't change the barrier.
	ErrCASVersionStale = errors.New("asset: CAS version stale (newer version requested)")
	// ErrCASExpectedMismatch: the current_version_id the caller expected
	// (expected_current) does not match the row's actual current_version_id — a
	// concurrent activation already moved the pointer. Permanent: the CAS has
	// already decided; retrying races again.
	ErrCASExpectedMismatch = errors.New("asset: CAS expected_current mismatch (concurrent activation)")
	// ErrProjectionsNotReady: at least one required projection is not yet
	// 'ready' for this version. Transient: projections may still be building;
	// the worker retries the activation job until all required projections land.
	ErrProjectionsNotReady = errors.New("asset: required projections not ready")
	// ErrNotPublished: the version's governance_status is not 'published' —
	// governance has not approved it. Permanent: governance state won't change
	// by retrying the activation job; it changes via a review decision.
	ErrNotPublished = errors.New("asset: version not published (governance pending)")
	// ErrAssetVersionNotFound: the asset or version id does not resolve.
	// Permanent: the job references an id that no longer exists.
	ErrAssetVersionNotFound = errors.New("asset: asset/version not found")
)

// ReconcileReport is the outcome of a §3.3 reconcile scan for one workspace.
// It tallies the divergences the background ticker found and CAS-repaired:
// the CAS gate is self-healing — a crashed activation that left
// current_version_id unset is repaired to the latest ready+published version,
// and stuck projections (building/pending past a deadline) are re-queued.
type ReconcileReport struct {
	WorkspaceID        UUID
	VersionCASFixed     int // current_version_id repaired (was unset/stale)
	ProjectionsQueued   int // stuck building/pending projections re-queued
	ProjectionsStaled   int // superseded-version projections marked stale
	NeedsHuman          int // versions ready but not published → review inbox
}
