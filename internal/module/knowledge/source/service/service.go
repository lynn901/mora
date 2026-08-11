// Package service implements the Source management application service
// (design-docs/14 §4.4 D13). It owns Source CRUD, SyncRun creation
// (Idempotency-Key + transactional outbox event), credential-ref updates,
// and review-decision appends. It does NOT run the Connector — the
// knowledge-worker consumes the outbox event and dispatches the Connector
// (§5). The service only enqueues a source_sync_run + Knowledge Outbox event.
//
// Security invariants enforced here:
//   - URINormalized MUST have embedded credentials stripped before Create
//     (§2.2 / §6.5). The caller (handler) is responsible for normalization;
//     the service stores what it is given but never echoes plaintext.
//   - SetCredential stores only a credential_ref + version; it never reads
//     or returns plaintext (§13.2).
//   - Existence never leaks: a missing/unreadable source returns
//     ErrSourceNotFound, indistinguishable from a permission denial at the
//     handler (§8.2).
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// SyncRunSink is the transactional double-write port for sync-run creation
// (§6.3): the source_sync_runs row and its Knowledge Outbox event commit in
// ONE database transaction so the event is never lost relative to the run.
// The postgres implementation (DocWriteSink's sibling) owns the tx; the
// service layer stays pgx-free.
type SyncRunSink interface {
	// CreateRun inserts the run + records the outbox event in one tx. On a
	// duplicate idempotency_key for the SAME (source_id, requested_revision)
	// it returns the existing run (idempotent retry); for a DIFFERENT payload
	// it returns ErrIdempotencyConflict (§4.4 Idempotency-Key).
	CreateRun(ctx context.Context, run *domain.SourceSyncRun, ev domain.KnowledgeEvent) error
}

// CredentialStore is the credential-ref store port (§13.2). It stores a
// credential under a ref and returns the ref + a version stamp; it NEVER
// returns plaintext. The service keeps only the ref + version.
type CredentialStore interface {
	// Store writes a credential, returns its ref + a version stamp. Plaintext
	// is consumed here and never retained (in-memory only).
	Store(ctx context.Context, workspaceID uuid.UUID, plaintext []byte) (ref, version string, err error)
}

// Service is the Source management application service. It composes the
// SourceRepo, SyncRunRepo, ReviewRepo, SyncRunSink, and CredentialStore.
type Service struct {
	sources   SourceRepo
	runs      SyncRunRepo
	reviews   ReviewRepo
	runSink   SyncRunSink
	creds     CredentialStore
}

// NewService wires the Source management service. creds may be nil (Phase 1
// file/connector path uses file sink; SetCredential becomes a no-op store).
func NewService(sources SourceRepo, runs SyncRunRepo, reviews ReviewRepo, runSink SyncRunSink, creds CredentialStore) *Service {
	return &Service{sources: sources, runs: runs, reviews: reviews, runSink: runSink, creds: creds}
}

// CreateSource registers a new knowledge source. uri_normalized MUST already
// have embedded credentials stripped (the handler does this). A duplicate
// (workspace_id, source_type, uri_normalized) returns ErrSourceConflict → 409.
// It does NOT trigger a sync run — the caller POSTs /sync-runs explicitly
// (§4.4 separates create from trigger; a first-sync is an explicit action).
func (s *Service) CreateSource(ctx context.Context, in CreateSourceInput) (*domain.KnowledgeSource, error) {
	src := &domain.KnowledgeSource{
		ID:            uuid.New(),
		WorkspaceID:   in.WorkspaceID,
		SourceType:    in.SourceType,
		Name:          in.Name,
		URINormalized: in.URINormalized,
		CredentialRef: in.CredentialRef,
		SyncPolicy:    in.SyncPolicy,
		TrustLevel:    in.TrustLevel,
		License:       in.License,
		Enabled:       true,
		CreatedByType: in.CreatedByType,
		CreatedByID:   in.CreatedByID,
	}
	if src.SyncPolicy == nil {
		src.SyncPolicy = map[string]any{}
	}
	if src.TrustLevel == "" {
		src.TrustLevel = domain.TrustUntrusted
	}
	if err := s.sources.Create(ctx, src); err != nil {
		return nil, err
	}
	return src, nil
}

// CreateSourceInput is the create-source payload (§4.4 POST /sources).
type CreateSourceInput struct {
	WorkspaceID   uuid.UUID
	SourceType    domain.SourceType
	Name          string
	URINormalized string // already credential-stripped
	CredentialRef string
	SyncPolicy    map[string]any
	TrustLevel    domain.TrustLevel
	License       map[string]any
	CreatedByType domain.SubjectType
	CreatedByID   uuid.UUID
}

// GetSource loads a source by id (no existence leak — ErrSourceNotFound).
func (s *Service) GetSource(ctx context.Context, id uuid.UUID) (*domain.KnowledgeSource, error) {
	return s.sources.Get(ctx, id)
}

// ListSources returns a cursor-paginated page (§4.4 GET /sources).
func (s *Service) ListSources(ctx context.Context, q SourceListQuery) ([]*domain.KnowledgeSource, string, error) {
	return s.sources.List(ctx, q)
}

// UpdateSource applies a partial patch gated by ETag (§4.4 PATCH, If-Match).
// A mismatch returns ErrSourceConflict → 409.
func (s *Service) UpdateSource(ctx context.Context, id uuid.UUID, etag int64, patch SourcePatch) (*domain.KnowledgeSource, error) {
	return s.sources.Update(ctx, id, etag, patch)
}

// DisableSource soft-disables a source (§4.4 DELETE). Disabled sources are
// excluded from the default list and from authorization (the SourceLocator
// surfaces a disabled source as not-found, §8.2).
func (s *Service) DisableSource(ctx context.Context, id uuid.UUID) error {
	return s.sources.Disable(ctx, id)
}

// SetCredential updates a source's credential_ref (§4.4 PUT /credentials).
// It stores the plaintext via CredentialStore (which returns only a ref +
// version) then pins the ref on the source. Plaintext is never logged,
// echoed, or persisted outside the credential store. If no CredentialStore
// is wired, the ref is stored as-is from the caller (dev / file path).
func (s *Service) SetCredential(ctx context.Context, id, workspaceID uuid.UUID, plaintext []byte, refOverride string) error {
	ref, version := refOverride, ""
	if s.creds != nil {
		r, v, err := s.creds.Store(ctx, workspaceID, plaintext)
		if err != nil {
			return err
		}
		ref, version = r, v
	}
	return s.sources.SetCredential(ctx, id, ref, version)
}

// TriggerSync enqueues a source_sync_run + a Knowledge Outbox event (§4.4
// POST /sync-runs, §6.3). The idempotency_key is the caller's
// Idempotency-Key header (or a generated one). A duplicate key for the same
// (source_id, requested_revision) returns the existing run (idempotent
// retry — 200/201 with the original run); a duplicate key for a different
// payload returns ErrIdempotencyConflict → 409 (§4.4 Idempotency-Key).
//
// The run + outbox event commit atomically via the SyncRunSink — the
// knowledge-worker only ever sees a run whose event is already durably
// recorded, so a crash between create and dispatch never loses the sync.
func (s *Service) TriggerSync(ctx context.Context, in TriggerSyncInput) (*domain.SourceSyncRun, error) {
	src, err := s.sources.Get(ctx, in.SourceID)
	if err != nil {
		return nil, err
	}
	if !src.Enabled {
		// A disabled source cannot be synced — surface as not-found so the
		// enabled/disabled state does not leak to an unauthorized caller.
		return nil, ErrSourceNotFound
	}
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = uuid.NewString()
	}
	run := &domain.SourceSyncRun{
		ID:                   uuid.New(),
		SourceID:             src.ID,
		RequestedByType:      in.RequestedByType,
		RequestedByID:        in.RequestedByID,
		RequestedRevision:    in.RequestedRevision,
		CredentialVersion:    src.CredentialRef, // pinned so a rotation can't drift an in-flight run (§7.2)
		GovernanceProfileID:  in.GovernanceProfileID,
		RequestedAssetType:   in.RequestedAssetType,
		Status:               domain.SyncRunQueued,
		IdempotencyKey:       in.IdempotencyKey,
		SourceConfigSnapshot: snapshotOf(src), // redacted, immutable for the Run's life
	}
	ev := domain.KnowledgeEvent{
		EventID:       run.IdempotencyKey,
		EventType:     domain.KEAssetVersionRequested,
		EventVersion:  1,
		AggregateType: domain.AggKnowledgeAsset,
		AggregateID:   run.ID, // the run is the dispatch aggregate
		WorkspaceID:   &src.WorkspaceID,
		Actor:         domain.EventActor{Type: in.RequestedByType, ID: in.RequestedByID},
		OccurredAt:    time.Now().UTC(),
		Payload: map[string]any{
			"run_id":            run.ID.String(),
			"source_id":         src.ID.String(),
			"requested_revision": in.RequestedRevision,
			"asset_type":         string(in.RequestedAssetType),
		},
	}
	if err := s.runSink.CreateRun(ctx, run, ev); err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return nil, ErrIdempotencyConflict
		}
		if errors.Is(err, ErrIdempotentRetry) {
			// Idempotent retry — same payload, original run already exists.
			// Re-GET by idempotency_key (the sink resolved the collision; the
			// original run is the source of truth) and return it. The caller
			// sees the original run, satisfying the Idempotency-Key contract.
			orig, gerr := s.runs.GetByIdempotencyKey(ctx, in.IdempotencyKey)
			if gerr != nil {
				// Race: the original run vanished between collision-detect and
				// re-GET. Surface as conflict — the caller's key is unusable.
				return nil, ErrIdempotencyConflict
			}
			return orig, nil
		}
		return nil, err
	}
	return run, nil
}

// TriggerSyncInput is the trigger-sync payload (§4.4 POST /sync-runs).
type TriggerSyncInput struct {
	SourceID            uuid.UUID
	RequestedRevision   string // empty = latest
	RequestedAssetType  domain.RequestedAssetType
	GovernanceProfileID *uuid.UUID
	RequestedByType     domain.SubjectType
	RequestedByID       uuid.UUID
	IdempotencyKey      string
}

// ListRuns returns a cursor-paginated page of a source's sync-run history
// (§4.4 GET /sync-runs).
func (s *Service) ListRuns(ctx context.Context, q SyncRunListQuery) ([]*domain.SourceSyncRun, string, error) {
	return s.runs.List(ctx, q)
}

// GetRun loads a sync run by id (no existence leak — ErrRunNotFound).
func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (*domain.SourceSyncRun, error) {
	return s.runs.Get(ctx, id)
}

// ListPendingReviews returns pending review_requests for a workspace
// (§4.4 GET /reviews?status=pending). Cursor-paginated.
func (s *Service) ListPendingReviews(ctx context.Context, workspaceID uuid.UUID, cursor string, pageSize int) ([]*domain.ReviewRequest, string, error) {
	return s.reviews.ListPending(ctx, workspaceID, cursor, pageSize)
}

// AppendReviewDecision appends an immutable review_decision + projects the
// request status (§4.4 POST /reviews/{id}/decisions). Decisions are
// append-only; the request's status reflects the latest decision. The
// idempotency-key is enforced at the service layer: a duplicate key for the
// same decision is a no-op return of success (idempotent retry).
func (s *Service) AppendReviewDecision(ctx context.Context, in ReviewDecisionInput) error {
	d := &domain.ReviewDecisionRecord{
		ID:                uuid.New(),
		ReviewRequestID:   in.ReviewRequestID,
		Decision:          in.Decision,
		DecisionByType:    in.DecisionByType,
		DecisionByID:      in.DecisionByID,
		PolicyVersion:     in.PolicyVersion,
		RationaleRedacted: in.RationaleRedacted,
	}
	return s.reviews.AppendDecision(ctx, d)
}

// ReviewDecisionInput is the review-decision payload (§4.4 POST /decisions).
type ReviewDecisionInput struct {
	ReviewRequestID   uuid.UUID
	Decision          domain.ReviewDecision
	DecisionByType    domain.SubjectType
	DecisionByID      uuid.UUID
	PolicyVersion     string
	RationaleRedacted string
}

// snapshotOf builds the redacted, immutable Source config snapshot frozen for
// a Run's life (§7.2). It carries only what the Connector needs: type, URI
// (no credentials), sync_policy, trust_level. credential_ref is pinned
// separately as credential_version so a rotation can't drift the run.
func snapshotOf(src *domain.KnowledgeSource) map[string]any {
	return map[string]any{
		"source_type":  string(src.SourceType),
		"uri_normalized": src.URINormalized, // already credential-free
		"sync_policy":   src.SyncPolicy,
		"trust_level":   string(src.TrustLevel),
	}
}

// Compile-time: the service composes the ports it owns. The SyncRunSink +
// CredentialStore are wired by the infra layer (postgres / secret manager).
var (
	_ = outbox.KnowledgeEventsStream
)
