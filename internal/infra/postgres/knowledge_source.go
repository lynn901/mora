package postgres

// knowledge_source.go implements the source module's persistence ports over
// the 014_phase1_asset_source tables (design-docs/14 §2.1, §4.4):
//   - SourceRepo         → knowledge_sources
//   - SyncRunRepo         → source_sync_runs
//   - SourceTargetRepo    → knowledge_source_targets
//   - ReviewRepo          → review_requests / review_decisions
//   - ProjectionRepo      → asset_projections
//
// All SQL is parameterized — no string concatenation of user input
// (07-security §10). Cursor pagination uses (updated_at, id) tuples so the
// list is stable under concurrent updates. ETag is derived from updated_at
// epoch-millis + a row version; PATCH carries it as If-Match.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	source "github.com/lynn901/mora/internal/module/knowledge/source/service"
)

// errSourceNotFound mirrors source.ErrSourceNotFound for the package's
// existing errNotFound convention; the source module uses its own sentinels.
// Map pgx.ErrNoRows → source.ErrSourceNotFound in each method.

// ===========================================================================
// SourceRepo
// ===========================================================================

// SourceRepo is the postgres implementation of source.SourceRepo.
type SourceRepo struct{ db *DB }

// NewSourceRepo builds a source.SourceRepo over the pool.
func NewSourceRepo(db *DB) *SourceRepo { return &SourceRepo{db: db} }

var _ source.SourceRepo = (*SourceRepo)(nil)

// Create inserts a knowledge_sources row. The (workspace_id, source_type,
// uri_normalized) UNIQUE constraint rejects a duplicate; the caller surfaces
// it as 409. uri_normalized MUST already have embedded credentials stripped
// (§2.2) — the service enforces that before calling.
func (r *SourceRepo) Create(ctx context.Context, s *domain.KnowledgeSource) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	now := time.Now().UTC()
	s.CreatedAt = now
	s.UpdatedAt = now
	syncPolicy, _ := json.Marshal(s.SyncPolicy)
	license, _ := json.Marshal(s.License)
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO knowledge_sources
		  (id, workspace_id, source_type, name, uri_normalized, credential_ref,
		   sync_policy, trust_level, license, current_revision, enabled,
		   created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id`,
		s.ID, s.WorkspaceID, string(s.SourceType), s.Name, s.URINormalized,
		nullIfEmpty(s.CredentialRef), syncPolicy, string(s.TrustLevel),
		license, nullIfEmpty(s.CurrentRevision), s.Enabled,
		string(s.CreatedByType), s.CreatedByID, s.CreatedAt, s.UpdatedAt,
	).Scan(&s.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return errSourceConflict
		}
		return err
	}
	s.ETagVersion = etagOf(s.UpdatedAt, 0)
	return nil
}

// Get loads a source by id. A missing row returns source.ErrSourceNotFound so
// the handler surfaces 404 (existence never leaks, §8.2).
func (r *SourceRepo) Get(ctx context.Context, id uuid.UUID) (*domain.KnowledgeSource, error) {
	row := r.db.Pool.QueryRow(ctx, sourceColumns+` FROM knowledge_sources WHERE id = $1`, id)
	s, err := scanSource(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, source.ErrSourceNotFound
		}
		return nil, err
	}
	return s, nil
}

// GetWorkspace returns the source's workspace_id + enabled flag for the authz
// SourceLocator and the cross-workspace guard. A missing source returns
// source.ErrSourceNotFound (no existence leak).
func (r *SourceRepo) GetWorkspace(ctx context.Context, id uuid.UUID) (uuid.UUID, bool, error) {
	var wsID uuid.UUID
	var enabled bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT workspace_id, enabled FROM knowledge_sources WHERE id = $1`, id).
		Scan(&wsID, &enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, source.ErrSourceNotFound
		}
		return uuid.Nil, false, err
	}
	return wsID, enabled, nil
}

// List returns a cursor-paginated page of sources (§4.4 GET). Cursor is the
// base64 of "updated_at|id"; the next page starts AFTER that tuple so the
// ordering is stable under concurrent updates.
func (r *SourceRepo) List(ctx context.Context, q source.SourceListQuery) ([]*domain.KnowledgeSource, string, error) {
	pageSize := q.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	args := []any{q.WorkspaceID}
	sb := sourceColumns + ` FROM knowledge_sources WHERE workspace_id = $1`
	argIdx := 2
	if q.SourceType != "" {
		sb += fmt.Sprintf(` AND source_type = $%d`, argIdx)
		args = append(args, q.SourceType)
		argIdx++
	}
	if q.Enabled != nil {
		sb += fmt.Sprintf(` AND enabled = $%d`, argIdx)
		args = append(args, *q.Enabled)
		argIdx++
	}
	if q.Cursor != "" {
		ts, id, ok := decodeSourceCursor(q.Cursor)
		if ok {
			sb += fmt.Sprintf(` AND (updated_at, id) > ($%d, $%d)`, argIdx, argIdx+1)
			args = append(args, ts, id)
			argIdx += 2
		}
	}
	sb += fmt.Sprintf(` ORDER BY updated_at ASC, id ASC LIMIT $%d`, argIdx)
	args = append(args, pageSize+1) // fetch one extra to detect a next page
	rows, err := r.db.Pool.Query(ctx, sb, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]*domain.KnowledgeSource, 0, pageSize)
	var lastTs time.Time
	var lastID uuid.UUID
	for rows.Next() {
		s, err := scanSource(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, s)
		lastTs = s.UpdatedAt
		lastID = s.ID
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > pageSize {
		out = out[:pageSize]
		next = encodeSourceCursor(lastTs, lastID)
	}
	return out, next, nil
}

// Update applies a partial patch gated by ETag (§4.4 PATCH, If-Match). The
// ETag is updated_at epoch-ms; a mismatch (row updated under our feet) returns
// source.ErrSourceConflict.
func (r *SourceRepo) Update(ctx context.Context, id uuid.UUID, etag int64, patch source.SourcePatch) (*domain.KnowledgeSource, error) {
	// Optimistic concurrency: only update if updated_at epoch-ms matches etag.
	// We resolve the current row first so a missing source surfaces as 404
	// (existence never leaks) rather than a silent 0-rows-affected.
	cur, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if etagOf(cur.UpdatedAt, 0) != etag {
		return nil, source.ErrSourceConflict
	}
	args := []any{id}
	argIdx := 2
	setClause := "updated_at = now()"
	if patch.Name != nil {
		setClause += fmt.Sprintf(`, name = $%d`, argIdx)
		args = append(args, *patch.Name)
		argIdx++
	}
	if patch.SyncPolicy != nil {
		b, _ := json.Marshal(patch.SyncPolicy)
		setClause += fmt.Sprintf(`, sync_policy = $%d`, argIdx)
		args = append(args, b)
		argIdx++
	}
	if patch.TrustLevel != nil {
		setClause += fmt.Sprintf(`, trust_level = $%d`, argIdx)
		args = append(args, string(*patch.TrustLevel))
		argIdx++
	}
	if patch.License != nil {
		b, _ := json.Marshal(patch.License)
		setClause += fmt.Sprintf(`, license = $%d`, argIdx)
		args = append(args, b)
		argIdx++
	}
	if patch.Enabled != nil {
		setClause += fmt.Sprintf(`, enabled = $%d`, argIdx)
		args = append(args, *patch.Enabled)
		argIdx++
	}
	row := r.db.Pool.QueryRow(ctx, fmt.Sprintf(`
		UPDATE knowledge_sources SET %s WHERE id = $1
		RETURNING %s`, setClause, sourceColumnList), args...)
	s, err := scanSource(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, source.ErrSourceNotFound
		}
		return nil, err
	}
	s.ETagVersion = etagOf(s.UpdatedAt, 0)
	return s, nil
}

// Disable soft-deletes a source (enabled=false, §4.4 DELETE). A disabled
// source is excluded from the default list (idx_sources_workspace is partial
// on enabled=true).
func (r *SourceRepo) Disable(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE knowledge_sources SET enabled = false, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return source.ErrSourceNotFound
	}
	return nil
}

// SetCredential updates credential_ref only (§4.4 PUT /credentials). It bumps
// updated_at so the ETag advances (callers fetching the new ETag must re-GET).
// Never reads or returns plaintext.
func (r *SourceRepo) SetCredential(ctx context.Context, id uuid.UUID, ref, version string) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE knowledge_sources SET credential_ref = $2, updated_at = now() WHERE id = $1`,
		id, nullIfEmpty(ref))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return source.ErrSourceNotFound
	}
	_ = version // credential_version is recorded on the Run, not the source.
	return nil
}

// ===========================================================================
// SyncRunRepo
// ===========================================================================

// SyncRunRepo is the postgres implementation of source.SyncRunRepo.
type SyncRunRepo struct{ db *DB }

// NewSyncRunRepo builds a source.SyncRunRepo over the pool.
func NewSyncRunRepo(db *DB) *SyncRunRepo { return &SyncRunRepo{db: db} }

var _ source.SyncRunRepo = (*SyncRunRepo)(nil)

// Create inserts a source_sync_runs row. The idempotency_key is UNIQUE; a
// duplicate returns source.ErrIdempotencyConflict — the caller (service)
// decides whether the duplicate is a safe retry (same inputs) or a conflict.
func (r *SyncRunRepo) Create(ctx context.Context, run *domain.SourceSyncRun) error {
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	if run.IdempotencyKey == "" {
		run.IdempotencyKey = uuid.NewString()
	}
	if run.Status == "" {
		run.Status = domain.SyncRunQueued
	}
	snap, _ := json.Marshal(run.SourceConfigSnapshot)
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO source_sync_runs
		  (id, source_id, requested_by_type, requested_by_id, requested_revision,
		   source_config_snapshot, credential_version, governance_profile_id,
		   requested_asset_type, status, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, created_at`,
		run.ID, run.SourceID, string(run.RequestedByType), run.RequestedByID,
		nullIfEmpty(run.RequestedRevision), snap, nullIfEmpty(run.CredentialVersion),
		nilIfZero(run.GovernanceProfileID), string(run.RequestedAssetType),
		string(run.Status), run.IdempotencyKey,
	).Scan(&run.ID, &run.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return source.ErrIdempotencyConflict
		}
		return err
	}
	return nil
}

// Get loads a run by id.
func (r *SyncRunRepo) Get(ctx context.Context, id uuid.UUID) (*domain.SourceSyncRun, error) {
	row := r.db.Pool.QueryRow(ctx, runColumns+` FROM source_sync_runs WHERE id = $1`, id)
	run, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, source.ErrRunNotFound
		}
		return nil, err
	}
	return run, nil
}

// GetByIdempotencyKey loads a run by its idempotency_key (§4.4 idempotent
// retry). A missing key returns source.ErrRunNotFound — no existence leak.
func (r *SyncRunRepo) GetByIdempotencyKey(ctx context.Context, key string) (*domain.SourceSyncRun, error) {
	row := r.db.Pool.QueryRow(ctx, runColumns+` FROM source_sync_runs WHERE idempotency_key = $1`, key)
	run, err := scanRun(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, source.ErrRunNotFound
		}
		return nil, err
	}
	return run, nil
}

// List returns a cursor-paginated page of runs for a source (§4.4 GET
// /sync-runs). Cursor is base64 of "created_at|id".
func (r *SyncRunRepo) List(ctx context.Context, q source.SyncRunListQuery) ([]*domain.SourceSyncRun, string, error) {
	pageSize := q.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	args := []any{q.SourceID}
	sb := runColumns + ` FROM source_sync_runs WHERE source_id = $1`
	argIdx := 2
	if q.Status != "" {
		sb += fmt.Sprintf(` AND status = $%d`, argIdx)
		args = append(args, q.Status)
		argIdx++
	}
	if q.Cursor != "" {
		ts, id, ok := decodeSourceCursor(q.Cursor)
		if ok {
			sb += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d)`, argIdx, argIdx+1)
			args = append(args, ts, id)
			argIdx += 2
		}
	}
	sb += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, argIdx)
	args = append(args, pageSize+1)
	rows, err := r.db.Pool.Query(ctx, sb, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]*domain.SourceSyncRun, 0, pageSize)
	var lastTs time.Time
	var lastID uuid.UUID
	for rows.Next() {
		run, err := scanRun(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, run)
		lastTs = run.CreatedAt
		lastID = run.ID
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > pageSize {
		out = out[:pageSize]
		next = encodeSourceCursor(lastTs, lastID)
	}
	return out, next, nil
}

// UpdateStatus transitions a run's status (knowledge-worker path).
func (r *SyncRunRepo) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.SyncRunStatus, resolvedRevision, errCode, errDetail string) error {
	var finishedAt any
	switch status {
	case domain.SyncRunReady, domain.SyncRunFailed, domain.SyncRunCancelled:
		finishedAt = time.Now().UTC()
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE source_sync_runs
		SET status = $2, resolved_revision = COALESCE($3, resolved_revision),
		    error_code = NULLIF($4, ''), error_detail_redacted = NULLIF($5, ''),
		    finished_at = COALESCE($6, finished_at), started_at =
		      CASE WHEN $2 IN ('fetching','processing') AND started_at IS NULL THEN now() ELSE started_at END
		WHERE id = $1`,
		id, string(status), nullIfEmpty(resolvedRevision), errCode, errDetail, finishedAt)
	return err
}

// ===========================================================================
// SourceTargetRepo
// ===========================================================================

// SourceTargetRepo is the postgres implementation of source.SourceTargetRepo.
type SourceTargetRepo struct{ db *DB }

func NewSourceTargetRepo(db *DB) *SourceTargetRepo { return &SourceTargetRepo{db: db} }

var _ source.SourceTargetRepo = (*SourceTargetRepo)(nil)

// Upsert inserts or updates a target → asset mapping (§4.2). The same
// (source_id, target_key) re-syncs to the same asset; active stays true.
func (r *SourceTargetRepo) Upsert(ctx context.Context, t domain.SourceTarget) error {
	sel, _ := json.Marshal(t.Selector)
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO knowledge_source_targets
		  (source_id, target_key, asset_type, asset_id, selector, active)
		VALUES ($1,$2,$3,$4,$5,true)
		ON CONFLICT (source_id, target_key) DO UPDATE
		  SET asset_id = EXCLUDED.asset_id, asset_type = EXCLUDED.asset_type,
		      selector = EXCLUDED.selector, active = true, updated_at = now()`,
		t.SourceID, t.TargetKey, string(t.AssetType), t.AssetID, sel)
	return err
}

// ListBySource returns active targets for a source.
func (r *SourceTargetRepo) ListBySource(ctx context.Context, sourceID uuid.UUID) ([]domain.SourceTarget, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT source_id, target_key, asset_type, asset_id, selector, active,
		       first_seen_at, updated_at
		FROM knowledge_source_targets
		WHERE source_id = $1 AND active = true`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SourceTarget
	for rows.Next() {
		var t domain.SourceTarget
		var assetType string
		var sel []byte
		if err := rows.Scan(&t.SourceID, &t.TargetKey, &assetType, &t.AssetID,
			&sel, &t.Active, &t.FirstSeenAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.AssetType = domain.AssetType(assetType)
		if len(sel) > 0 && string(sel) != "null" {
			_ = json.Unmarshal(sel, &t.Selector)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ===========================================================================
// ReviewRepo
// ===========================================================================

// ReviewRepo is the postgres implementation of source.ReviewRepo.
type ReviewRepo struct{ db *DB }

func NewReviewRepo(db *DB) *ReviewRepo { return &ReviewRepo{db: db} }

var _ source.ReviewRepo = (*ReviewRepo)(nil)

// CreateRequest inserts a pending review_request (§4.2).
func (r *ReviewRepo) CreateRequest(ctx context.Context, req *domain.ReviewRequest) error {
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	req.CreatedAt = time.Now().UTC()
	req.Status = domain.ReviewPending
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO review_requests
		  (id, workspace_id, asset_id, asset_version_id, governance_profile_id,
		   requested_by_type, requested_by_id, status, rationale, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING created_at`,
		req.ID, req.WorkspaceID, req.AssetID, req.AssetVersionID,
		req.GovernanceProfileID, string(req.RequestedByType), req.RequestedByID,
		string(req.Status), nullIfEmpty(req.Rationale), req.CreatedAt,
	).Scan(&req.CreatedAt)
	return err
}

// GetRequest loads a review request by id.
func (r *ReviewRepo) GetRequest(ctx context.Context, id uuid.UUID) (*domain.ReviewRequest, error) {
	var req domain.ReviewRequest
	var (
		rationale        *string
		resolvedAt       *time.Time
		resolvedByType   *string
		resolvedByID     *uuid.UUID
		byType, status   string
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, asset_id, asset_version_id, governance_profile_id,
		       requested_by_type, requested_by_id, status, rationale,
		       created_at, resolved_at, resolved_by_type, resolved_by_id
		FROM review_requests WHERE id = $1`, id).Scan(
		&req.ID, &req.WorkspaceID, &req.AssetID, &req.AssetVersionID,
		&req.GovernanceProfileID, &byType, &req.RequestedByID, &status, &rationale,
		&req.CreatedAt, &resolvedAt, &resolvedByType, &resolvedByID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, source.ErrReviewNotFound
		}
		return nil, err
	}
	req.RequestedByType = domain.SubjectType(byType)
	req.Status = domain.ReviewRequestStatus(status)
	if rationale != nil {
		req.Rationale = *rationale
	}
	req.ResolvedAt = resolvedAt
	if resolvedByType != nil {
		req.ResolvedByType = domain.SubjectType(*resolvedByType)
	}
	req.ResolvedByID = resolvedByID
	return &req, nil
}

// ListPending returns pending review_requests for a workspace (cursor-paginated,
// §4.4 GET /reviews?status=pending). Cursor is base64 of "created_at|id".
func (r *ReviewRepo) ListPending(ctx context.Context, workspaceID uuid.UUID, cursor string, pageSize int) ([]*domain.ReviewRequest, string, error) {
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	args := []any{workspaceID}
	sb := `SELECT id, workspace_id, asset_id, asset_version_id, governance_profile_id,
		       requested_by_type, requested_by_id, status, rationale,
		       created_at, resolved_at, resolved_by_type, resolved_by_id
		FROM review_requests WHERE workspace_id = $1 AND status = 'pending'`
	argIdx := 2
	if cursor != "" {
		ts, id, ok := decodeSourceCursor(cursor)
		if ok {
			sb += fmt.Sprintf(` AND (created_at, id) > ($%d, $%d)`, argIdx, argIdx+1)
			args = append(args, ts, id)
			argIdx += 2
		}
	}
	sb += fmt.Sprintf(` ORDER BY created_at ASC, id ASC LIMIT $%d`, argIdx)
	args = append(args, pageSize+1)
	rows, err := r.db.Pool.Query(ctx, sb, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := make([]*domain.ReviewRequest, 0, pageSize)
	var lastTs time.Time
	var lastID uuid.UUID
	for rows.Next() {
		var req domain.ReviewRequest
		var (
			rationale      *string
			resolvedAt     *time.Time
			resolvedByType *string
			resolvedByID   *uuid.UUID
			byType, status string
		)
		if err := rows.Scan(
			&req.ID, &req.WorkspaceID, &req.AssetID, &req.AssetVersionID,
			&req.GovernanceProfileID, &byType, &req.RequestedByID, &status, &rationale,
			&req.CreatedAt, &resolvedAt, &resolvedByType, &resolvedByID); err != nil {
			return nil, "", err
		}
		req.RequestedByType = domain.SubjectType(byType)
		req.Status = domain.ReviewRequestStatus(status)
		if rationale != nil {
			req.Rationale = *rationale
		}
		req.ResolvedAt = resolvedAt
		if resolvedByType != nil {
			req.ResolvedByType = domain.SubjectType(*resolvedByType)
		}
		req.ResolvedByID = resolvedByID
		out = append(out, &req)
		lastTs = req.CreatedAt
		lastID = req.ID
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > pageSize {
		out = out[:pageSize]
		next = encodeSourceCursor(lastTs, lastID)
	}
	return out, next, nil
}

// AppendDecision adds an immutable review_decision + projects the request
// status (§4.2). Decisions are append-only; the request's status reflects the
// latest decision. Idempotency-Key is enforced at the service layer (the
// idempotency_key column on the Run is for sync, not review).
func (r *ReviewRepo) AppendDecision(ctx context.Context, d *domain.ReviewDecisionRecord) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	d.CreatedAt = time.Now().UTC()
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
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
	// Project the request status from the decision.
	status := map[string]string{
		"approve":   "approved",
		"reject":    "rejected",
		"merge":     "approved",
		"promote":   "approved",
		"deprecate": "superseded",
	}[string(d.Decision)]
	if status == "" {
		status = "superseded"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE review_requests
		SET status = $2, resolved_at = now(),
		    resolved_by_type = $3, resolved_by_id = $4
		WHERE id = $1 AND status = 'pending'`,
		d.ReviewRequestID, status, string(d.DecisionByType), d.DecisionByID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetWorkspace returns the review's workspace_id (for the authz ReviewLocator).
func (r *ReviewRepo) GetWorkspace(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var wsID uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT workspace_id FROM review_requests WHERE id = $1`, id).Scan(&wsID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, source.ErrReviewNotFound
		}
		return uuid.Nil, err
	}
	return wsID, nil
}

// ===========================================================================
// ProjectionRepo
// ===========================================================================

// ProjectionRepo is the postgres implementation of source.ProjectionRepo.
type ProjectionRepo struct{ db *DB }

func NewProjectionRepo(db *DB) *ProjectionRepo { return &ProjectionRepo{db: db} }

var _ source.ProjectionRepo = (*ProjectionRepo)(nil)

// Upsert inserts or updates an asset_projections row keyed by
// (asset_version_id, projection_kind, build_revision). A rebuild produces a
// new build_revision — never an in-place rewrite (§2.2).
func (r *ProjectionRepo) Upsert(ctx context.Context, p domain.AssetProjection) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	loc, _ := json.Marshal(p.Locator)
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO asset_projections
		  (id, asset_version_id, projection_kind, provider, provider_version,
		   build_revision, status, locator, built_at, last_error, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now(),now())
		ON CONFLICT (asset_version_id, projection_kind, build_revision) DO UPDATE
		  SET status = EXCLUDED.status, locator = EXCLUDED.locator,
		      built_at = EXCLUDED.built_at, last_error = EXCLUDED.last_error,
		      updated_at = now()`,
		p.ID, p.AssetVersionID, string(p.ProjectionKind), p.Provider,
		nullIfEmpty(p.ProviderVersion), p.BuildRevision, string(p.Status),
		loc, nilIfZeroTime(p.BuiltAt), nullIfEmpty(p.LastError))
	return err
}

// ListByVersion returns all projections for a version (for the §7 activation
// gate: every required projection must be `ready`).
func (r *ProjectionRepo) ListByVersion(ctx context.Context, versionID uuid.UUID) ([]domain.AssetProjection, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, asset_version_id, projection_kind, provider, provider_version,
		       build_revision, status, locator, built_at, last_error, created_at, updated_at
		FROM asset_projections WHERE asset_version_id = $1`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AssetProjection
	for rows.Next() {
		var p domain.AssetProjection
		var (
			kind, status, provider string
			providerVer            *string
			loc                    []byte
			builtAt                *time.Time
			lastErr                *string
		)
		if err := rows.Scan(&p.ID, &p.AssetVersionID, &kind, &provider, &providerVer,
			&p.BuildRevision, &status, &loc, &builtAt, &lastErr, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.ProjectionKind = domain.ProjectionKind(kind)
		p.Status = domain.ProjectionStatus(status)
		p.Provider = provider
		if providerVer != nil {
			p.ProviderVersion = *providerVer
		}
		if len(loc) > 0 && string(loc) != "null" {
			_ = json.Unmarshal(loc, &p.Locator)
		}
		p.BuiltAt = builtAt
		if lastErr != nil {
			p.LastError = *lastErr
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ===========================================================================
// helpers
// ===========================================================================

// sourceColumnList is the bare column list (no "SELECT ") for knowledge_sources,
// kept in sync with scanSource's field order. Callers compose either
// "SELECT <list> FROM knowledge_sources WHERE ..." or
// "UPDATE knowledge_sources SET ... RETURNING <list>".
const sourceColumnList = `id, workspace_id, source_type, name, uri_normalized,
       credential_ref, sync_policy, trust_level, license, current_revision,
       enabled, last_synced_at, last_error, created_by_type, created_by_id,
       created_at, updated_at`

// sourceColumns prepends "SELECT " for QueryRow/Query callers that build a
// full SELECT statement by string concatenation.
const sourceColumns = `SELECT ` + sourceColumnList

// scanSource scans a knowledge_sources row. The empty-credential-redaction
// invariant (§2.2) is enforced at insert; here we just read what's stored.
func scanSource(scan func(dest ...any) error) (*domain.KnowledgeSource, error) {
	var s domain.KnowledgeSource
	var (
		credRef, currentRev *string
		syncPolicy, license  []byte
		lastSynced           *time.Time
		lastError            *string
		stype, trust         string
		byType               string
	)
	if err := scan(
		&s.ID, &s.WorkspaceID, &stype, &s.Name, &s.URINormalized,
		&credRef, &syncPolicy, &trust, &license, &currentRev,
		&s.Enabled, &lastSynced, &lastError, &byType, &s.CreatedByID,
		&s.CreatedAt, &s.UpdatedAt,
	); err != nil {
		return nil, err
	}
	s.SourceType = domain.SourceType(stype)
	s.TrustLevel = domain.TrustLevel(trust)
	s.CreatedByType = domain.SubjectType(byType)
	if credRef != nil {
		s.CredentialRef = *credRef
	}
	if currentRev != nil {
		s.CurrentRevision = *currentRev
	}
	if lastSynced != nil {
		s.LastSyncedAt = lastSynced
	}
	if lastError != nil {
		s.LastError = *lastError
	}
	if len(syncPolicy) > 0 && string(syncPolicy) != "null" {
		_ = json.Unmarshal(syncPolicy, &s.SyncPolicy)
	}
	if len(license) > 0 && string(license) != "null" {
		_ = json.Unmarshal(license, &s.License)
	}
	s.ETagVersion = etagOf(s.UpdatedAt, 0)
	return &s, nil
}

// runColumns is the canonical SELECT column list for source_sync_runs.
const runColumns = `SELECT id, source_id, requested_by_type, requested_by_id,
       requested_revision, resolved_revision, source_config_snapshot,
       credential_version, governance_profile_id, requested_asset_type,
       status, attempt, idempotency_key, started_at, finished_at,
       error_code, error_detail_redacted, created_at`

func scanRun(scan func(dest ...any) error) (*domain.SourceSyncRun, error) {
	var run domain.SourceSyncRun
	var (
		reqByType, status, assetType string
		reqRev, resRev, credVer      *string
		configSnap                   []byte
		govProfile                   *uuid.UUID
		started, finished             *time.Time
		errCode, errDetail           *string
	)
	if err := scan(
		&run.ID, &run.SourceID, &reqByType, &run.RequestedByID,
		&reqRev, &resRev, &configSnap, &credVer, &govProfile, &assetType,
		&status, &run.Attempt, &run.IdempotencyKey, &started, &finished,
		&errCode, &errDetail, &run.CreatedAt,
	); err != nil {
		return nil, err
	}
	run.RequestedByType = domain.SubjectType(reqByType)
	run.Status = domain.SyncRunStatus(status)
	run.RequestedAssetType = domain.RequestedAssetType(assetType)
	if reqRev != nil {
		run.RequestedRevision = *reqRev
	}
	if resRev != nil {
		run.ResolvedRevision = *resRev
	}
	if credVer != nil {
		run.CredentialVersion = *credVer
	}
	run.GovernanceProfileID = govProfile
	run.StartedAt = started
	run.FinishedAt = finished
	if errCode != nil {
		run.ErrorCode = *errCode
	}
	if errDetail != nil {
		run.ErrorDetailRedacted = *errDetail
	}
	if len(configSnap) > 0 && string(configSnap) != "null" {
		_ = json.Unmarshal(configSnap, &run.SourceConfigSnapshot)
	}
	return &run, nil
}

// etagOf derives an ETag integer from updated_at epoch-millis. Good enough for
// optimistic concurrency: any update bumps updated_at → the ETag changes.
func etagOf(t time.Time, _ int64) int64 {
	return t.UnixMilli()
}

// encodeSourceCursor base64-encodes "RFC3339Nano|uuid" for opaque pagination.
func encodeSourceCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

// decodeSourceCursor parses an opaque cursor into (timestamp, id). Returns
// (zero, zero, false) on a malformed cursor so the caller degrades to page 1.
func decodeSourceCursor(c string) (time.Time, uuid.UUID, bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	parts := splitTwo(string(b), "|")
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, false
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	return ts, id, true
}

// splitTwo splits s on sep into at most 2 parts.
func splitTwo(s, sep string) []string {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}

// nilIfZeroTime returns nil for a zero time so nullable TIMESTAMPTZ stays NULL.
func nilIfZeroTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}

// errSourceConflict is the package-local alias for the source conflict error.
var errSourceConflict = source.ErrSourceConflict
