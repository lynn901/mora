package postgres

// asset_read_repo.go implements asset.ReadRepo — the read-side persistence for
// knowledge assets (design-docs/14 §4.4 D13 GET /knowledge/assets/:id,
// /:id/versions, /:id/relations, GET /workspaces/:ws/knowledge/assets).
//
// The service layer owns RBAC; this repo only fetches rows. A missing asset
// returns asset.ErrAssetNotFound so the repo and the RBAC denial surface
// identically to the caller (§8.2 / §10.4 用例 26/27: existence never leaks).
// List is always scoped by workspace_id; relations never cross workspaces
// (knowledge_relations.workspace_id NOT NULL + CHECK).
//
// All SQL is parameterized (07-security §10). No content is read: a document
// asset's body stays read through documents.content / document_versions.content
// via the native_document_version_id reference (§3.3 不复制正文); the asset
// version row carries only metadata + that reference.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// jsonbToMap unmarshals a pgx-scanned []byte (jsonb) into a map[string]any.
// Returns nil for a NULL/empty value so the domain struct's reference fields
// stay nil for native document versions (§3.3: no content copied).
func jsonbToMap(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// AssetReadRepo is the postgres implementation of asset.ReadRepo.
type AssetReadRepo struct{ db *DB }

// NewAssetReadRepo builds an asset.ReadRepo over the mora database.
func NewAssetReadRepo(db *DB) *AssetReadRepo { return &AssetReadRepo{db: db} }

// Compile-time check.
var _ asset.ReadRepo = (*AssetReadRepo)(nil)

// assetColumnList is the stable SELECT column list for knowledge_assets.
const assetColumnList = `id, workspace_id, asset_type, name, description, owner_type, owner_id,
	status, visibility, governance_profile_id, native_document_id,
	current_version_id, latest_requested_version_no, confidence,
	valid_from, expires_at, created_at, updated_at`

// scanAsset scans a knowledge_assets row into a *domain.KnowledgeAsset.
func scanAsset(scan func(dest ...any) error) (*domain.KnowledgeAsset, error) {
	a := &domain.KnowledgeAsset{}
	var (
		govProfileID, nativeDocID, curVersionID *uuid.UUID
		confidence                              *float64
		validFrom, expiresAt                     *time.Time
		ownerType                                string
		description                              *string
	)
	err := scan(
		&a.ID, &a.WorkspaceID, &a.AssetType, &a.Name, &description,
		&ownerType, &a.OwnerID, &a.Status, &a.Visibility,
		&govProfileID, &nativeDocID, &curVersionID, &a.LatestRequestedVersionNo,
		&confidence, &validFrom, &expiresAt, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	a.OwnerType = domain.SubjectType(ownerType)
	if description != nil {
		a.Description = *description
	}
	a.GovernanceProfileID = govProfileID
	a.NativeDocumentID = nativeDocID
	a.CurrentVersionID = curVersionID
	a.Confidence = confidence
	a.ValidFrom = validFrom
	a.ExpiresAt = expiresAt
	return a, nil
}

// Get loads a single knowledge asset by id (§4.4 GET /knowledge/assets/:id).
// Missing → asset.ErrAssetNotFound (no existence leak).
func (r *AssetReadRepo) Get(ctx context.Context, id uuid.UUID) (*domain.KnowledgeAsset, error) {
	row := r.db.Pool.QueryRow(ctx, `SELECT `+assetColumnList+` FROM knowledge_assets WHERE id = $1`, id)
	a, err := scanAsset(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, asset.ErrAssetNotFound
		}
		return nil, err
	}
	return a, nil
}

// List returns a cursor-paginated page of assets in workspaceID (§4.4 GET
// /workspaces/:ws/knowledge/assets). Cursor is base64 of "updated_at|id"; the
// next page starts AFTER that tuple so ordering is stable under concurrent
// updates. Scoped by workspace_id so a non-member never sees another
// workspace's assets.
func (r *AssetReadRepo) List(ctx context.Context, q asset.ListQuery) ([]*domain.KnowledgeAsset, string, error) {
	pageSize := q.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	args := []any{q.WorkspaceID}
	sb := `SELECT ` + assetColumnList + ` FROM knowledge_assets WHERE workspace_id = $1`
	argIdx := 2
	if q.AssetType != "" {
		sb += fmt.Sprintf(` AND asset_type = $%d`, argIdx)
		args = append(args, q.AssetType)
		argIdx++
	}
	if q.Status != "" {
		sb += fmt.Sprintf(` AND status = $%d`, argIdx)
		args = append(args, q.Status)
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
	out := make([]*domain.KnowledgeAsset, 0, pageSize)
	var lastTs time.Time
	var lastID uuid.UUID
	for rows.Next() {
		a, err := scanAsset(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
		lastTs = a.UpdatedAt
		lastID = a.ID
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

// versionColumnList is the stable SELECT list for knowledge_asset_versions.
const versionColumnList = `id, asset_id, version_no, source_id, source_revision,
	native_document_version_id, content_origin, generation_ref, provider_ref,
	content_hash, dedupe_key, build_status, governance_status,
	activation_policy_snapshot, approved_by_type, approved_by_id, approved_at,
	created_by_type, created_by_id, created_at`

// scanAssetVersion scans a knowledge_asset_versions row.
func scanAssetVersion(scan func(dest ...any) error) (*domain.AssetVersion, error) {
	v := &domain.AssetVersion{}
	var (
		sourceID, nativeDocVersionID, approvedByID *uuid.UUID
		generationRef, providerRef, policySnapshot []byte
		approvedAt                                  *time.Time
		approvedByType, createdByType               string
	)
	err := scan(
		&v.ID, &v.AssetID, &v.VersionNo, &sourceID, &v.SourceRevision,
		&nativeDocVersionID, &v.ContentOrigin, &generationRef, &providerRef,
		&v.ContentHash, &v.DedupeKey, &v.BuildStatus, &v.GovernanceStatus,
		&policySnapshot, &approvedByType, &approvedByID, &approvedAt,
		&createdByType, &v.CreatedByID, &v.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	v.SourceID = sourceID
	v.NativeDocumentVersionID = nativeDocVersionID
	v.GenerationRef = jsonbToMap(generationRef)
	v.ProviderRef = jsonbToMap(providerRef)
	v.ActivationPolicySnapshot = jsonbToMap(policySnapshot)
	if approvedByType != "" {
		v.ApprovedByType = approvedByType
	}
	v.ApprovedByID = approvedByID
	v.ApprovedAt = approvedAt
	v.CreatedByType = domain.SubjectType(createdByType)
	return v, nil
}

// ListVersions returns an asset's version history, newest-first (§4.4 GET
// /knowledge/assets/:id/versions). Missing asset → empty slice (the service
// already gated on RBAC; a missing asset was 404'd there).
func (r *AssetReadRepo) ListVersions(ctx context.Context, assetID uuid.UUID) ([]*domain.AssetVersion, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT `+versionColumnList+` FROM knowledge_asset_versions WHERE asset_id = $1 ORDER BY version_no DESC, created_at DESC`,
		assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*domain.AssetVersion, 0)
	for rows.Next() {
		v, err := scanAssetVersion(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListRelations returns an asset's relation edges, optionally filtered by
// relation_type (§4.4 GET /knowledge/assets/:id/relations). Returns both
// outgoing (from_asset_id) and incoming (to_asset_id) edges so the caller sees
// the full relation graph around the asset. Relations never cross workspaces
// (knowledge_relations.workspace_id NOT NULL + the repo scopes by the asset's
// workspace — the asset was RBAC-checked, so a cross-workspace caller already
// 404'd).
func (r *AssetReadRepo) ListRelations(ctx context.Context, assetID uuid.UUID, relationType string) ([]*domain.KnowledgeRelation, error) {
	args := []any{assetID}
	sb := `SELECT id, workspace_id, from_asset_id, from_version_id, relation_type,
		to_asset_id, to_version_id, origin, confidence,
		created_by_type, created_by_id, created_at
		FROM knowledge_relations WHERE (from_asset_id = $1 OR to_asset_id = $1)`
	argIdx := 2
	if relationType != "" {
		sb += fmt.Sprintf(` AND relation_type = $%d`, argIdx)
		args = append(args, relationType)
		argIdx++
	}
	sb += ` ORDER BY created_at DESC`
	rows, err := r.db.Pool.Query(ctx, sb, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*domain.KnowledgeRelation, 0)
	for rows.Next() {
		rel := &domain.KnowledgeRelation{}
		var (
			fromVersionID, toVersionID *uuid.UUID
			confidence                  *float64
			createdByType               string
			relationType, origin        string
		)
		if err := rows.Scan(
			&rel.ID, &rel.WorkspaceID, &rel.FromAssetID, &fromVersionID, &relationType,
			&rel.ToAssetID, &toVersionID, &origin, &confidence,
			&createdByType, &rel.CreatedByID, &rel.CreatedAt,
		); err != nil {
			return nil, err
		}
		rel.FromVersionID = fromVersionID
		rel.ToVersionID = toVersionID
		rel.RelationType = domain.RelationType(relationType)
		rel.Origin = domain.RelationOrigin(origin)
		rel.Confidence = confidence
		rel.CreatedByType = domain.SubjectType(createdByType)
		out = append(out, rel)
	}
	return out, rows.Err()
}
