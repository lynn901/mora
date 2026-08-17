package postgres

// evidence_authz_repo.go implements the authz.EvidenceRepo narrow port over
// the 018_phase4_agent_memory.memory_evidence table (design-docs/18 §4.4,
// decision D2). It returns only EvidenceInfo (workspace + owner + source_asset
// + visibility) — the minimal projection the authz decision pipeline consults.
// It is distinct from the module/memory evidence.EvidenceRepo full CRUD repo
// for the same narrow-port reason as assetRepo/sourceRepo: the authz layer
// depends on a minimal read shape, not the whole repository.
//
// A missing or deleted row returns errNotFound so the locator maps it to
// ErrTargetNotFound (existence never leaks, §9.3). All SQL is parameterized
// — no string-concatenated user input (07-security §10).

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
)

// evidenceAuthzRepo adapts memory_evidence reads for authz.Service + the
// EvidenceLocator.
type evidenceAuthzRepo struct{ db *DB }

// NewAuthzEvidenceRepo builds an authz.EvidenceRepo over memory_evidence.
func NewAuthzEvidenceRepo(db *DB) authz.EvidenceRepo { return &evidenceAuthzRepo{db: db} }

// Get loads the authz projection of an evidence row by id. Deleted rows
// (deleted_at IS NOT NULL) are treated as not-found so a soft-deleted evidence
// is invisible to authorization (§9.3).
func (r *evidenceAuthzRepo) Get(ctx context.Context, evidenceID uuid.UUID) (authz.EvidenceInfo, error) {
	var (
		info        authz.EvidenceInfo
		ownerType   string
		visibility  string
		sourceAsset *uuid.UUID
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT workspace_id, owner_type, owner_id, source_asset_id, visibility
		FROM memory_evidence
		WHERE id = $1 AND deleted_at IS NULL`, evidenceID).Scan(
		&info.WorkspaceID, &ownerType, &info.OwnerID, &sourceAsset, &visibility)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.EvidenceInfo{}, errNotFound
		}
		return authz.EvidenceInfo{}, err
	}
	info.OwnerType = domain.OwnerType(ownerType)
	info.Visibility = domain.EvidenceVisibility(visibility)
	info.SourceAssetID = sourceAsset
	return info, nil
}

var _ authz.EvidenceRepo = (*evidenceAuthzRepo)(nil)
