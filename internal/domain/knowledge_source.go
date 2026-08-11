package domain

// knowledge_source.go defines the Phase 1 Source/SyncRun/SourceTarget/Relation
// value objects (design-docs/14 §2.1, §8 internal/domain/knowledge_source.go).
// These are the in-memory shapes the source module and knowledge-worker read
// and write. Persistence lives in infra/postgres/knowledge_source.go and
// asset_registry.go; no business logic here — just the data shapes.

import "time"

// SourceType is the kind of ingestion source adapter (14 §4.1).
type SourceType string

const (
	SourceFile   SourceType = "file"
	SourceURLAPI SourceType = "url_api"
	SourceGit    SourceType = "git"
)

// TrustLevel classifies a source's trust boundary (14 §2.1, §4.3). untrusted
// sources require human review before publish; trusted/internal may be
// auto-published per the governance Profile's auto_publish policy.
type TrustLevel string

const (
	TrustUntrusted TrustLevel = "untrusted"
	TrustTrusted   TrustLevel = "trusted"
	TrustInternal  TrustLevel = "internal"
)

// KnowledgeSource is an external ingestion source registered in a workspace
// (14 §2.1). uri_normalized carries NO embedded credentials — those live in
// credential_ref (§13.2).
type KnowledgeSource struct {
	ID              UUID
	WorkspaceID     UUID
	SourceType      SourceType
	Name            string
	URINormalized   string
	CredentialRef   string
	SyncPolicy      map[string]any
	TrustLevel      TrustLevel
	License         map[string]any
	CurrentRevision string
	Enabled         bool
	LastSyncedAt    *time.Time
	LastError       string
	CreatedByType   SubjectType
	CreatedByID     UUID
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// ETagVersion is the optimistic-concurrency version the repository layer
	// derives (updated_at epoch ms + revision). Not a DB column; surfaced on
	// reads so PATCH can carry it back as If-Match (§4.4).
	ETagVersion int64
}

// SyncRunStatus is the lifecycle of a source_sync_run (14 §2.1).
type SyncRunStatus string

const (
	SyncRunQueued     SyncRunStatus = "queued"
	SyncRunFetching   SyncRunStatus = "fetching"
	SyncRunProcessing SyncRunStatus = "processing"
	SyncRunReady      SyncRunStatus = "ready"
	SyncRunFailed     SyncRunStatus = "failed"
	SyncRunCancelled  SyncRunStatus = "cancelled"
)

// RequestedAssetType is the asset kind a sync run produces (14 §2.1).
type RequestedAssetType string

const (
	RequestedAssetDocument RequestedAssetType = "document"
	RequestedAssetCodebase  RequestedAssetType = "codebase"
	RequestedAssetMemory    RequestedAssetType = "memory"
	RequestedAssetSkill     RequestedAssetType = "skill"
)

// SourceSyncRun is an immutable snapshot of a single source sync attempt
// (14 §2.1). source_config_snapshot is redacted and frozen at creation so a
// later Source edit cannot drift an already-queued Run (§7.2).
type SourceSyncRun struct {
	ID                    UUID
	SourceID              UUID
	RequestedByType       SubjectType
	RequestedByID         UUID
	RequestedRevision     string
	ResolvedRevision      string
	SourceConfigSnapshot  map[string]any
	CredentialVersion     string
	GovernanceProfileID   *UUID
	RequestedAssetType    RequestedAssetType
	Status                SyncRunStatus
	Attempt               int
	IdempotencyKey        string
	StartedAt             *time.Time
	FinishedAt            *time.Time
	ErrorCode             string
	ErrorDetailRedacted   string
	CreatedAt             time.Time
}

// SourceTarget maps a Connector's stable target_key to the asset it produced
// (14 §2.1). Re-syncing the same target upserts this row; it does NOT create a
// new asset — assets are never inferred from title or URL.
type SourceTarget struct {
	SourceID   UUID
	TargetKey  string
	AssetType  AssetType
	AssetID    UUID
	Selector   map[string]any
	Active     bool
	FirstSeenAt time.Time
	UpdatedAt  time.Time
}

// RelationType is a typed edge between two assets (14 §2.1). Relations never
// cross workspaces; supersedes/contradicts must carry creation evidence.
type RelationType string

const (
	RelationDerivedFrom RelationType = "derived_from"
	RelationExplains   RelationType = "explains"
	RelationImplements RelationType = "implements"
	RelationSupersedes RelationType = "supersedes"
	RelationContradicts RelationType = "contradicts"
	RelationUses       RelationType = "uses"
	RelationRelatedTo  RelationType = "related_to"
)

// RelationOrigin is who/what asserted a relation (14 §2.1).
type RelationOrigin string

const (
	RelationOriginHuman     RelationOrigin = "human"
	RelationOriginGenerated RelationOrigin = "generated"
	RelationOriginSystem    RelationOrigin = "system"
)

// KnowledgeRelation is a directed edge between two assets (14 §2.1).
// from_asset_id and to_asset_id must share a workspace (application-enforced,
// and the workspace_id column pins it).
type KnowledgeRelation struct {
	ID            UUID
	WorkspaceID   UUID
	FromAssetID   UUID
	FromVersionID *UUID
	RelationType  RelationType
	ToAssetID     UUID
	ToVersionID   *UUID
	Origin        RelationOrigin
	Confidence    *float64
	CreatedByType SubjectType
	CreatedByID   UUID
	CreatedAt     time.Time
}
