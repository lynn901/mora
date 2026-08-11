// Package service defines the Source management ports and errors for the
// Phase 1 knowledge module (design-docs/14 §4.4). The service layer depends on
// these ports; the postgres implementations live in internal/infra/postgres.
//
// The Source module owns: Source CRUD, SyncRun creation (Idempotency-Key), and
// the cursor-paginated list over sources/sync-runs. It does NOT own the
// Connector dispatch (that is the knowledge-worker's job, §5) — the service
// only enqueues a source_sync_run + outbox event.
package service

import (
	"context"
	"errors"

	"github.com/lynn901/mora/internal/domain"
)

// Sentinel errors. Repositories return these so the service + handler can map
// them to the §11.4 error envelope without leaking existence (§8.2):
//   - ErrSourceNotFound: missing/unreadable source → 404 + 40400 (no leak).
//   - ErrRunNotFound: missing/unreadable sync run → 404 + 40400.
//   - ErrSourceConflict: ETag mismatch / concurrent update → 409 + 40900.
//   - ErrIdempotencyConflict: Idempotency-Key already used by a different body
//     → 409 + 40900 (§4.4 Idempotency-Key).
//   - ErrIdempotentRetry: Idempotency-Key used again with the SAME payload;
//     not an error at all — the sink signals it so the service can re-GET and
//     return the original run (idempotent retry, §4.4).
var (
	ErrSourceNotFound       = errors.New("source: not found")
	ErrRunNotFound          = errors.New("source: sync run not found")
	ErrSourceConflict       = errors.New("source: etag conflict")
	ErrIdempotencyConflict  = errors.New("source: idempotency-key conflict")
	ErrIdempotentRetry      = errors.New("source: idempotent retry")
	ErrReviewNotFound       = errors.New("source: review not found")
)

// SourceListQuery is the cursor-paginated list filter (§4.4 GET /sources).
// Cursor is opaque (updated_at + id encoded); PageSize bounds the page.
type SourceListQuery struct {
	WorkspaceID domain.UUID
	Cursor      string
	PageSize    int
	SourceType  string // empty = all
	Enabled     *bool  // nil = all
}

// SyncRunListQuery is the cursor-paginated run-history filter (§4.4).
type SyncRunListQuery struct {
	SourceID domain.UUID
	Cursor   string
	PageSize int
	Status   string // empty = all
}

// SourceRepo is the persistence port over knowledge_sources (§4.4). It is the
// only write path for Source rows; the authz SourceLocator reads via the same
// GetWorkspace so existence does not leak.
type SourceRepo interface {
	// Create inserts a source. Idempotency: caller checks uri uniqueness first.
	Create(ctx context.Context, s *domain.KnowledgeSource) error
	// Get loads a source by id (does NOT leak existence — returns ErrSourceNotFound).
	Get(ctx context.Context, id domain.UUID) (*domain.KnowledgeSource, error)
	// GetWorkspace returns the source's workspace_id + enabled flag. Used by
	// the authz SourceLocator and the cross-workspace guard. A missing source
	// returns ErrSourceNotFound so callers surface 404 (no existence leak).
	GetWorkspace(ctx context.Context, id domain.UUID) (domain.UUID, bool, error)
	// List returns a cursor-paginated page of sources.
	List(ctx context.Context, q SourceListQuery) ([]*domain.KnowledgeSource, string, error)
	// Update applies a partial update gated by ETag (If-Match). Returns the
	// updated source with a fresh ETagVersion. An ETag mismatch returns
	// ErrSourceConflict.
	Update(ctx context.Context, id domain.UUID, etag int64, patch SourcePatch) (*domain.KnowledgeSource, error)
	// Disable soft-deletes a source (enabled=false). §4.4 DELETE.
	Disable(ctx context.Context, id domain.UUID) error
	// SetCredential updates credential_ref only (§4.4 PUT /credentials).
	// Never reads or returns plaintext; callers pass a ref + version.
	SetCredential(ctx context.Context, id domain.UUID, ref, version string) error
}

// SourcePatch is the partial-update payload for PATCH /sources/{id} (§4.4).
// Only sync_policy / trust_level / license / name / enabled may change here;
// URI + source_type are immutable (a new source is a new row). Credential_ref
// goes through PUT /credentials.
type SourcePatch struct {
	Name       *string
	SyncPolicy map[string]any
	TrustLevel *domain.TrustLevel
	License    map[string]any
	Enabled    *bool
}

// SyncRunRepo is the persistence port over source_sync_runs (§4.4). Runs are
// immutable once created except for status transitions (queued → fetching →
// processing → ready/failed/cancelled).
type SyncRunRepo interface {
	// Create inserts a run. The idempotency_key is UNIQUE; a duplicate key for
	// a DIFFERENT (source_id, requested_revision) returns ErrIdempotencyConflict
	// (§4.4 Idempotency-Key). A duplicate key for the SAME inputs returns the
	// existing run (idempotent retry).
	Create(ctx context.Context, r *domain.SourceSyncRun) error
	// Get loads a run by id.
	Get(ctx context.Context, id domain.UUID) (*domain.SourceSyncRun, error)
	// GetByIdempotencyKey loads a run by its idempotency_key. Used by the
	// service to satisfy an idempotent retry (§4.4) — the original run is the
	// source of truth. A missing key returns ErrRunNotFound.
	GetByIdempotencyKey(ctx context.Context, key string) (*domain.SourceSyncRun, error)
	// List returns a cursor-paginated page of runs for a source.
	List(ctx context.Context, q SyncRunListQuery) ([]*domain.SourceSyncRun, string, error)
	// UpdateStatus transitions a run's status. used by the knowledge-worker.
	UpdateStatus(ctx context.Context, id domain.UUID, status domain.SyncRunStatus, resolvedRevision string, errCode, errDetail string) error
}

// SourceTargetRepo is the persistence port over knowledge_source_targets
// (§4.2). Upserted on each sync — the asset is not recreated when the same
// target_key re-syncs.
type SourceTargetRepo interface {
	// Upsert inserts or updates a target → asset mapping. Active stays true.
	Upsert(ctx context.Context, t domain.SourceTarget) error
	// ListBySource returns active targets for a source.
	ListBySource(ctx context.Context, sourceID domain.UUID) ([]domain.SourceTarget, error)
}

// ReviewRepo is the persistence port over review_requests + review_decisions
// (§4.2, §4.4). Decisions are append-only.
type ReviewRepo interface {
	// CreateRequest inserts a pending review_request.
	CreateRequest(ctx context.Context, r *domain.ReviewRequest) error
	// GetRequest loads a review request by id.
	GetRequest(ctx context.Context, id domain.UUID) (*domain.ReviewRequest, error)
	// ListPending returns pending review_requests for a workspace (cursor-paginated).
	ListPending(ctx context.Context, workspaceID domain.UUID, cursor string, pageSize int) ([]*domain.ReviewRequest, string, error)
	// AppendDecision adds an immutable review_decision + projects the request
	// status. Idempotency-Key enforcement is at the service layer.
	AppendDecision(ctx context.Context, d *domain.ReviewDecisionRecord) error
	// GetWorkspace returns the review's workspace_id (for the authz ReviewLocator).
	GetWorkspace(ctx context.Context, id domain.UUID) (domain.UUID, error)
}

// ProjectionRepo is the persistence port over asset_projections (§4.5). Phase 1
// minimal: upsert + read for the activation gate (§7).
type ProjectionRepo interface {
	Upsert(ctx context.Context, p domain.AssetProjection) error
	ListByVersion(ctx context.Context, versionID domain.UUID) ([]domain.AssetProjection, error)
}
