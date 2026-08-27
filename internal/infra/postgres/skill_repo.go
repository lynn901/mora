package postgres

// skill_repo.go implements skill.Repository over skill_packages (Phase 5-3 /
// YS-163, design-docs/19 §3.1 / §6.1). It mounts 1:1 on
// knowledge_asset_versions: the PK is asset_version_id, so Get/Save/
// UpdateValidationReport key off it directly. JSONB columns (manifest,
// original_frontmatter, signature, provenance_ref, validation_report,
// compatibility_report) are scanned/marshaled via the same jsonMap/jsonBytes
// helpers the memory evidence repo uses — no special pgx json binding.
//
// Layering (modular monolith): the skill service declares the Repository port;
// this file is the pgx implementation. The service stays pgx-free (same
// precedent as binding_repo.go over its sink). The ArchiveOpener adapter that
// bridges objstore.Store → skill.ArchiveOpener lives here too, so the service's
// archive seam is wired without the service importing infra/objstore.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/lynn901/mora/internal/platform/rbac"
	skill "github.com/lynn901/mora/internal/module/skill"
)

// Compile-time checks.
var (
	_ skill.Repository = (*SkillRepo)(nil)
)

// SkillRepo implements skill.Repository over the skill_packages table.
type SkillRepo struct{ db *DB }

// NewSkillRepo wires the skill packages repository.
func NewSkillRepo(db *DB) *SkillRepo { return &SkillRepo{db: db} }

// skillColumns is the canonical SELECT list (matches migration 022 column
// order). Kept in sync with scanSkillPackage's field order.
const skillColumns = `asset_version_id, storage_key, format_id, schema_version,
	manifest, original_frontmatter, content_hash, signature, provenance_ref,
	validation_status, validation_report, compatibility_report, scanner_version,
	created_at, updated_at`

// scanSkillPackage scans one skill_packages row into a domain.SkillPackage.
// JSONB columns come back as raw bytes and are unmarshaled into their value
// types; NULL jsonb (original_frontmatter/signature/provenance_ref may be NULL)
// degrades to empty/nil shapes (no panic, no leak).
func scanSkillPackage(row pgx.Row) (domain.SkillPackage, error) {
	var (
		pkg              domain.SkillPackage
		formatID         string
		schemaVersion    string
		manifestBytes    []byte
		frontmatterBytes []byte
		signatureBytes   []byte
		provenanceBytes  []byte
		valStatus        string
		valReportBytes   []byte
		compatBytes      []byte
		scannerVersion   string
	)
	if err := row.Scan(
		&pkg.AssetVersionID, &pkg.StorageKey, &formatID, &schemaVersion,
		&manifestBytes, &frontmatterBytes, &pkg.ContentHash, &signatureBytes, &provenanceBytes,
		&valStatus, &valReportBytes, &compatBytes, &scannerVersion,
		&pkg.CreatedAt, &pkg.UpdatedAt); err != nil {
		return pkg, err
	}
	pkg.FormatID = domain.SkillFormatID(formatID)
	pkg.SchemaVersion = schemaVersion
	pkg.ScannerVersion = scannerVersion
	pkg.ValidationStatus = domain.SkillValidationStatus(valStatus)
	if err := unmarshalSkillJSON(manifestBytes, &pkg.Manifest); err != nil {
		return pkg, err
	}
	if len(frontmatterBytes) > 0 {
		_ = json.Unmarshal(frontmatterBytes, &pkg.OriginalFrontmatter) // best-effort; NULL → nil map
	}
	if len(signatureBytes) > 0 {
		_ = json.Unmarshal(signatureBytes, &pkg.Signature)
	}
	if len(provenanceBytes) > 0 {
		_ = json.Unmarshal(provenanceBytes, &pkg.ProvenanceRef)
	}
	if err := unmarshalSkillJSON(valReportBytes, &pkg.ValidationReport); err != nil {
		return pkg, err
	}
	if err := unmarshalSkillJSON(compatBytes, &pkg.CompatibilityReport); err != nil {
		return pkg, err
	}
	return pkg, nil
}

// unmarshalSkillJSON unmarshals a JSONB column into a non-pointer value. An
// empty/NULL payload leaves the zero value (empty slices/maps) — matching the
// table DEFAULT '{}' for NOT NULL jsonb and NULL for nullable jsonb.
func unmarshalSkillJSON[T any](b []byte, v *T) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("skill: unmarshal jsonb: %w", err)
	}
	return nil
}

// Get returns the skill package mounted on an asset version. A missing row
// maps to skill.ErrPackageNotFound so the service surfaces 404 (no existence
// leak — same convention as binding.ErrBindingNotFound).
func (r *SkillRepo) Get(ctx context.Context, assetVersionID uuid.UUID) (domain.SkillPackage, error) {
	row := r.db.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM skill_packages WHERE asset_version_id = $1`, skillColumns),
		assetVersionID)
	pkg, err := scanSkillPackage(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pkg, skill.ErrPackageNotFound
		}
		return pkg, err
	}
	return pkg, nil
}

// Save upserts a skill package (1:1 on asset_version_id). The import path calls
// it once with the full package (manifest + reports + status). ON CONFLICT
// (asset_version_id) DO UPDATE keeps the row idempotent if the same version is
// re-imported (the content_hash anchor makes a re-import detectable upstream).
func (r *SkillRepo) Save(ctx context.Context, pkg domain.SkillPackage) error {
	manifestJSON, _ := json.Marshal(pkg.Manifest)
	valReportJSON, _ := json.Marshal(pkg.ValidationReport)
	compatJSON, _ := json.Marshal(pkg.CompatibilityReport)
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO skill_packages
		  (asset_version_id, storage_key, format_id, schema_version,
		   manifest, original_frontmatter, content_hash, signature, provenance_ref,
		   validation_status, validation_report, compatibility_report,
		   scanner_version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (asset_version_id) DO UPDATE SET
		  storage_key = EXCLUDED.storage_key,
		  format_id = EXCLUDED.format_id,
		  schema_version = EXCLUDED.schema_version,
		  manifest = EXCLUDED.manifest,
		  original_frontmatter = EXCLUDED.original_frontmatter,
		  content_hash = EXCLUDED.content_hash,
		  signature = EXCLUDED.signature,
		  provenance_ref = EXCLUDED.provenance_ref,
		  validation_status = EXCLUDED.validation_status,
		  validation_report = EXCLUDED.validation_report,
		  compatibility_report = EXCLUDED.compatibility_report,
		  scanner_version = EXCLUDED.scanner_version,
		  updated_at = now()`,
		pkg.AssetVersionID, pkg.StorageKey, string(pkg.FormatID), pkg.SchemaVersion,
		manifestJSON, jsonBytes(pkg.OriginalFrontmatter), pkg.ContentHash,
		jsonBytes(pkg.Signature), jsonBytes(pkg.ProvenanceRef),
		string(pkg.ValidationStatus), valReportJSON, compatJSON,
		pkg.ScannerVersion, pkg.CreatedAt, pkg.UpdatedAt)
	return err
}

// UpdateValidationReport updates validation_status + validation_report +
// scanner_version for a stored package (the revalidate path). updated_at is
// bumped so a future ETag-on the version row reflects the rescan. A missing
// row maps to skill.ErrPackageNotFound (no leak).
func (r *SkillRepo) UpdateValidationReport(ctx context.Context, assetVersionID uuid.UUID, status domain.SkillValidationStatus, report domain.ValidationReport, scannerVersion string) error {
	valReportJSON, _ := json.Marshal(report)
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE skill_packages
		SET validation_status = $2, validation_report = $3, scanner_version = $4, updated_at = now()
		WHERE asset_version_id = $1`,
		assetVersionID, string(status), valReportJSON, scannerVersion)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return skill.ErrPackageNotFound
	}
	return nil
}

// --- ArchiveOpener adapter (objstore.Store → skill.ArchiveOpener) ---

// SkillArchiveOpener adapts an objstore.Store to the skill.ArchiveOpener seam.
// objstore.Store.Read returns the full archive bytes ([]byte); the opener
// wraps them in an io.NopCloser so the service's parse/export path — which
// expects an io.ReadCloser — stays storage-agnostic. Mora never synthesizes
// an executable bit here: the bytes are the immutable archive original as
// stored (§4.4 / §7).
//
// A nil store (object storage not wired, e.g. dev without MinIO) returns
// objstore.ErrNotConfigured; the handler maps that to a 503 so import/export
// degrade cleanly rather than crash.
type SkillArchiveOpener struct{ Store *objstore.Store }

// NewSkillArchiveOpener wires the objstore-backed archive opener.
func NewSkillArchiveOpener(store *objstore.Store) *SkillArchiveOpener {
	return &SkillArchiveOpener{Store: store}
}

// OpenArchive opens a streaming reader over the immutable archive original.
// The caller MUST Close it; io.NopCloser makes Close a no-op (the bytes are
// already materialized). Script-execution count stays 0 — no exec bit is
// honored (§4.4).
func (a *SkillArchiveOpener) OpenArchive(storageKey string) (io.ReadCloser, error) {
	if a == nil || a.Store == nil {
		return nil, objstore.ErrNotConfigured
	}
	b, err := a.Store.Read(context.Background(), storageKey)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// --- SkillAuthz: rbacEngine adapter (skill service authz seam) ---

// SkillAuthzAdapter adapts the real rbac.Engine to the skill service's
// rbacEngine seam (CheckAssign). It resolves asset_version_id → workspace_id
// via the asset read repo's GetVersionByID (mirrors the memory-unit ACL gate:
// resolve the asset id before Check — YS-146 — so a non-owner caller with a
// grant on the workspace is allowed while a bare unit/version id never matches
// the AssetLocator's resolved asset id). A denial returns skill.ErrPackageNotFound
// so the service surfaces 404 (no existence leak, §8.2).
//
// The adapter is the production wiring point B left for D: the service stays
// platform/rbac-free, this adapter bridges the two.
type SkillAuthzAdapter struct {
	engine    *rbac.Engine
	versions  AssetVersionResolver
}

// AssetVersionResolver resolves an asset version id to its owning workspace
// id. The asset read repo's GetVersionByID satisfies this (returns workspaceID).
// A missing version returns ErrAssetNotFound (no leak).
type AssetVersionResolver interface {
	GetVersionByID(ctx context.Context, versionID uuid.UUID) (*domain.AssetVersion, uuid.UUID, error)
}

// NewSkillAuthzAdapter wires the skill authz adapter. versions is the asset
// read repo (or any AssetVersionResolver); engine is the rbac.Engine.
func NewSkillAuthzAdapter(engine *rbac.Engine, versions AssetVersionResolver) *SkillAuthzAdapter {
	return &SkillAuthzAdapter{engine: engine, versions: versions}
}

// CheckAssign resolves assetVersionID → workspaceID, then runs
// rbac.Engine.Check for ActionAssign on TargetWorkspace. A missing version or
// a denial returns skill.ErrPackageNotFound (no existence leak). An admin is
// short-circuited by the caller (requireAuthorized) before this is reached.
func (a *SkillAuthzAdapter) CheckAssign(ctx context.Context, assetVersionID uuid.UUID, subject domain.SubjectType, principalID uuid.UUID, groupIDs []uuid.UUID, isAdmin bool) error {
	_, workspaceID, err := a.versions.GetVersionByID(ctx, assetVersionID)
	if err != nil {
		return skill.ErrPackageNotFound
	}
	dec, err := a.engine.Check(ctx, principalID, groupIDs, domain.TargetWorkspace, workspaceID, domain.ActionAssign)
	if err != nil || !dec.Allowed {
		return skill.ErrPackageNotFound
	}
	return nil
}

// --- SkillAssetRegistrar: import-path asset+version creation ---

// SkillRegistrar implements skill.SkillAssetRegistrar: creates the
// knowledge_assets (asset_type=skill) + knowledge_asset_versions rows in one
// tx, and stores the immutable archive original in MinIO. A skill asset has
// no native_document_id (document-only); the version's content_origin=
// 'imported', build_status='ready' (an imported skill is immediately saveable
// — skill_packages.validation_status tracks structural validity, separately),
// governance_status='candidate' (awaits the standard review inbox per §3.1).
//
// Idempotency: the asset's partial unique index uq_assets_native_doc only
// covers native_document_id IS NOT NULL; skills have it NULL, so a skill
// asset is keyed by (workspace_id, name) at the application layer — a re-
// import of the same name creates a new version on the existing asset (name
// is the natural identity, mirroring how a document's native id identifies
// its asset). The version's dedupe_key is the archive content_hash so a
// re-import of the exact same bytes is a no-op on the version row.
type SkillRegistrar struct {
	pool   *pgxpool.Pool
	store  *objstore.Store
}

// NewSkillRegistrar wires the skill asset registrar. store may be nil when
// object storage is not wired (dev); RegisterSkillAsset then returns
// objstore.ErrNotConfigured so the handler degrades to 503, not a crash.
func NewSkillRegistrar(pool *pgxpool.Pool, store *objstore.Store) *SkillRegistrar {
	return &SkillRegistrar{pool: pool, store: store}
}

// SkillProposalSink implements skill.ProposalSink (§6.3 skill_propose). It
// creates a CANDIDATE knowledge_asset + knowledge_asset_version
// (governance_status='candidate', build_status='ready') + a pending
// review_request in one tx, storing the draft archive verbatim in MinIO. It
// NEVER publishes or binds the skill — the candidate enters the governance
// review flow; a human promotes it (import + validate + publish) via §6.1.
//
// No script execution occurs (§4.4): the draft archive bytes are stored
// verbatim, never materialized with an exec bit. Validation is NOT run on the
// proposal path — the static validator runs on the management import path
// (§6.1, `assign`-gated), not the agent candidate path.
type SkillProposalSink struct {
	pool  *pgxpool.Pool
	store *objstore.Store
}

// NewSkillProposalSink wires the skill candidate-proposal sink. store may be
// nil when object storage is not wired (dev); Submit then surfaces
// objstore.ErrNotConfigured so the handler degrades to 503, not a leak.
func NewSkillProposalSink(pool *pgxpool.Pool, store *objstore.Store) *SkillProposalSink {
	return &SkillProposalSink{pool: pool, store: store}
}

// Compile-time check.
var _ skill.ProposalSink = (*SkillProposalSink)(nil)

// Submit creates the candidate asset + version + pending review_request.
func (s *SkillProposalSink) Submit(ctx context.Context, in skill.ProposalInput) (skill.ProposalResult, error) {
	if s.store == nil {
		return skill.ProposalResult{}, objstore.ErrNotConfigured
	}
	if in.WorkspaceID == uuid.Nil || in.Name == "" || len(in.DraftArchive) == 0 {
		return skill.ProposalResult{}, skill.ErrInvalidProposal
	}
	// Store the draft archive verbatim (content-addressed). No exec bit —
	// MinIO objects carry no POSIX mode (§4.4).
	contentHash := sha256HexBytes(in.DraftArchive)
	storageKey := fmt.Sprintf("skills/proposal/%s/%s.tar.gz", in.WorkspaceID, contentHash)
	if _, err := s.store.Put(ctx, storageKey, "application/gzip", in.DraftArchive); err != nil {
		return skill.ProposalResult{}, fmt.Errorf("skill: store draft archive: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return skill.ProposalResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	ownerType := string(in.SubmittedBy.Type)
	if ownerType == "" {
		ownerType = string(domain.SubjectAgent)
	}
	ownerID := in.SubmittedBy.ID
	if ownerID == uuid.Nil {
		return skill.ProposalResult{}, skill.ErrInvalidProposal
	}

	// Ensure a skill governance profile exists for the workspace (idempotent).
	// review_roles=[] keeps the proposal out of an auto-promote path; a human
	// must review it. transition_rules={} means no auto-publish on candidate.
	profileID, err := ensureSkillProfile(ctx, tx, in.WorkspaceID)
	if err != nil {
		return skill.ProposalResult{}, fmt.Errorf("skill: ensure profile: %w", err)
	}

	// Create the candidate asset (asset_type=skill, status=draft,
	// visibility=private). A proposal re-using an existing name bumps the
	// version_no; a fresh name creates a new asset.
	var (
		assetID   uuid.UUID
		versionNo int64
	)
	err = tx.QueryRow(ctx, `
		SELECT id, latest_requested_version_no FROM knowledge_assets
		WHERE workspace_id = $1 AND asset_type = 'skill' AND name = $2`,
		in.WorkspaceID, in.Name).Scan(&assetID, &versionNo)
	if errors.Is(err, pgx.ErrNoRows) {
		versionNo = 1
		if err := tx.QueryRow(ctx, `
			INSERT INTO knowledge_assets
			  (workspace_id, asset_type, name, description, owner_type, owner_id,
			   status, visibility, governance_profile_id, latest_requested_version_no)
			VALUES ($1,'skill',$2,$3,$4,$5,'draft','private',$6,1)
			RETURNING id`,
			in.WorkspaceID, in.Name, nullableStr(in.Description), ownerType, ownerID,
			profileID).Scan(&assetID); err != nil {
			return skill.ProposalResult{}, err
		}
	} else if err != nil {
		return skill.ProposalResult{}, err
	} else {
		versionNo++
		if _, err := tx.Exec(ctx, `
			UPDATE knowledge_assets
			SET latest_requested_version_no = $2, updated_at = now()
			WHERE id = $1`, assetID, versionNo); err != nil {
			return skill.ProposalResult{}, err
		}
	}

	// Insert the candidate version. governance_status='candidate' so the
	// asset-version is NOT authorized for delivery until a human publishes it
	// (the authz lifecycle gate treats only 'published' as authorized).
	// dedupe_key = 'skill_proposal:'||content_hash so a re-proposal of the
	// same draft bytes is a no-op (returns the existing version id).
	var versionID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, content_hash, dedupe_key,
		   build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1,$2,'imported',$3,$4,'ready','candidate',$5,$6)
		ON CONFLICT (asset_id, dedupe_key) DO UPDATE SET updated_at = now()
		RETURNING id`,
		assetID, versionNo, contentHash, "skill_proposal:"+contentHash,
		ownerType, ownerID).Scan(&versionID)
	if err != nil {
		return skill.ProposalResult{}, err
	}

	// Insert the pending review_request. status='pending' — the proposal
	// awaits a human governance decision (approve → publish, reject → drop).
	// requested_by carries the acting agent principal.
	var reviewID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO review_requests
		  (workspace_id, asset_id, asset_version_id, governance_profile_id,
		   requested_by_type, requested_by_id, status, rationale)
		VALUES ($1,$2,$3,$4,$5,$6,'pending','skill_propose candidate')
		RETURNING id`,
		in.WorkspaceID, assetID, versionID, profileID,
		ownerType, ownerID).Scan(&reviewID)
	if err != nil {
		return skill.ProposalResult{}, fmt.Errorf("skill: insert review_request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return skill.ProposalResult{}, err
	}
	return skill.ProposalResult{
		AssetID:         assetID,
		AssetVersionID:  versionID,
		ReviewRequestID: reviewID,
		StorageKey:      storageKey,
	}, nil
}

// ensureSkillProfile idempotently ensures a skill governance profile exists for
// the workspace and returns its id. transition_rules='{}' + review_roles='[]'
// + auto_publish='{}' means no auto-promote: a human must review + approve a
// candidate before it can publish. is_system=true marks it the platform's
// default skill profile (not a user-authored policy).
func ensureSkillProfile(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID) (uuid.UUID, error) {
	var profileID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO governance_profiles
		  (workspace_id, name, asset_type, transition_rules, review_roles,
		   auto_publish, required_projections, is_system)
		VALUES ($1,'skill_default','skill','{}'::jsonb,'[]'::jsonb,
		        '{}'::jsonb,'[]'::jsonb,true)
		ON CONFLICT (workspace_id, name) DO UPDATE SET updated_at = now()
		RETURNING id`,
		workspaceID).Scan(&profileID)
	if err != nil {
		return uuid.Nil, err
	}
	return profileID, nil
}

// Compile-time check.
var _ skill.SkillAssetRegistrar = (*SkillRegistrar)(nil)

// RegisterSkillAsset stores the archive in MinIO, then in one tx: upserts the
// knowledge_assets row (asset_type=skill, keyed by workspace+name) and inserts
// the knowledge_asset_versions row (dedupe_key=content_hash). Returns the ids
// + the storage_key the ArchiveOpener reads. Script-execution count stays 0:
// the archive bytes are stored verbatim, never materialized with an exec bit.
func (r *SkillRegistrar) RegisterSkillAsset(ctx context.Context, in skill.RegisterSkillInput) (skill.RegisterSkillResult, error) {
	if r.store == nil {
		return skill.RegisterSkillResult{}, objstore.ErrNotConfigured
	}
	// 1. Compute the content_hash (sha256 of the archive bytes) — the
	//    roundtrip anchor (§3.1) + the version's dedupe_key.
	contentHash := sha256HexBytes(in.Archive)

	// 2. Store the immutable archive original in MinIO. The storage_key is
	//    workspace-scoped + content-addressed so the same bytes land once
	//    (MinIO Put on the same key is idempotent). No exec bit is set —
	//    MinIO objects carry no POSIX mode (§4.4).
	storageKey := fmt.Sprintf("skills/%s/%s.tar.gz", in.WorkspaceID, contentHash)
	if _, err := r.store.Put(ctx, storageKey, "application/gzip", in.Archive); err != nil {
		return skill.RegisterSkillResult{}, fmt.Errorf("skill: store archive: %w", err)
	}

	// 3. In one tx: upsert the asset (keyed by workspace+name) + insert the
	//    version (dedupe_key=content_hash). ON CONFLICT on the version's
	//    (asset_id, dedupe_key) makes a re-import of the same bytes a no-op
	//    that returns the existing version id.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return skill.RegisterSkillResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	ownerType := string(in.CreatedBy.Type)
	if ownerType == "" {
		ownerType = string(domain.SubjectUser)
	}
	ownerID := in.CreatedBy.ID
	if ownerID == uuid.Nil {
		return skill.RegisterSkillResult{}, fmt.Errorf("skill: import requires a created_by principal")
	}

	// Upsert the skill asset. There is no partial unique index for skills
	// (uq_assets_native_doc only covers native_document_id IS NOT NULL, which
	// is NULL for skills), so (workspace_id, asset_type='skill', name) is the
	// natural identity at the application layer. SELECT-then-INSERT under the
	// tx: a concurrent insert of the same name races — the loser's tx will see
	// the winner's row on re-SELECT (READ COMMITTED + the tx's statement
	// snapshot re-evaluates per-statement). For a management-path low-volume
	// write this is sufficient; a true race aborts with a clear error.
	var assetID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id, latest_requested_version_no FROM knowledge_assets
		WHERE workspace_id = $1 AND asset_type = 'skill' AND name = $2`,
		in.WorkspaceID, in.Name).Scan(&assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		// New skill asset.
		if err := tx.QueryRow(ctx, `
			INSERT INTO knowledge_assets
			  (workspace_id, asset_type, name, description, owner_type, owner_id,
			   status, visibility, latest_requested_version_no)
			VALUES ($1,'skill',$2,$3,$4,$5,'draft','private',1)
			RETURNING id`,
			in.WorkspaceID, in.Name, nullableStr(in.Description), ownerType, ownerID).Scan(&assetID); err != nil {
			return skill.RegisterSkillResult{}, err
		}
	} else if err != nil {
		return skill.RegisterSkillResult{}, err
	} else {
		// Existing skill asset — bump latest_requested_version_no so the new
		// version_no is next (mirrors the document path's bump on re-register).
		if err := tx.QueryRow(ctx, `
			UPDATE knowledge_assets
			SET latest_requested_version_no = latest_requested_version_no + 1,
			    updated_at = now()
			WHERE id = $1
			RETURNING latest_requested_version_no`, assetID).Scan(new(int64)); err != nil {
			return skill.RegisterSkillResult{}, err
		}
	}

	// The version_no: 1 for a new asset, else latest_requested_version_no (just
	// bumped). Re-read to be safe for the new-asset path (latest_requested=1).
	var versionNo int64
	if err := tx.QueryRow(ctx, `SELECT latest_requested_version_no FROM knowledge_assets WHERE id = $1`, assetID).Scan(&versionNo); err != nil {
		return skill.RegisterSkillResult{}, err
	}

	// Insert the version. dedupe_key = content_hash so re-importing the same
	// archive bytes is a no-op (ON CONFLICT DO NOTHING → re-read existing id).
	// content_origin='imported', build_status='ready', governance_status=
	// 'candidate' (awaits review per §3.1).
	var versionID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, content_origin, content_hash, dedupe_key,
		   build_status, governance_status, created_by_type, created_by_id)
		VALUES ($1,$2,'imported',$3,$4,'ready','candidate',$5,$6)
		ON CONFLICT (asset_id, dedupe_key) DO NOTHING
		RETURNING id`,
		assetID, versionNo, contentHash, "skill_archive:"+contentHash,
		ownerType, ownerID).Scan(&versionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT matched: the same archive bytes already imported.
			// Re-read the existing version id.
			if err := tx.QueryRow(ctx, `
				SELECT id FROM knowledge_asset_versions
				WHERE asset_id = $1 AND dedupe_key = $2`,
				assetID, "skill_archive:"+contentHash).Scan(&versionID); err != nil {
				return skill.RegisterSkillResult{}, err
			}
		} else {
			return skill.RegisterSkillResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return skill.RegisterSkillResult{}, err
	}
	return skill.RegisterSkillResult{AssetID: assetID, AssetVersionID: versionID, StorageKey: storageKey}, nil
}

// nullableStr returns nil for an empty string so a NULL-able TEXT column
// (knowledge_assets.description) stays NULL when the caller omitted it.
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// sha256HexBytes returns the hex sha256 of b. Used as the archive content_hash
// (the §3.1 roundtrip anchor) and the version's dedupe_key so a re-import of
// the same bytes is a no-op.
func sha256HexBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- Delivery path adapters (§6.2 internal API) ---

// SkillAssetResolver adapts AssetReadRepo to the skill.DeliveryService's
// AssetResolver seam. GetAsset dereferences the *KnowledgeAsset pointer
// (AssetReadRepo.Get returns *domain.KnowledgeAsset); a not-found maps to
// skill.ErrPackageNotFound so the delivery path surfaces 404 (no leak).
// ResolveVersion delegates to AssetReadRepo.ResolveVersion, mapping
// asset.ErrAssetNotFound → skill.ErrPackageNotFound for the same reason.
type SkillAssetResolver struct{ assets *AssetReadRepo }

// NewSkillAssetResolver wires the delivery-path asset resolver over the asset
// read repo.
func NewSkillAssetResolver(assets *AssetReadRepo) *SkillAssetResolver {
	return &SkillAssetResolver{assets: assets}
}

// GetAsset returns the knowledge asset (type + workspace) for assetID. A
// missing asset surfaces as skill.ErrPackageNotFound (no existence leak).
func (a *SkillAssetResolver) GetAsset(ctx context.Context, assetID uuid.UUID) (domain.KnowledgeAsset, error) {
	asset, err := a.assets.Get(ctx, assetID)
	if err != nil || asset == nil {
		return domain.KnowledgeAsset{}, skill.ErrPackageNotFound
	}
	return *asset, nil
}

// ResolveVersion resolves a version spec to a concrete asset version. A missing
// version surfaces as skill.ErrPackageNotFound (no leak).
func (a *SkillAssetResolver) ResolveVersion(ctx context.Context, assetID uuid.UUID, versionSpec string) (domain.AssetVersion, error) {
	v, err := a.assets.ResolveVersion(ctx, assetID, versionSpec)
	if err != nil {
		return domain.AssetVersion{}, skill.ErrPackageNotFound
	}
	return v, nil
}

// ListSkillsByWorkspace returns the skill-typed knowledge assets in a workspace
// (skill_list backing, §6.3). It pages through AssetReadRepo.List with
// asset_type='skill' until exhausted; the per-workspace skill set is naturally
// bounded so this is a bounded walk, not an unbounded scan. The repo scopes by
// workspace_id so a non-member never sees another workspace's skills. A repo
// fault surfaces as skill.ErrPackageNotFound so the delivery path returns an
// empty list (no leak — a transient fault never reveals skill existence).
func (a *SkillAssetResolver) ListSkillsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]domain.KnowledgeAsset, error) {
	var out []domain.KnowledgeAsset
	cursor := ""
	for {
		page, next, err := a.assets.List(ctx, asset.ListQuery{
			WorkspaceID: workspaceID,
			AssetType:   string(domain.AssetTypeSkill),
			Cursor:      cursor,
			PageSize:    100,
		})
		if err != nil {
			return nil, skill.ErrPackageNotFound
		}
		for _, a := range page {
			out = append(out, *a)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// SkillBindingResolver adapts BindingRepo to the skill.DeliveryService's
// BindingResolver seam (ActiveForAgent). It is a thin pass-through so the skill
// delivery service does not import module/binding directly.
type SkillBindingResolver struct{ bindings *BindingRepo }

// NewSkillBindingResolver wires the delivery-path binding resolver over the
// binding repo.
func NewSkillBindingResolver(bindings *BindingRepo) *SkillBindingResolver {
	return &SkillBindingResolver{bindings: bindings}
}

// ActiveForAgent returns all active bindings for (agent, workspace). The
// delivery service resolves the §5.3 effective winner in-memory.
func (r *SkillBindingResolver) ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error) {
	return r.bindings.ActiveForAgent(ctx, agentID, workspaceID)
}
