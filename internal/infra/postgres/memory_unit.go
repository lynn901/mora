package postgres

// memory_unit.go implements evidence.MemoryUnitRepo over the
// 018_phase4_agent_memory.memory_units table (design-docs/18 §2.2).
//
// memory_units.asset_id has a real FK to knowledge_assets(id) ON DELETE
// CASCADE; the asset is the lifecycle owner of the unit. asset_version_id is
// ON DELETE SET NULL (a pinned version may vanish without nuking the unit).
// superseded_by and evidence_missing are written only by reviewer-confirmed
// governance / deletion-propagation paths — this repo carries the writes, the
// policy lives in the dedup/propagation services.
//
// All SQL is parameterized — no string-concatenated user input (07-security §10).

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryUnitRepo adapts memory_units for the recall/dedup/inbox services.
type MemoryUnitRepo struct{ db *DB }

// NewMemoryUnitRepo builds a MemoryUnitRepo over the 018 memory_units table.
func NewMemoryUnitRepo(db *DB) evidence.MemoryUnitRepo { return &MemoryUnitRepo{db: db} }

// Insert persists a new memory unit in the candidate state (§6.2). The caller
// sets MemoryType/Statement/StructuredPayload/Confidence/Validity; AssetID must
// reference an existing knowledge_assets(asset_type='memory') row.
func (r *MemoryUnitRepo) Insert(ctx context.Context, u domain.MemoryUnit) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO memory_units
		  (workspace_id, asset_id, asset_version_id, memory_type, statement,
		   structured_payload, confidence, valid_from, expires_at, state,
		   evidence_missing, authority, created_by_type, created_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		RETURNING id`,
		u.WorkspaceID, u.AssetID, uuidPtr(u.AssetVersionID),
		string(u.MemoryType), u.Statement,
		jsonBytes(u.StructuredPayload),
		floatPtr(u.Confidence), timePtr(u.ValidFrom), timePtr(u.ExpiresAt),
		stateOrCandidate(u.State),
		u.EvidenceMissing, authorityOrHalf(u.Authority),
		string(u.CreatedByType), u.CreatedByID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Get loads a memory unit by id. A missing row returns ErrMemoryUnitNotFound so
// existence never leaks (§9.3). The service layer filters private candidates to
// the owner above this repo.
func (r *MemoryUnitRepo) Get(ctx context.Context, id uuid.UUID) (domain.MemoryUnit, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, asset_id, asset_version_id, memory_type, statement,
		       structured_payload, confidence, valid_from, expires_at, state,
		       superseded_by, evidence_missing, authority, created_by_type, created_by_id,
		       created_at, updated_at
		FROM memory_units WHERE id = $1`, id)
	u, err := scanUnit(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.MemoryUnit{}, domain.ErrMemoryUnitNotFound
		}
		return domain.MemoryUnit{}, err
	}
	return u, nil
}

// ListByAsset returns all units for an asset (asset-version history view).
func (r *MemoryUnitRepo) ListByAsset(ctx context.Context, assetID uuid.UUID) ([]domain.MemoryUnit, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, asset_id, asset_version_id, memory_type, statement,
		       structured_payload, confidence, valid_from, expires_at, state,
		       superseded_by, evidence_missing, authority, created_by_type, created_by_id,
		       created_at, updated_at
		FROM memory_units WHERE asset_id = $1 ORDER BY created_at DESC`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUnits(rows)
}

// ListCandidates returns candidate-state units in a workspace (§6.3 inbox).
func (r *MemoryUnitRepo) ListCandidates(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemoryUnit, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, asset_id, asset_version_id, memory_type, statement,
		       structured_payload, confidence, valid_from, expires_at, state,
		       superseded_by, evidence_missing, authority, created_by_type, created_by_id,
		       created_at, updated_at
		FROM memory_units WHERE workspace_id = $1 AND state = 'candidate'
		ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUnits(rows)
}

// SetState transitions a unit's state (§6.2). Published requires a
// review_decision by the caller — the repo does not enforce review here. The
// CHECK (state='published' AND superseded_by IS NULL) is enforced by the DB.
func (r *MemoryUnitRepo) SetState(ctx context.Context, id uuid.UUID, state domain.MemoryUnitState) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_units SET state = $2, updated_at = now() WHERE id = $1`,
		id, string(state))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMemoryUnitNotFound
	}
	return nil
}

// SetSupersededBy records a reviewer-confirmed merge/supersede (D7). The DB
// CHECK forbids setting superseded_by on a published unit.
func (r *MemoryUnitRepo) SetSupersededBy(ctx context.Context, id, supersededBy uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_units SET superseded_by = $2, updated_at = now()
		WHERE id = $1 AND state <> 'published'`, id, supersededBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMemoryUnitNotFound
	}
	return nil
}

// MarkEvidenceMissing flags a unit whose backing evidence is gone (D3
// propagation). The unit exits high-authority recall but stays readable.
func (r *MemoryUnitRepo) MarkEvidenceMissing(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_units SET evidence_missing = true, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMemoryUnitNotFound
	}
	return nil
}

func scanUnit(scan scanFunc) (domain.MemoryUnit, error) {
	var u domain.MemoryUnit
	var (
		mtype, state, createdByType string
		payload                     []byte
		confidence                  *float64
		assetVer                    *uuid.UUID
		superseded                  *uuid.UUID
	)
	err := scan(
		&u.ID, &u.WorkspaceID, &u.AssetID, &assetVer, &mtype, &u.Statement,
		&payload, &confidence, &u.ValidFrom, &u.ExpiresAt, &state,
		&superseded, &u.EvidenceMissing, &u.Authority, &createdByType, &u.CreatedByID,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return domain.MemoryUnit{}, err
	}
	u.MemoryType = domain.MemoryType(mtype)
	u.State = domain.MemoryUnitState(state)
	u.AssetVersionID = assetVer
	u.SupersededBy = superseded
	u.Confidence = confidence
	u.StructuredPayload = jsonMap(payload)
	u.CreatedByType = domain.OwnerType(createdByType)
	return u, nil
}

func collectUnits(rows pgx.Rows) ([]domain.MemoryUnit, error) {
	var out []domain.MemoryUnit
	for rows.Next() {
		u, err := scanUnit(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// stateOrCandidate defaults an empty state to 'candidate' (§6.2 first version).
func stateOrCandidate(s domain.MemoryUnitState) any {
	if s == "" {
		return string(domain.MemoryCandidate)
	}
	return string(s)
}

// authorityOrHalf defaults a zero authority to 0.5 (§2.2 DEFAULT 0.5).
func authorityOrHalf(a float64) any {
	if a == 0 {
		return 0.5
	}
	return a
}

// floatPtr returns a *float64 for nullable NUMERIC columns.
func floatPtr(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

var _ evidence.MemoryUnitRepo = (*MemoryUnitRepo)(nil)
