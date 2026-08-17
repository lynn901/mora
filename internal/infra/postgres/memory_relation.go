package postgres

// memory_relation.go implements evidence.KnowledgeRelationWriter over the 014
// knowledge_relations table (design-docs/18 §6.1, §8.3, decision D7). It is the
// narrow write slice the memory dedup/publish paths use:
//
//   - dedup: a `contradicts` RELATION is recorded here as a pending
//     reviewer-facing edge (§8.3 — recall returns contradicts Relations, never
//     silently picking one answer). This is DISTINCT from the
//     memory_dedup_suggestions row that tracks the *proposal* (pending/
//     accepted/rejected): the relation row is what recall surfaces, the
//     suggestion row is the reviewer workflow state. A contradicts suggestion
//     thus does NOT pollute memory_dedup_suggestions' semantics (验收门禁:
//     "`contradicts` 正确落 `knowledge_relations`，不污染
//     `memory_dedup_suggestions` 语义").
//   - publish: a reviewer-confirmed supersede writes a `supersedes` edge
//     between two memory assets (§6.2).
//
// Relations never cross workspaces (014 CHECK workspace_id pins it +
// application-enforced). All SQL is parameterized (07-security §10).

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryRelationRepo adapts knowledge_relations for the memory module.
type MemoryRelationRepo struct{ db *DB }

// NewMemoryRelationRepo builds a MemoryRelationRepo over the 014 table.
func NewMemoryRelationRepo(db *DB) evidence.KnowledgeRelationWriter {
	return &MemoryRelationRepo{db: db}
}

// InsertRelation inserts a knowledge_relations row. The caller pins the
// workspace_id and supplies the from/to asset ids; the 021-relaxed CHECK
// (from_asset_id <> to_asset_id OR relation_type IN ('contradicts','supersedes'))
// permits same-asset memory intra-asset edges, and the workspace FK enforces
// integrity. origin defaults to 'generated' for dedup-produced edges and
// 'human' for reviewer-confirmed supersedes; the caller sets it explicitly.
//
// FromUnitID/ToUnitID are written when set (memory intra-asset contradicts/
// supersede edges, 021); nil → NULL, which is the correct value for cross-asset
// edges whose join key is the asset id, not a unit id.
func (r *MemoryRelationRepo) InsertRelation(ctx context.Context, rel domain.KnowledgeRelation) (uuid.UUID, error) {
	var id uuid.UUID
	origin := string(rel.Origin)
	if origin == "" {
		origin = string(domain.RelationOriginGenerated)
	}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO knowledge_relations
		  (workspace_id, from_asset_id, from_version_id, relation_type,
		   to_asset_id, to_version_id, from_unit_id, to_unit_id,
		   origin, confidence, created_by_type, created_by_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id`,
		rel.WorkspaceID, rel.FromAssetID, uuidPtr(rel.FromVersionID),
		string(rel.RelationType),
		rel.ToAssetID, uuidPtr(rel.ToVersionID),
		uuidPtr(rel.FromUnitID), uuidPtr(rel.ToUnitID),
		origin, floatPtr(rel.Confidence),
		string(rel.CreatedByType), rel.CreatedByID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Compile-time check: MemoryRelationRepo satisfies the port.
var _ evidence.KnowledgeRelationWriter = (*MemoryRelationRepo)(nil)
