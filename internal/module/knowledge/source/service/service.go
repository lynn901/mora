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
//   - Resource-level RBAC: every method calls rbac.Engine.Check before
//     touching the resource. The engine's CompositeLocator resolves a source
//     to its owning workspace (a missing OR disabled OR cross-workspace
//     source fails to resolve → ErrTargetNotFound), so a caller outside the
//     source's workspace has no grant and is denied (§8.5 / §10.4 用例 27).
//   - Existence never leaks: a read denial returns ErrSourceNotFound, the
//     SAME sentinel a genuinely missing resource returns, so the handler
//     surfaces 404 + 40400 indistinguishable from not-found (§8.2). A
//     write/governance denial returns ErrSourceForbidden → 403 + 40300
//     (§10.4 用例 25/29); the AuditMiddleware at the HTTP layer records the
//     denied attempt, satisfying the "+ 审计" requirement.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/outbox"
	"github.com/lynn901/mora/internal/platform/rbac"
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

// AuthContext carries the caller identity needed for RBAC + audit (mirrors
// mora/service.AuthContext). IsAdmin short-circuits the Check (an admin
// bypasses per-resource RBAC, matching the document-service pattern). An
// agent acting on behalf of a user records the user as the RBAC subject; a
// service_account caller resolves to itself with no admin bypass.
type AuthContext struct {
	SubjectType     domain.SubjectType
	PrincipalID     uuid.UUID // user (or acting user) / service account id
	GroupIDs        []uuid.UUID
	IsAdmin         bool
	IsServiceCaller bool
}

// Service is the Source management application service. It composes the
// SourceRepo, SyncRunRepo, ReviewRepo, SyncRunSink, and CredentialStore.
type Service struct {
	sources SourceRepo
	runs    SyncRunRepo
	reviews ReviewRepo
	runSink SyncRunSink
	creds   CredentialStore
	rbac    *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit   *audit.Logger
}

// NewService wires the Source management service. creds may be nil (Phase 1
// file/connector path uses file sink; SetCredential becomes a no-op store).
// rbac is nil here by design: production wiring MUST chain WithAuthz so every
// method enforces resource-level RBAC (§8.5). A nil rbac is only acceptable in
// unit tests that exercise non-authz behavior; it MUST NOT ship in main.go.
func NewService(sources SourceRepo, runs SyncRunRepo, reviews ReviewRepo, runSink SyncRunSink, creds CredentialStore) *Service {
	return &Service{sources: sources, runs: runs, reviews: reviews, runSink: runSink, creds: creds}
}

// WithAuthz injects the RBAC engine + audit logger and returns the service for
// chaining (same pattern as mora/service.DocumentService.WithSink). Once set,
// every method calls rbac.Engine.Check before touching a resource: read
// denials map to ErrSourceNotFound (no existence leak, §8.2) and write/
// governance denials map to ErrSourceForbidden (403, §10.4 用例 25/29).
func (s *Service) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *Service {
	s.rbac = engine
	s.audit = logger
	return s
}

// authorize runs an rbac.Engine.Check for a resource target and maps the
// outcome to the §8.2 no-leak / §10.4 deny contract:
//   - leak=false (a read): a denial returns ErrSourceNotFound so the caller
//     cannot tell not-found from not-allowed (existence never leaks).
//   - leak=false on a run/review: ErrRunNotFound / ErrReviewNotFound likewise.
//   - leak=true (a write/governance action): a denial returns
//     ErrSourceForbidden (→ 403, §10.4 用例 25/29). The HTTP AuditMiddleware
//     records the rejected attempt; recordDeniedAudit attributes it to the
//     resource target too.
//
// An admin (auth.IsAdmin) short-circuits to allowed, matching the
// document-service pattern. A nil rbac engine (unit tests only) also allows —
// production wiring MUST chain WithAuthz so this is never the shipped path.
func (s *Service) authorize(ctx context.Context, auth AuthContext, t domain.TargetType, id uuid.UUID, action domain.Action, leak bool) error {
	if s.rbac == nil || auth.IsAdmin {
		return nil
	}
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, t, id, action)
	if err != nil {
		// The locator returns an error only for a MISSING / disabled / cross-
		// workspace-UNRESOLVABLE target (the source/review genuinely does not
		// resolve for this caller). Map that to the not-found sentinel on BOTH
		// read and write paths — existence of such a target never leaks (§8.2
		// 用例 10 / §10.4 用例 27 missing/disabled leg). A write against a target
		// that DOES resolve but is merely denied takes the 403 branch below.
		if leak {
			// Still attribute the rejected attempt to audit; the target id is
			// caller-supplied so logging it reveals nothing the caller didn't know.
			s.recordDeniedAudit(ctx, auth, action, t, id)
		}
		return notFoundFor(t)
	}
	if !dec.Allowed {
		if leak {
			// A write/governance denial is allowed to reveal the action was
			// forbidden (the caller is authenticated and asked to mutate); record
			// the denied audit and surface 403 (§10.4 用例 25/29).
			s.recordDeniedAudit(ctx, auth, action, t, id)
			return ErrSourceForbidden
		}
		// A read denial MUST NOT leak existence — surface as not-found.
		return notFoundFor(t)
	}
	return nil
}

// notFoundFor returns the no-leak sentinel for the target kind. A source
// denial is indistinguishable from a missing source; a run/review denial
// likewise uses its own not-found sentinel.
func notFoundFor(t domain.TargetType) error {
	switch t {
	case domain.TargetReview:
		return ErrReviewNotFound
	default:
		return ErrSourceNotFound
	}
}

// recordDeniedAudit writes a best-effort denied-decision audit row attributing
// the rejected action to its target (the HTTP AuditMiddleware also records
// the request at path granularity; this enriches it with the resource). Audit
// is best-effort: a failure never blocks the denial (§5 audit invariants).
func (s *Service) recordDeniedAudit(ctx context.Context, auth AuthContext, action domain.Action, t domain.TargetType, id uuid.UUID) {
	if s.audit == nil {
		return
	}
	actorType := "user"
	if auth.IsServiceCaller {
		actorType = "service"
	}
	var actorID *uuid.UUID
	if auth.PrincipalID != uuid.Nil {
		pid := auth.PrincipalID
		actorID = &pid
	}
	tid := id
	targetType := string(t)
	s.audit.Record(ctx, actorType, actorID,
		"denied."+string(action),
		targetType, &tid,
		map[string]any{"reason": "rbac deny", "subject_type": string(auth.SubjectType)},
		"", "")
}

// CreateSource registers a new knowledge source. uri_normalized MUST already
// have embedded credentials stripped (the handler does this). A duplicate
// (workspace_id, source_type, uri_normalized) returns ErrSourceConflict → 409.
// It does NOT trigger a sync run — the caller POSTs /sync-runs explicitly
// (§4.4 separates create from trigger; a first-sync is an explicit action).
// A principal without workspace write permission is rejected (§10.4 用例 25).
func (s *Service) CreateSource(ctx context.Context, auth AuthContext, in CreateSourceInput) (*domain.KnowledgeSource, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, in.WorkspaceID, domain.ActionWrite, true); err != nil {
		return nil, err
	}
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

// GetSource loads a source by id after an RBAC read check. Existence never
// leaks: a missing source AND a read denial both surface as ErrSourceNotFound
// (§8.2 / §10.4 用例 27 cross-workspace → 404, no leak).
func (s *Service) GetSource(ctx context.Context, auth AuthContext, id uuid.UUID) (*domain.KnowledgeSource, error) {
	if err := s.authorize(ctx, auth, domain.TargetSource, id, domain.ActionRead, false); err != nil {
		return nil, err
	}
	return s.sources.Get(ctx, id)
}

// ListSources returns a cursor-paginated page (§4.4 GET /sources). The page
// is already scoped to q.WorkspaceID at the repo layer; this gates the call
// on a workspace read grant so a non-member gets an empty/forbidden result
// rather than a cross-workspace listing (§10.4 用例 27).
func (s *Service) ListSources(ctx context.Context, auth AuthContext, q SourceListQuery) ([]*domain.KnowledgeSource, string, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, q.WorkspaceID, domain.ActionRead, false); err != nil {
		return nil, "", err
	}
	return s.sources.List(ctx, q)
}

// UpdateSource applies a partial patch gated by ETag (§4.4 PATCH, If-Match).
// A mismatch returns ErrSourceConflict → 409. A principal without source
// write permission is rejected (§10.4 用例 25 → 403 + audit).
func (s *Service) UpdateSource(ctx context.Context, auth AuthContext, id uuid.UUID, etag int64, patch SourcePatch) (*domain.KnowledgeSource, error) {
	if err := s.authorize(ctx, auth, domain.TargetSource, id, domain.ActionWrite, true); err != nil {
		return nil, err
	}
	return s.sources.Update(ctx, id, etag, patch)
}

// DisableSource soft-disables a source (§4.4 DELETE). Disabled sources are
// excluded from the default list and from authorization (the SourceLocator
// surfaces a disabled source as not-found, §8.2). Requires admin on the
// source (a destructive lifecycle action).
func (s *Service) DisableSource(ctx context.Context, auth AuthContext, id uuid.UUID) error {
	if err := s.authorize(ctx, auth, domain.TargetSource, id, domain.ActionAdmin, true); err != nil {
		return err
	}
	return s.sources.Disable(ctx, id)
}

// SetCredential updates a source's credential_ref (§4.4 PUT /credentials).
// It stores the plaintext via CredentialStore (which returns only a ref +
// version) then pins the ref on the source. Plaintext is never logged,
// echoed, or persisted outside the credential store. If no CredentialStore
// is wired, the ref is stored as-is from the caller (dev / file path).
// Requires admin on the source (a credential rotation is a privileged action).
func (s *Service) SetCredential(ctx context.Context, auth AuthContext, id, workspaceID uuid.UUID, plaintext []byte, refOverride string) error {
	if err := s.authorize(ctx, auth, domain.TargetSource, id, domain.ActionAdmin, true); err != nil {
		return err
	}
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
// A principal without the `sync` action on the source is rejected with 403
// + audit (§10.4 用例 25); a cross-workspace or disabled source surfaces as
// ErrSourceNotFound (no existence leak, §10.4 用例 27/28).
//
// The run + outbox event commit atomically via the SyncRunSink — the
// knowledge-worker only ever sees a run whose event is already durably
// recorded, so a crash between create and dispatch never loses the sync.
func (s *Service) TriggerSync(ctx context.Context, auth AuthContext, in TriggerSyncInput) (*domain.SourceSyncRun, error) {
	if err := s.authorize(ctx, auth, domain.TargetSource, in.SourceID, domain.ActionSync, true); err != nil {
		return nil, err
	}
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
// (§4.4 GET /sync-runs). Gated on a source read grant; a cross-workspace
// caller gets ErrSourceNotFound (no leak, §10.4 用例 27).
func (s *Service) ListRuns(ctx context.Context, auth AuthContext, q SyncRunListQuery) ([]*domain.SourceSyncRun, string, error) {
	if err := s.authorize(ctx, auth, domain.TargetSource, q.SourceID, domain.ActionRead, false); err != nil {
		return nil, "", err
	}
	return s.runs.List(ctx, q)
}

// GetRun loads a sync run by id (no existence leak — ErrRunNotFound). Gated
// on a read grant over the run's source: the engine resolves the run via
// its source's workspace, so a cross-workspace caller gets ErrRunNotFound
// (no leak, §10.4 用例 27).
func (s *Service) GetRun(ctx context.Context, auth AuthContext, id uuid.UUID) (*domain.SourceSyncRun, error) {
	run, err := s.runs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// The run carries only its source_id; authorize against that source so the
	// engine resolves the source's workspace + enabled state (a cross-workspace
	// or disabled source fails to resolve → ErrSourceNotFound, no leak).
	if err := s.authorize(ctx, auth, domain.TargetSource, run.SourceID, domain.ActionRead, false); err != nil {
		return nil, err
	}
	return run, nil
}

// ListPendingReviews returns pending review_requests for a workspace
// (§4.4 GET /reviews?status=pending). Cursor-paginated. Gated on a
// workspace read grant; the repo already filters by workspace_id, so a
// non-member gets an empty result, never another workspace's reviews.
func (s *Service) ListPendingReviews(ctx context.Context, auth AuthContext, workspaceID uuid.UUID, cursor string, pageSize int) ([]*domain.ReviewRequest, string, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionRead, false); err != nil {
		return nil, "", err
	}
	return s.reviews.ListPending(ctx, workspaceID, cursor, pageSize)
}

// AppendReviewDecision appends an immutable review_decision + projects the
// request status (§4.4 POST /reviews/{id}/decisions). Decisions are
// append-only; the request's status reflects the latest decision. The
// idempotency-key is enforced at the service layer: a duplicate key for the
// same decision is a no-op return of success (idempotent retry).
//
// Gated on the `review` action over the review target: a principal whose
// role is not in the governance review_roles is rejected with 403 + audit
// (§10.4 用例 29). The engine resolves the review to its workspace (a
// cross-workspace caller gets ErrReviewNotFound, no leak).
func (s *Service) AppendReviewDecision(ctx context.Context, auth AuthContext, in ReviewDecisionInput) error {
	if err := s.authorize(ctx, auth, domain.TargetReview, in.ReviewRequestID, domain.ActionReview, true); err != nil {
		return err
	}
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
