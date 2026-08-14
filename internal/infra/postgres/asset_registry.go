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
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// AssetRegistry is the postgres implementation of asset.Registry.
type AssetRegistry struct {
	// pool is used only by ReconcileScan, which does not take a caller tx — it
	// opens its own short transactions. nil is fine for the tx-scoped methods
	// (RegisterDocumentAsset / MarkProjectionReady / Activate), which run
	// inside the caller's transaction.
	pool *pgxpool.Pool
}

// NewAssetRegistry builds an asset.Registry. The tx-scoped write methods run
// inside the caller's transaction, so no pool is needed for them. ReconcileScan
// needs a pool — wire one with WithPool on the worker that runs the reconcile
// ticker; mora-api/backfill callers that never reconcile can leave it nil.
func NewAssetRegistry() *AssetRegistry { return &AssetRegistry{} }

// WithPool attaches a connection pool so ReconcileScan can run. Returns the
// receiver for chaining. Idempotent.
func (r *AssetRegistry) WithPool(pool *pgxpool.Pool) *AssetRegistry {
	r.pool = pool
	return r
}

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

// --- §7 CAS activation path (design-docs/14 §7) ---

// MarkProjectionReady flips an asset_projections row to status='ready' and,
// when all required projections for the asset version are ready, flips
// knowledge_asset_versions.build_status='ready' (§7). It is the rag-worker /
// Provider write-back: the build runs out-of-band, this marks it done so the
// activation CAS can gate on it.
//
// Idempotent: re-marking an already-ready projection is a no-op. The
// (asset_version_id, projection_kind, build_revision) UNIQUE means a rebuild
// produces a NEW row (new build_revision) — the old row is left as-is and the
// new one becomes the ready marker; activation gates on "no pending/failed/
// building required projection" so a stale old row does not block.
//
// required_projections is read from the version's
// activation_policy_snapshot (§6.4: the snapshot pins the gate so a later
// profile edit can't retroactively change what was required). When the snapshot
// is missing/empty, the default document gate is ["fts","vector"] (§7).
func (r *AssetRegistry) MarkProjectionReady(ctx context.Context, tx pgx.Tx, assetVersionID domain.UUID, kind domain.ProjectionKind, provider, buildRevision string, locator map[string]any) error {
	if tx == nil {
		return errors.New("asset: MarkProjectionReady requires a transaction")
	}
	if assetVersionID == uuid.Nil || kind == "" || buildRevision == "" {
		return errors.New("asset: asset_version_id, kind and build_revision are required")
	}
	locatorJSON, _ := json.Marshal(locator)

	// 1. Upsert the ready projection row. ON CONFLICT (asset_version_id,
	//    projection_kind, build_revision) DO UPDATE status='ready' makes this
	//    idempotent (a re-delivery re-marks the same build ready, no churn).
	var projID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO asset_projections
		  (asset_version_id, projection_kind, provider, build_revision,
		   status, locator, built_at, last_error)
		VALUES ($1,$2,$3,$4,'ready',$5,now(),NULL)
		ON CONFLICT (asset_version_id, projection_kind, build_revision)
		DO UPDATE SET status='ready', locator=$5, built_at=now(), last_error=NULL
		RETURNING id`,
		assetVersionID, string(kind), provider, buildRevision, locatorJSON,
	).Scan(&projID)
	if err != nil {
		return fmt.Errorf("asset: upsert projection ready: %w", err)
	}

	// 2. Read the version's activation_policy_snapshot to resolve
	//    required_projections, then check whether ANY required projection is
	//    still non-ready (pending/building/failed). If all required are ready,
	//    flip build_status='ready'. We do this in the SAME tx so the projection
	//    row + the build_status flip commit together (§6.4 atomic).
	var snapshotJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT activation_policy_snapshot FROM knowledge_asset_versions WHERE id=$1`,
		assetVersionID).Scan(&snapshotJSON)
	if err != nil {
		return fmt.Errorf("asset: read activation snapshot: %w", err)
	}
	required := requiredProjectionsFromSnapshot(snapshotJSON)
	// Dedupe: a malformed snapshot listing the same kind twice would inflate
	// len(required) past the achievable count(DISTINCT …) and wedge the build.
	required = dedupeKinds(required)

	// §7 red-line gate: a version is build-complete ONLY when EVERY required
	// projection has a 'ready' row. The earlier form counted non-ready rows
	// (status <> 'ready') and treated count=0 as "all ready" — but a required
	// projection that has NO row at all also yields count=0, so a version
	// missing, say, its 'vector' projection would flip to ready and then
	// activate with no vector index (P0 D4-1). The fix is an assertion: count
	// the DISTINCT required kinds that have a ready row and require it to equal
	// the number of required kinds. Missing rows (no ready row for that kind)
	// therefore block ready, as do pending/building/failed rows.
	if len(required) == 0 {
		// No required projections configured — nothing gates the build. This
		// path is not expected (the helper defaults to ["fts","vector"]) but is
		// guarded so a malformed empty snapshot can't wedge the build forever.
		_, err = tx.Exec(ctx,
			`UPDATE knowledge_asset_versions SET build_status='ready' WHERE id=$1`,
			assetVersionID)
		if err != nil {
			return fmt.Errorf("asset: flip build_status ready: %w", err)
		}
		return nil
	}
	var readyKinds int
	err = tx.QueryRow(ctx, `
		SELECT count(DISTINCT projection_kind) FROM asset_projections
		WHERE asset_version_id = $1
		  AND projection_kind = ANY($2)
		  AND status = 'ready'`,
		assetVersionID, required).Scan(&readyKinds)
	if err != nil {
		return fmt.Errorf("asset: count ready required projections: %w", err)
	}
	if readyKinds < len(required) {
		// At least one required projection has no ready row (missing entirely,
		// or still pending/building/failed). Leave build_status alone — the
		// activation CAS will be rejected by the build_status='ready' gate
		// until all required kinds have a ready row (§7 部分投影就绪不得覆盖).
		return nil
	}
	// All required projections ready → flip build_status='ready'. Idempotent
	// (a re-mark when already ready is a no-op).
	_, err = tx.Exec(ctx,
		`UPDATE knowledge_asset_versions SET build_status='ready' WHERE id=$1`,
		assetVersionID)
	if err != nil {
		return fmt.Errorf("asset: flip build_status ready: %w", err)
	}
	return nil
}

// Activate performs the §7 CAS activation of current_version_id. The CAS gate
// enforces the four §7 invariants atomically; on failure current_version_id is
// UNTOUCHED and a sentinel error is returned so the worker can classify
// retry vs dead.
func (r *AssetRegistry) Activate(ctx context.Context, tx pgx.Tx, assetID, assetVersionID domain.UUID, fence int64, expectedCurrent *domain.UUID) error {
	if tx == nil {
		return errors.New("asset: Activate requires a transaction")
	}
	if assetID == uuid.Nil || assetVersionID == uuid.Nil {
		return domain.ErrAssetVersionNotFound
	}

	// 1. Read the version's build_status + governance_status + the activation
	//    policy snapshot. The gate is enforced BEFORE the CAS, inside the same
	//    tx so it sees a MarkProjectionReady flip (if it ran in this tx) and is
	//    consistent with the CAS UPDATE below.
	var buildStatus, govStatus string
	var snapshotJSON []byte
	err := tx.QueryRow(ctx,
		`SELECT build_status, governance_status, activation_policy_snapshot
		 FROM knowledge_asset_versions WHERE id=$1`,
		assetVersionID).Scan(&buildStatus, &govStatus, &snapshotJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrAssetVersionNotFound
		}
		return fmt.Errorf("asset: read version status: %w", err)
	}
	if buildStatus != domain.VersionBuildReady {
		return domain.ErrProjectionsNotReady
	}
	if govStatus != domain.VersionGovPublished {
		return domain.ErrNotPublished
	}
	// §7 defense-in-depth: do NOT trust build_status alone. Re-assert that
	// EVERY required projection has a 'ready' row at activation time. The
	// MarkProjectionReady gate is supposed to have enforced this before
	// flipping build_status, but a future reconcile-repair or a manual SQL
	// edit could set build_status='ready' without all projections actually
	// ready — the CAS must still refuse (§7 失败不覆盖 / 部分就绪不得覆盖).
	// Same assertion shape as MarkProjectionReady: count DISTINCT ready
	// required kinds and require == len(required). A missing required
	// projection (no row) blocks activation here too, not just at build time.
	required := dedupeKinds(requiredProjectionsFromSnapshot(snapshotJSON))
	if len(required) > 0 {
		var readyKinds int
		err := tx.QueryRow(ctx, `
			SELECT count(DISTINCT projection_kind) FROM asset_projections
			WHERE asset_version_id = $1
			  AND projection_kind = ANY($2)
			  AND status = 'ready'`,
			assetVersionID, required).Scan(&readyKinds)
		if err != nil {
			return fmt.Errorf("asset: count ready required projections (activate): %w", err)
		}
		if readyKinds < len(required) {
			return domain.ErrProjectionsNotReady
		}
	}

	// 2. The CAS UPDATE. The WHERE clause encodes the §7 invariants:
	//    - latest_requested_version_no = fence  (monotonic barrier; an old
	//      version completing late must NOT overwrite a newer pointer)
	//    - current_version_id IS NOT DISTINCT FROM expected  (expected_current;
	//      detects a concurrent activation)
	//    RowsAffected==0 means the CAS rejected — classify stale vs mismatch.
	tag, err := tx.Exec(ctx, `
		UPDATE knowledge_assets
		SET current_version_id = $1, updated_at = now()
		WHERE id = $2
		  AND latest_requested_version_no = $3
		  AND current_version_id IS NOT DISTINCT FROM $4`,
		assetVersionID, assetID, fence, nilIfZero(expectedCurrent))
	if err != nil {
		return fmt.Errorf("asset: CAS activate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// CAS rejected. Distinguish the barrier (stale fence) from
		// expected_current mismatch by re-reading the row.
		var actualLatest int64
		var actualCurrent *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT latest_requested_version_no, current_version_id FROM knowledge_assets WHERE id=$1`,
			assetID).Scan(&actualLatest, &actualCurrent)
		if err != nil {
			return fmt.Errorf("asset: CAS rejected, re-read failed: %w", err)
		}
		if actualLatest != fence {
			return domain.ErrCASVersionStale
		}
		return domain.ErrCASExpectedMismatch
	}
	return nil
}

// ReconcileScan runs the §3.3 consistency scan for one workspace. It opens its
// own short transactions (it does NOT take a caller tx) so each repair commits
// independently — a failure on one asset does not roll back repairs to others.
// Requires a pool wired via WithPool; returns an error otherwise.
func (r *AssetRegistry) ReconcileScan(ctx context.Context, workspaceID domain.UUID) (domain.ReconcileReport, error) {
	if r.pool == nil {
		return domain.ReconcileReport{}, errors.New("asset: ReconcileScan requires a pool (WithPool)")
	}
	if workspaceID == uuid.Nil {
		return domain.ReconcileReport{}, errors.New("asset: workspace_id required")
	}
	rep := domain.ReconcileReport{WorkspaceID: workspaceID}

	// 1. Repair current_version_id drift: assets whose current_version_id is
	//    unset or points at a non-ready/non-published version, but which have a
	//    newer ready+published version. The CAS here is the same barrier gate —
	//    we only advance to the latest ready+published version, never rewind.
	assets, err := r.pool.Query(ctx, `
		SELECT a.id, a.latest_requested_version_no, a.current_version_id
		FROM knowledge_assets a
		WHERE a.workspace_id = $1`, workspaceID)
	if err != nil {
		return rep, fmt.Errorf("asset: reconcile list assets: %w", err)
	}
	type assetRow struct {
		id     uuid.UUID
		latest int64
		cur    *uuid.UUID
	}
	var rows []assetRow
	for assets.Next() {
		var ar assetRow
		if err := assets.Scan(&ar.id, &ar.latest, &ar.cur); err != nil {
			assets.Close()
			return rep, fmt.Errorf("asset: reconcile scan assets: %w", err)
		}
		rows = append(rows, ar)
	}
	assets.Close()

	for _, ar := range rows {
		needsFix := false
		if ar.cur == nil {
			needsFix = true
		} else {
			// Is the current version actually usable (ready+published)?
			var bs, gs string
			err := r.pool.QueryRow(ctx,
				`SELECT build_status, governance_status FROM knowledge_asset_versions WHERE id=$1`,
				*ar.cur).Scan(&bs, &gs)
			if err != nil || bs != domain.VersionBuildReady || gs != domain.VersionGovPublished {
				needsFix = true
			}
		}
		if !needsFix {
			continue
		}
		// Find the latest ready+published version for this asset.
		var fixVer uuid.UUID
		err := r.pool.QueryRow(ctx, `
			SELECT id FROM knowledge_asset_versions
			WHERE asset_id = $1 AND build_status='ready' AND governance_status='published'
			ORDER BY version_no DESC LIMIT 1`, ar.id).Scan(&fixVer)
		if err != nil {
			continue // no usable version — needs human; counted below
		}
		// CAS-repair under the barrier.
		tag, err := r.pool.Exec(ctx, `
			UPDATE knowledge_assets SET current_version_id=$1, updated_at=now()
			WHERE id=$2 AND latest_requested_version_no=$3
			  AND current_version_id IS DISTINCT FROM $1`,
			fixVer, ar.id, ar.latest)
		if err == nil && tag.RowsAffected() > 0 {
			rep.VersionCASFixed++
		}
	}

	// 2. Mark superseded-version projections stale (async cleanup). A version
	//    whose version_no < the asset's current version_no is superseded; its
	//    ready projections can be reaped by the async cleaner. We mark them
	//    'stale' (not delete — provenance is retained).
	tag, err := r.pool.Exec(ctx, `
		UPDATE asset_projections p SET status='stale'
		WHERE p.status='ready'
		  AND EXISTS (SELECT 1 FROM knowledge_asset_versions v
		              JOIN knowledge_assets a ON a.id=v.asset_id
		              WHERE v.id=p.asset_version_id
		                AND a.current_version_id IS NOT NULL
		                AND a.current_version_id <> v.id)`)
	if err == nil {
		rep.ProjectionsStaled += int(tag.RowsAffected())
	}

	// 3. Tally ready-but-not-published versions (need human review) for the
	//    report — the reconcile loop does not auto-publish (governance is a
	//    human action, §7).
	err = r.pool.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_asset_versions v
		JOIN knowledge_assets a ON a.id=v.asset_id
		WHERE a.workspace_id=$1
		  AND v.build_status='ready' AND v.governance_status<>'published'`,
		workspaceID).Scan(&rep.NeedsHuman)
	if err == nil {
		// best-effort; a failure here does not fail the scan
	}

	return rep, nil
}

// requiredProjectionsFromSnapshot resolves the required_projections list from
// a version's activation_policy_snapshot JSONB. Falls back to the document
// default ["fts","vector"] (§7) when the snapshot is missing or the field is
// absent. Returns a []string suitable for `= ANY($n)`.
func requiredProjectionsFromSnapshot(snapshotJSON []byte) []string {
	if len(snapshotJSON) == 0 {
		return []string{"fts", "vector"}
	}
	var snap map[string]any
	if err := json.Unmarshal(snapshotJSON, &snap); err != nil {
		return []string{"fts", "vector"}
	}
	raw, ok := snap["required_projections"]
	if !ok {
		return []string{"fts", "vector"}
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return []string{"fts", "vector"}
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{"fts", "vector"}
	}
	return out
}

// dedupeKinds returns kinds with duplicates removed, preserving order. Guards
// the build_status gate against a malformed activation_policy_snapshot that
// lists the same projection_kind twice (which would inflate len(required)
// past the achievable count(DISTINCT projection_kind) and wedge the build).
func dedupeKinds(kinds []string) []string {
	seen := make(map[string]struct{}, len(kinds))
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
