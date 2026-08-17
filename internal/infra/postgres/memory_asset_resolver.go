package postgres

// memory_asset_resolver.go implements distill.MemoryAssetResolver over
// knowledge_assets (design-docs/18 §2.2, D1). memory_units.asset_id references
// knowledge_assets(asset_type='memory'); the distill loader resolves that row
// per workspace so a candidate unit has an asset to attach to.
//
// The asset is created on first capture: INSERT ... ON CONFLICT (workspace_id,
// asset_type, name) — the name "memory" is the canonical workspace-memory
// asset. A workspace thus has exactly one memory asset, the lifecycle owner of
// all its memory_units rows. Idempotent: a re-run returns the existing id.
//
// All SQL is parameterized (07-security §10). Existence is not leaked — the
// resolver is only called from the authenticated extract path.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// MemoryAssetResolverImpl is the postgres implementation of
// distill.MemoryAssetResolver (the knowledge-worker's narrow port). It is
// distinct from the full AssetRegistry: memory assets are workspace-scoped and
// have no native_document_id, so the document upsert path does not apply.
type MemoryAssetResolverImpl struct{ db *DB }

// NewMemoryAssetResolver builds the resolver over a DB.
func NewMemoryAssetResolver(db *DB) *MemoryAssetResolverImpl {
	return &MemoryAssetResolverImpl{db: db}
}

// GetOrCreateMemoryAsset resolves the workspace's memory asset, creating it
// idempotently on first call. The asset's owner is the caller; on a race the
// ON CONFLICT path returns the existing row.
func (r *MemoryAssetResolverImpl) GetOrCreateMemoryAsset(ctx context.Context, workspaceID uuid.UUID, ownerType domain.OwnerType, ownerID uuid.UUID) (uuid.UUID, error) {
	const name = "memory"
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO knowledge_assets
		  (workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,'memory',$2,$3,$4,'draft','private')
		ON CONFLICT (workspace_id, asset_type, name) DO UPDATE SET updated_at = now()
		RETURNING id`,
		workspaceID, name, string(ownerType), ownerID,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, domain.ErrMemoryUnitNotFound
		}
		return uuid.Nil, err
	}
	return id, nil
}

// Compile-time check.
var _ = errors.Is
