package postgres

// memory_publish_sink.go implements the transactional boundary for manual
// memory publish (design-docs/18 §6.2, decision D7; 附录 A 不变量 8/9).
//
// The inbox/publish service composes three ports to publish a candidate memory
// unit as a team Memory:
//
//   - ReviewGate (review_requests + review_decisions): the publish gate. A
//     published unit REQUIRES a review_decision — first version has NO
//     auto-publish (附录 A 不变量 9). MemoryReviewGate wraps the existing
//     ReviewRepo so the memory module does not import the source package.
//   - MemoryAssetVersionSink (knowledge_asset_versions + asset_projections +
//     memory_units): the atomic publish. PublishUnit creates the memory asset
//     version (governance_status='published', build_status='ready'), writes the
//     FTS asset_projection (status='ready', locator non-executable), stamps the
//     unit's asset_version_id + state='published', and records the
//     review_decision — all in one tx.
//
// Publish NEVER writes a permissions(target_type='evidence') row (附录 A
// 不变量 8): the Evidence ACL stays independent of Memory publish. The sink
// touches memory_units / knowledge_asset_versions / asset_projections /
// review_requests / review_decisions only — never the permissions table.
//
// All SQL is parameterized — no string-concatenated user input (07-security
// §10).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryReviewGate wraps the existing ReviewRepo to satisfy evidence.ReviewGate.
// It is a thin adapter so the memory inbox service depends on its own port,
// not the source package's ReviewRepo. The wrapped repo owns the
// review_requests/review_decisions SQL.
type MemoryReviewGate struct{ inner ReviewPort }

// ReviewPort is the subset of source.ReviewRepo the gate needs. Defined here
// to avoid importing the source package; the concrete ReviewRepo satisfies it.
type ReviewPort interface {
	CreateRequest(ctx context.Context, r *domain.ReviewRequest) error
	AppendDecision(ctx context.Context, d *domain.ReviewDecisionRecord) error
}

// NewMemoryReviewGate builds an evidence.ReviewGate over an existing review repo.
func NewMemoryReviewGate(inner ReviewPort) evidence.ReviewGate {
	return &MemoryReviewGate{inner: inner}
}

// CreateRequest delegates to the wrapped repo.
func (g *MemoryReviewGate) CreateRequest(ctx context.Context, req *domain.ReviewRequest) error {
	return g.inner.CreateRequest(ctx, req)
}

// AppendDecision delegates to the wrapped repo.
func (g *MemoryReviewGate) AppendDecision(ctx context.Context, d *domain.ReviewDecisionRecord) error {
	return g.inner.AppendDecision(ctx, d)
}

// Compile-time check.
var _ evidence.ReviewGate = (*MemoryReviewGate)(nil)

// MemoryPublishSink implements evidence.MemoryAssetVersionSink.
type MemoryPublishSink struct {
	db     *DB
	gate   evidence.ReviewGate
}

// NewMemoryPublishSink builds the publish sink over a pool + the review gate.
func NewMemoryPublishSink(db *DB, gate evidence.ReviewGate) *MemoryPublishSink {
	return &MemoryPublishSink{db: db, gate: gate}
}

// Compile-time check.
var _ evidence.MemoryAssetVersionSink = (*MemoryPublishSink)(nil)

// PublishUnit creates the memory asset version + FTS projection + stamps the
// unit published + records the review_decision, all in one tx (§6.2).
//
// The FTS projection row (status='ready') records that full-text retrieval is
// available for this published memory version; its locator is non-executable
// (the memory_units.statement column is the FTS corpus — recall builds the GIN
// query, the projection row records readiness + the provider that built it).
// The actual GIN index for memory recall is the §8.1 recall path's concern;
// publish only flips the unit to 'published' and stamps the projection.
//
// The sink does NOT touch permissions(target_type='evidence') — Evidence ACL
// stays independent (附录 A 不变量 8).
func (s *MemoryPublishSink) PublishUnit(ctx context.Context, req evidence.PublishUnitRequest) (uuid.UUID, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	// 1. Create the knowledge_asset_versions row FIRST (governance_status=
	//    'published', build_status='ready'). version_no = next per asset.
	//    content_origin='system' (a reviewer-published memory, not a human-
	//    authored doc); dedupe_key pins idempotency on the unit id.
	//
	//    Ordering matters: review_requests.asset_version_id is NOT NULL and
	//    REFERENCES knowledge_asset_versions(id) (014), so the version row
	//    MUST exist before the review_request insert. The earlier code inserted
	//    the review_request (step 1) with a hardcoded nil asset_version_id
	//    before creating the version (step 2) — a NOT NULL violation on real
	//    SQL (SQLSTATE 23502), and even a real versionID would have violated
	//    the FK because the target row did not exist yet. Create version, then
	//    thread its id into the review_request.
	versionID, err := createMemoryAssetVersionTx(ctx, tx, req)
	if err != nil {
		return uuid.Nil, fmt.Errorf("memory publish: asset version: %w", err)
	}

	// 2. Create the review_request (pending) carrying the version id from
	//    step 1. review_requests.governance_profile_id is NOT NULL, so the
	//    caller must supply a real profile id (the inbox service resolves the
	//    workspace's memory governance profile before calling).
	reviewID := uuid.New()
	rr := &domain.ReviewRequest{
		ID:                  reviewID,
		WorkspaceID:         req.WorkspaceID,
		AssetID:             req.AssetID,
		AssetVersionID:      versionID, // satisfies NOT NULL + FK (row exists from step 1)
		GovernanceProfileID: req.GovernanceProfileID,
		RequestedByType:     req.ReviewerType,
		RequestedByID:       req.ReviewerID,
		Status:              domain.ReviewPending,
		Rationale:           req.RationaleRedacted,
	}
	// We cannot call the wrapped ReviewRepo.CreateRequest (it opens its own tx);
	// the publish tx owns the boundary, so we inline the review_requests insert
	// here and append the decision on the same tx.
	if err := insertReviewRequestTx(ctx, tx, rr); err != nil {
		return uuid.Nil, fmt.Errorf("memory publish: review request: %w", err)
	}

	// 3. Append the approve review_decision (immutable) + project the request
	//    status to 'approved'. The decision records the reviewer + policy
	//    snapshot so the publish is auditable (验收门禁: 操作可审计).
	dec := &domain.ReviewDecisionRecord{
		ReviewRequestID:   reviewID,
		Decision:         domain.DecisionApprove,
		DecisionByType:   req.ReviewerType,
		DecisionByID:     req.ReviewerID,
		PolicyVersion:    req.PolicyVersion,
		RationaleRedacted: req.RationaleRedacted,
	}
	if err := appendReviewDecisionTx(ctx, tx, dec); err != nil {
		return uuid.Nil, fmt.Errorf("memory publish: review decision: %w", err)
	}

	// 4. Write the FTS asset_projection (status='ready'). locator is
	//    non-executable: {table: memory_units, unit_id, corpus: statement}.
	//    Provider = the configured FTS provider (zhparser by default).
	ftsProvider := req.FTSProvider
	if ftsProvider == "" {
		ftsProvider = "zhparser"
	}
	if err := insertFTSProjectionTx(ctx, tx, versionID, ftsProvider, req.FTSProviderVersion, req.UnitID); err != nil {
		return uuid.Nil, fmt.Errorf("memory publish: fts projection: %w", err)
	}

	// 5. Stamp the unit: asset_version_id + state='published'. The DB CHECK
	//    (state='published' AND superseded_by IS NULL) forbids publishing a
	//    unit that still carries an unresolved supersede candidate — a
	//    constraint violation here means the caller tried to publish a unit
	//    with a pending merge suggestion, which must be resolved first.
	if err := stampUnitPublishedTx(ctx, tx, req.UnitID, versionID); err != nil {
		return uuid.Nil, fmt.Errorf("memory publish: stamp unit: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return versionID, nil
}

// insertReviewRequestTx inserts a pending review_request on the publish tx.
// r.AssetVersionID MUST be set by the caller (the version row is created in an
// earlier step on the same tx so the NOT NULL + FK constraints hold).
func insertReviewRequestTx(ctx context.Context, tx pgx.Tx, r *domain.ReviewRequest) error {
	r.CreatedAt = time.Now().UTC()
	r.Status = domain.ReviewPending
	_, err := tx.Exec(ctx, `
		INSERT INTO review_requests
		  (id, workspace_id, asset_id, asset_version_id, governance_profile_id,
		   requested_by_type, requested_by_id, status, rationale, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.ID, r.WorkspaceID, r.AssetID, r.AssetVersionID,
		r.GovernanceProfileID, string(r.RequestedByType), r.RequestedByID,
		string(r.Status), nullIfEmpty(r.Rationale), r.CreatedAt)
	return err
}

// appendReviewDecisionTx inserts an immutable review_decision + projects the
// request status to 'approved' on the publish tx.
func appendReviewDecisionTx(ctx context.Context, tx pgx.Tx, d *domain.ReviewDecisionRecord) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.CreatedAt = time.Now().UTC()
	if _, err := tx.Exec(ctx, `
		INSERT INTO review_decisions
		  (id, review_request_id, decision, decision_by_type, decision_by_id,
		   policy_version, rationale_redacted, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		d.ID, d.ReviewRequestID, string(d.Decision),
		string(d.DecisionByType), d.DecisionByID,
		d.PolicyVersion, nullIfEmpty(d.RationaleRedacted), d.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE review_requests
		SET status = 'approved', resolved_at = now(),
		    resolved_by_type = $2, resolved_by_id = $3
		WHERE id = $1 AND status = 'pending'`,
		d.ReviewRequestID, string(d.DecisionByType), d.DecisionByID); err != nil {
		return err
	}
	return nil
}

// createMemoryAssetVersionTx inserts the knowledge_asset_versions row for a
// published memory unit. version_no is the next per asset (MAX+1).
// dedupe_key='memory_unit:'+unitID pins idempotency — a re-publish of the same
// unit is a no-op conflict (returns the existing version id).
func createMemoryAssetVersionTx(ctx context.Context, tx pgx.Tx, req evidence.PublishUnitRequest) (uuid.UUID, error) {
	dedupe := "memory_unit:" + req.UnitID.String()
	snapshot, _ := json.Marshal(map[string]any{
		"governance_profile":   "memory",
		"required_projections": []string{"fts"},
		"auto_publish":         map[string]any{"memory": false}, // 附录 A 不变量 9: no auto-publish
	})
	var versionID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, dedupe_key, build_status,
		   governance_status, activation_policy_snapshot,
		   approved_by_type, approved_by_id, approved_at,
		   created_by_type, created_by_id)
		VALUES ($1,
		        COALESCE((SELECT MAX(version_no) FROM knowledge_asset_versions WHERE asset_id = $1), 0) + 1,
		        'system', $2, 'ready', 'published', $3,
		        $4, $5, now(), $4, $5)
		ON CONFLICT (asset_id, dedupe_key) DO UPDATE
		  SET build_status = 'ready', governance_status = 'published',
		      approved_by_type = EXCLUDED.approved_by_type,
		      approved_by_id = EXCLUDED.approved_by_id,
		      approved_at = now()
		  RETURNING id`,
		req.AssetID, dedupe, snapshot,
		string(req.ReviewerType), req.ReviewerID,
	).Scan(&versionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, errors.New("memory publish: asset version insert returned no row")
		}
		return uuid.Nil, err
	}
	return versionID, nil
}

// insertFTSProjectionTx writes the FTS asset_projection (status='ready').
func insertFTSProjectionTx(ctx context.Context, tx pgx.Tx, versionID uuid.UUID, provider, providerVersion string, unitID uuid.UUID) error {
	loc, _ := json.Marshal(map[string]any{
		"table":   "memory_units",
		"unit_id": unitID.String(),
		"corpus":  "statement",
	})
	_, err := tx.Exec(ctx, `
		INSERT INTO asset_projections
		  (id, asset_version_id, projection_kind, provider, provider_version,
		   build_revision, status, locator, built_at)
		VALUES ($1,$2,'fts',$3,$4,$5,'ready',$6,now())
		ON CONFLICT (asset_version_id, projection_kind, build_revision) DO UPDATE
		  SET status = 'ready', locator = EXCLUDED.locator,
		      built_at = now(), updated_at = now()`,
		uuid.New(), versionID, provider, nullIfEmpty(providerVersion),
		"publish-"+versionID.String()[:8], loc)
	return err
}

// stampUnitPublishedTx sets the unit's asset_version_id + state='published'.
// The WHERE state <> 'published' guard makes a re-publish idempotent (the unit
// is already published → no-op; the version row's ON CONFLICT handled the
// version side). A unit with superseded_by set fails the DB CHECK and surfaces
// as an error the caller must resolve.
func stampUnitPublishedTx(ctx context.Context, tx pgx.Tx, unitID, versionID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `
		UPDATE memory_units
		SET asset_version_id = $2, state = 'published', updated_at = now()
		WHERE id = $1 AND state <> 'published'`,
		unitID, versionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already published — acceptable (idempotent re-publish); the version
		// row ON CONFLICT already reconciled. Not an error.
		return nil
	}
	return nil
}
