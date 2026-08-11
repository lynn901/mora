package postgres

// asset_registry.go implements asset.Registry — the transactional write
// surface for registering a native document as a knowledge asset (design-docs/
// 14 §3.1 dual-write, §3.2 backfill, §3.4 legacy_migration governance).
//
// The same code path serves both the DocWriteSink dual-write (a user-authored
// new document/version) and the backfill runner (existing documents), because
// both produce the exact same rows: knowledge_assets (once per document) +
// knowledge_asset_versions (once per document version), with dedupe_key=
// 'document_version:'||version.id and current_version_id CAS-activated. The
// dedupe_key UNIQUE + (asset_id, native_document_version_id) UNIQUE make the
// two paths idempotent w.r.t. each other: if backfill already registered a
// document, the subsequent dual-write of its next version is a fresh version
// (different version_id) and proceeds; if a dual-write raced a backfill for the
// SAME version, the loser sees a unique violation and returns the existing ids.
//
// All SQL is parameterized (07-security §10). Content is never written — only
// the native_document_version_id reference (§3.3 不复制正文).

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// AssetRegistry is the postgres implementation of asset.Registry.
type AssetRegistry struct{}

// NewAssetRegistry builds an asset.Registry. Stateless: every write runs inside
// the caller's transaction, so one registry serves all producers (DocWriteSink,
// backfill, reconcile).
func NewAssetRegistry() *AssetRegistry { return &AssetRegistry{} }

// Compile-time check.
var _ asset.Registry = (*AssetRegistry)(nil)

// RegisterDocumentAsset idempotently registers a native document version as a
// Document asset version inside tx (§3.1/§3.2).
func (r *AssetRegistry) RegisterDocumentAsset(ctx context.Context, tx pgx.Tx, reg asset.Registration) (asset.Result, error) {
	if tx == nil {
		return asset.Result{}, errors.New("asset: register requires a transaction")
	}
	if reg.DocumentID == uuid.Nil || reg.VersionID == uuid.Nil || reg.WorkspaceID == uuid.Nil {
		return asset.Result{}, errors.New("asset: document_id, workspace_id and version_id are required")
	}
	dedupeKey := "document_version:" + reg.VersionID.String()

	// 1. Resolve / insert the knowledge_assets row for the document.
	//    uq_assets_native_doc (asset_type='document', native_document_id) makes
	//    this idempotent per document. ON CONFLICT DO NOTHING + a re-read gives us
	//    the existing asset_id without a separate lookup.
	assetID, _, err := upsertDocumentAsset(ctx, tx, reg)
	if err != nil {
		return asset.Result{}, err
	}

	// 2. Insert the asset version. (asset_id, dedupe_key) UNIQUE and
	//    uq_versions_native_doc_version make this idempotent per version. On a
	//    duplicate we return the existing version id with Created=false.
	versionRowID, versionCreated, err := insertDocumentAssetVersion(ctx, tx, assetID, reg, dedupeKey)
	if err != nil {
		return asset.Result{}, err
	}

	// 3. Bump latest_requested_version_no monotonically. ON CONFLICT on a
	//    re-registration (versionCreated=false) is a no-op; on a new version we
	//    advance the barrier only forward (GREATEST) so a late-finishing old
	//    version can never rewind it (§6.4 单调栅栏).
	if versionCreated {
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_assets
			SET latest_requested_version_no = GREATEST(latest_requested_version_no, $2),
			    updated_at = now()
			WHERE id = $1 AND latest_requested_version_no < $2`,
			assetID, reg.VersionNo); err != nil {
			return asset.Result{}, err
		}
	}

	// 4. CAS-activate current_version_id. backfill sets the initial value
	//    (current_version_id IS NULL guard); the latest_requested_version_no
	//    barrier prevents an old version from overwriting a newer one (§6.4).
	//    For dual-write of a brand-new version this advances the pointer only
	//    if the version_no is current — otherwise the WHERE clause matches 0
	//    rows and the existing pointer is preserved (no overwrite).
	if versionCreated {
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_assets
			SET current_version_id = $1, updated_at = now()
			WHERE id = $2
			  AND latest_requested_version_no = $3
			  AND current_version_id IS DISTINCT FROM $1`,
			versionRowID, assetID, reg.VersionNo); err != nil {
			return asset.Result{}, err
		}
	}

	// 5. legacy_migration system review record (§3.4) — only for backfill, i.e.
	//    when a migration service account is named. A user-authored dual-write
	//    needs no review row: native documents are published by default (§3.1).
	if versionCreated && reg.MigrationServiceAccountID != nil {
		if err := recordLegacyMigrationReview(ctx, tx, reg, assetID, versionRowID); err != nil {
			return asset.Result{}, err
		}
	}

	return asset.Result{
		AssetID:        assetID,
		VersionID:      reg.VersionID,
		AssetVersionID: versionRowID,
		Created:        versionCreated,
	}, nil
}

// LegacyMigrationProfileID returns the workspace's legacy_migration system
// governance profile id, creating it idempotently if missing (§2.2/§3.4).
func (r *AssetRegistry) LegacyMigrationProfileID(ctx context.Context, tx pgx.Tx, workspaceID domain.UUID) (domain.UUID, error) {
	if tx == nil {
		return uuid.Nil, errors.New("asset: legacy profile lookup requires a transaction")
	}
	var profileID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO governance_profiles
		  (workspace_id, name, asset_type, transition_rules, review_roles,
		   auto_publish, required_projections, is_system)
		VALUES ($1,'legacy_migration','document','{}'::jsonb,'[]'::jsonb,
		        '{"legacy_migration": true}'::jsonb,'["fts","vector"]'::jsonb,true)
		ON CONFLICT (workspace_id, name) DO UPDATE SET updated_at = now()
		RETURNING id`,
		workspaceID).Scan(&profileID)
	if err != nil {
		return uuid.Nil, err
	}
	return profileID, nil
}

// --- tx-scoped SQL ---

// upsertDocumentAsset inserts the knowledge_assets row for a document the
// first time it is registered, or returns the existing asset id. The partial
// unique index uq_assets_native_doc (asset_type='document', native_document_id)
// is the idempotency guard. status='published' because native documents are
// already published human content (§3.1).
func upsertDocumentAsset(ctx context.Context, tx pgx.Tx, reg asset.Registration) (uuid.UUID, bool, error) {
	var (
		id      uuid.UUID
		created bool
	)
	err := tx.QueryRow(ctx, `
		INSERT INTO knowledge_assets
		  (workspace_id, asset_type, name, owner_type, owner_id, status,
		   visibility, governance_profile_id, native_document_id,
		   latest_requested_version_no)
		VALUES ($1,'document',$2,$3,$4,'published','private',$5,$6,$7)
		ON CONFLICT (asset_type, native_document_id)
		    WHERE native_document_id IS NOT NULL
		DO UPDATE SET updated_at = now()
		RETURNING id, (xmax = 0) AS created`,
		reg.WorkspaceID, reg.Title, string(reg.CreatedByType), reg.CreatedByID,
		nilIfZero(reg.GovernanceProfileID), reg.DocumentID, reg.VersionNo,
	).Scan(&id, &created)
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, created, nil
}

// insertDocumentAssetVersion inserts a knowledge_asset_versions row for one
// document version. content_origin='human', build_status='ready' (a native
// document version is readable as soon as it commits), governance_status=
// 'published' (native documents default to published, §3.1). dedupe_key=
// 'document_version:'||version.id is the cross-path idempotency key.
// activation_policy_snapshot records the governance profile + required
// projections so the CAS gate at activation time is reproducible (§6.4).
//
// Idempotency: ON CONFLICT (asset_id, dedupe_key) DO NOTHING turns a re-register
// of the SAME version into a clean no-op (no transaction abort), after which we
// re-read the existing id. We target (asset_id, dedupe_key) because dedupe_key
// is derived from VersionID, so all well-formed re-registrations conflict on it;
// the (asset_id, version_no) and native_document_version_id constraints are
// redundant guards for the same identity. A conflict on RETURNING yields no row
// (pgx.ErrNoRows), which we translate into created=false via the re-read.
func insertDocumentAssetVersion(ctx context.Context, tx pgx.Tx, assetID uuid.UUID, reg asset.Registration, dedupeKey string) (uuid.UUID, bool, error) {
	snapshot := map[string]any{
		"governance_profile":   "legacy_migration",
		"required_projections": []string{"fts", "vector"},
		"auto_publish":         map[string]any{"legacy_migration": true},
	}
	snapshotJSON, _ := json.Marshal(snapshot)
	var (
		id      uuid.UUID
		created bool
	)
	err := tx.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, native_document_version_id, content_origin,
		   dedupe_key, build_status, governance_status, activation_policy_snapshot,
		   approved_by_type, approved_by_id, approved_at, created_by_type, created_by_id)
		VALUES ($1,$2,$3,'human',$4,'ready','published',$5,$6,$7,now(),$8,$9)
		ON CONFLICT (asset_id, dedupe_key) DO NOTHING
		RETURNING id, (xmax = 0) AS created`,
		assetID, reg.VersionNo, reg.VersionID, dedupeKey, snapshotJSON,
		string(reg.CreatedByType), reg.CreatedByID,
		string(reg.CreatedByType), reg.CreatedByID,
	).Scan(&id, &created)
	if err != nil {
		// No rows → ON CONFLICT matched: the version already exists. Re-read its
		// id in the same (still-healthy) transaction. Any other error is real.
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				SELECT id FROM knowledge_asset_versions
				WHERE asset_id = $1 AND dedupe_key = $2`,
				assetID, dedupeKey).Scan(&id)
			if err != nil {
				return uuid.Nil, false, err
			}
			return id, false, nil
		}
		return uuid.Nil, false, err
	}
	return id, created, nil
}

// recordLegacyMigrationReview writes the §3.4 system review_request (approved)
// + review_decision for a backfilled version. requested_by_type=
// service_account, rationale='legacy_migration backfill'; decision='approve',
// decision_by_type=service_account, policy_version='legacy_migration-v1'. The
// request does not enter a team inbox (review_roles=[] on the profile).
//
// Idempotent: review_requests has no unique constraint beyond its PK, so a
// re-run is guarded by checking whether an approved legacy_migration request
// already exists for this version. (RegisterDocumentAsset also only calls this
// when versionCreated=true, but the existence check is defense-in-depth for a
// caller that re-registers after a partial commit.)
func recordLegacyMigrationReview(ctx context.Context, tx pgx.Tx, reg asset.Registration, assetID, versionRowID uuid.UUID) error {
	var existing uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM review_requests
		WHERE asset_version_id = $1
		  AND status = 'approved'
		  AND rationale = 'legacy_migration backfill'
		LIMIT 1`, versionRowID).Scan(&existing)
	switch {
	case err == nil:
		return nil // already recorded for this version
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	var reqID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO review_requests
		  (workspace_id, asset_id, asset_version_id, governance_profile_id,
		   requested_by_type, requested_by_id, status, rationale)
		VALUES ($1,$2,$3,$4,'service_account',$5,'approved','legacy_migration backfill')
		RETURNING id`,
		reg.WorkspaceID, assetID, versionRowID, nilIfZero(reg.GovernanceProfileID),
		reg.MigrationServiceAccountID,
	).Scan(&reqID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO review_decisions
		  (review_request_id, decision, decision_by_type, decision_by_id,
		   policy_version, rationale_redacted)
		VALUES ($1,'approve','service_account',$2,'legacy_migration-v1','legacy_migration backfill')`,
		reqID, reg.MigrationServiceAccountID)
	return err
}
