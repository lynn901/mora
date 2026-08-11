package domain

// projection.go defines the Phase 1 AssetProjection value object
// (design-docs/14 §2.1 asset_projections, §8 internal/domain/projection.go).
// A projection is the materialized, queryable form of an asset version for one
// kind of retrieval (fts / vector / summary / codegraph / relation). The
// (asset_version_id, projection_kind, build_revision) triple is unique — a
// rebuild produces a new build_revision row, it never overwrites in place.

import "time"

// ProjectionKind is the retrieval modality a projection serves (14 §2.1).
type ProjectionKind string

const (
	ProjectionFts        ProjectionKind = "fts"
	ProjectionVector     ProjectionKind = "vector"
	ProjectionSummary    ProjectionKind = "summary"
	ProjectionCodegraph  ProjectionKind = "codegraph"
	ProjectionRelation   ProjectionKind = "relation"
)

// ProjectionStatus is the build state of a projection row (14 §2.1).
type ProjectionStatus string

const (
	ProjectionPending  ProjectionStatus = "pending"
	ProjectionBuilding ProjectionStatus = "building"
	ProjectionReady    ProjectionStatus = "ready"
	ProjectionFailed   ProjectionStatus = "failed"
	ProjectionStale    ProjectionStatus = "stale"
)

// AssetProjection is a single materialized projection of an asset version
// (14 §2.1). locator holds non-executable placement info (Qdrant collection /
// point filter, FTS table, MinIO key prefix) — never content. For native
// document assets, the FTS/vector projections are the existing documents FTS
// index / Qdrant points, recorded here for reconciliation, not duplicated.
type AssetProjection struct {
	ID              UUID
	AssetVersionID  UUID
	ProjectionKind  ProjectionKind
	Provider        string
	ProviderVersion string
	BuildRevision   string
	Status          ProjectionStatus
	Locator         map[string]any
	BuiltAt         *time.Time
	LastError       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
