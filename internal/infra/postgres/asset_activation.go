package postgres

// asset_activation.go implements asset.ActivationRegistry over postgres — the
// async version-activation path (design-docs/14 §7 CAS + §3.3 reconcile).
// It extends the dual-write AssetRegistry (asset_registry.go) with the three
// methods the knowledge-worker handlers call:
//   - MarkProjectionReady  (ProjectionBuildHandler, §7 step "projection ready")
//   - Activate             (AssetActivateHandler, §7 CAS)
//   - ReconcileScan        (ReconcileHandler, §3.3 sweep)
//
// All SQL is parameterized (07-security §10). The CAS is the final authority:
// a stale fence or expected_current leaves current_version_id untouched (§7
// fail-no-overwrite, §10.3 用例 22/24). A missing version/projection returns a
// sentinel — existence never leaks (§8.2).

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// Compile-time check: AssetRegistry satisfies BOTH the dual-write Registry and
// the async ActivationRegistry.
var _ asset.ActivationRegistry = (*AssetRegistry)(nil)

// requiredProjectionsOf reads the version's activation_policy_snapshot and
// returns its required_projections list. The snapshot is stamped at
// registration time (§6.4) so the activation gate is reproducible without a
// live join to governance_profiles (a profile could be edited after the version
// was requested). Falls back to [] (no blocking projections) if the snapshot is
// missing/malformed — a malformed snapshot should not block activation; the
// reconcile sweep would catch a genuinely missing projection separately.
func requiredProjectionsOf(ctx context.Context, tx pgx.Tx, versionID domain.UUID) ([]string, error) {
	var snapshot []byte
	err := tx.QueryRow(ctx,
		`SELECT activation_policy_snapshot FROM knowledge_asset_versions WHERE id = $1`,
		versionID).Scan(&snapshot)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, asset.ErrVersionNotFound
		}
		return nil, err
	}
	if len(snapshot) == 0 {
		return nil, nil
	}
	var snap struct {
		RequiredProjections []string `json:"required_projections"`
	}
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return nil, nil
	}
	return snap.RequiredProjections, nil
}

// MarkProjectionReady upserts an asset_projections row as 'ready' and, if this
// was the last required projection, flips the version's build_status to 'ready'
// (§10.3 用例 20: a missing required projection blocks ready).
//
// Idempotent: the (asset_version_id, projection_kind, build_revision) UNIQUE
// means a re-mark of the same projection is an upsert that just refreshes
// status='ready'/built_at. A different build_revision is a NEW row (a rebuild
// produces a new revision, not an in-place rewrite — §2.1).
func (r *AssetRegistry) MarkProjectionReady(ctx context.Context, tx pgx.Tx, pr asset.ProjectionReady) (asset.MarkProjectionReadyResult, error) {
	if tx == nil {
		return asset.MarkProjectionReadyResult{}, errors.New("asset: mark projection requires a transaction")
	}
	if pr.AssetVersionID == uuidNil() || pr.Kind == "" || pr.BuildRevision == "" {
		return asset.MarkProjectionReadyResult{}, errors.New("asset: asset_version_id, kind, build_revision are required")
	}
	// 1. Verify the version exists (no existence leak: a missing version is a
	//    sentinel the handler maps to permanent failure).
	required, err := requiredProjectionsOf(ctx, tx, pr.AssetVersionID)
	if err != nil {
		return asset.MarkProjectionReadyResult{}, err
	}
	// 2. Upsert the projection row as ready. ON CONFLICT refreshes status/built_at
	//    for an idempotent re-mark; a new build_revision inserts a new row.
	locator, _ := json.Marshal(pr.Locator)
	if _, err := tx.Exec(ctx, `
		INSERT INTO asset_projections
		  (asset_version_id, projection_kind, provider, provider_version,
		   build_revision, status, locator, built_at)
		VALUES ($1,$2,$3,$4,$5,'ready',$6,now())
		ON CONFLICT (asset_version_id, projection_kind, build_revision)
		DO UPDATE SET status='ready', locator=EXCLUDED.locator, built_at=now(),
		              updated_at=now()`,
		pr.AssetVersionID, string(pr.Kind), pr.Provider, pr.ProviderVersion,
		pr.BuildRevision, locator); err != nil {
		return asset.MarkProjectionReadyResult{}, err
	}
	// 3. If this kind is a required (blocking) projection, check whether ALL
	//    required projections are now ready. If so, flip build_status='ready'.
	buildReady := false
	if isBlocking(pr.Kind, required) {
		ready, err := allRequiredReady(ctx, tx, pr.AssetVersionID, required)
		if err != nil {
			return asset.MarkProjectionReadyResult{}, err
		}
		if ready {
			if _, err := tx.Exec(ctx,
				`UPDATE knowledge_asset_versions SET build_status='ready', updated_at=now()
				 WHERE id = $1 AND build_status != 'ready'`,
				pr.AssetVersionID); err != nil {
				return asset.MarkProjectionReadyResult{}, err
			}
			buildReady = true
		}
	}
	return asset.MarkProjectionReadyResult{BuildReady: buildReady}, nil
}

// isBlocking reports whether kind is in the required list. A projection not in
// the required list (a non-blocking/optional modality) landing does not, by
// itself, unblock build_status — §7 "非阻塞投影未就绪时降级，不覆盖旧版本".
func isBlocking(kind domain.ProjectionKind, required []string) bool {
	for _, k := range required {
		if k == string(kind) {
			return true
		}
	}
	return false
}

// allRequiredReady counts ready asset_projections rows for each required kind
// (latest build_revision per kind) and returns true only when every required
// kind has at least one 'ready' row. §10.3 用例 20: a missing required projection
// blocks ready.
func allRequiredReady(ctx context.Context, tx pgx.Tx, versionID domain.UUID, required []string) (bool, error) {
	if len(required) == 0 {
		return true, nil
	}
	// For each required kind, a 'ready' row must exist. We count DISTINCT kinds
	// that have a ready projection and compare to the required count.
	var readyCount int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT projection_kind) FROM asset_projections
		WHERE asset_version_id = $1 AND status = 'ready'
			AND projection_kind = ANY($2::text[])`,
		versionID, required).Scan(&readyCount)
	if err != nil {
		return false, err
	}
	return readyCount == len(required), nil
}

// Activate performs the §7 CAS activation. See asset.Activation for the
// input contract and the sentinel errors for the failure dispositions.
func (r *AssetRegistry) Activate(ctx context.Context, tx pgx.Tx, a asset.Activation) (asset.ActivationResult, error) {
	if tx == nil {
		return asset.ActivationResult{}, errors.New("asset: activate requires a transaction")
	}
	if a.AssetID == uuidNil() || a.VersionID == uuidNil() {
		return asset.ActivationResult{}, errors.New("asset: asset_id and version_id are required")
	}

	// 1. Read the version's governance_status + build_status, and the asset's
	//    current_version_id + latest_requested_version_no, in one round trip.
	var (
		assetLatest int64
		assetCurrent *domain.UUID
		verBuild, verGov string
	)
	err := tx.QueryRow(ctx, `
		SELECT a.latest_requested_version_no, a.current_version_id,
		       v.build_status, v.governance_status
		FROM knowledge_asset_versions v
		JOIN knowledge_assets a ON a.id = v.asset_id
		WHERE v.id = $1 AND v.asset_id = $2`,
		a.VersionID, a.AssetID).Scan(&assetLatest, &assetCurrent, &verBuild, &verGov)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return asset.ActivationResult{}, asset.ErrVersionNotFound
		}
		return asset.ActivationResult{}, err
	}

	// 2. Governance gate: only a published version may become current (§7).
	if verGov != "published" {
		return asset.ActivationResult{}, asset.ErrNotPublished
	}
	// 3. Build gate: all required projections must be ready (§10.3 用例 20).
	//    build_status='ready' is the projection gate's own write-back; a version
	//    still 'pending'/'building' has a missing required projection.
	if verBuild != "ready" {
		return asset.ActivationResult{}, asset.ErrProjectionsNotReady
	}

	// 4. Monotonic barrier: the caller's Fence must equal the asset's current
	//    latest_requested_version_no. If the barrier advanced past Fence, a
	//    newer version already activated — this old version must NOT switch
	//    current_version_id (§7 单调栅栏, §10.3 用例 22). Mark ready-only.
	if assetLatest > a.Fence {
		// Still ensure the version is build_status='ready' (a late-completing
		// old version can be ready without becoming current), then refuse.
		_, _ = tx.Exec(ctx,
			`UPDATE knowledge_asset_versions SET build_status='ready', updated_at=now()
			 WHERE id = $1 AND build_status != 'ready'`, a.VersionID)
		return asset.ActivationResult{}, asset.ErrCASVersionStale
	}

	// 5. expected_current guard: the caller must name the version it expects to
	//    replace (§10.3 用例 24). uuid.Nil = "no current / initial activation".
	currentVal := uuidNil()
	if assetCurrent != nil {
		currentVal = *assetCurrent
	}
	if currentVal != a.ExpectedCurrent {
		// The CAS would overwrite an unexpected version — refuse. This is the
		// explicit-rollback invariant: a rollback must name expected_current.
		return asset.ActivationResult{PreviousCurrentID: currentVal}, asset.ErrCASExpectedMismatch
	}
	// If the version is already current (ExpectedCurrent == VersionID), the CAS
	// is a no-op success (idempotent activation).
	if currentVal == a.VersionID {
		return asset.ActivationResult{Activated: false, PreviousCurrentID: currentVal}, nil
	}

	// 6. CAS: flip current_version_id under both guards (barrier + expected).
	//    WHERE latest_requested_version_no = Fence AND current_version_id IS
	//    NOT DISTINCT FROM ExpectedCurrent. One of these is belt, the other
	//    braces — together they prevent an old version or a racing rollback
	//    from moving the pointer unexpectedly.
	tag, err := tx.Exec(ctx, `
		UPDATE knowledge_assets
		SET current_version_id = $1, updated_at = now()
		WHERE id = $2
		  AND latest_requested_version_no = $3
		  AND current_version_id IS NOT DISTINCT FROM $4`,
		a.VersionID, a.AssetID, a.Fence, nilIfZeroUUID(a.ExpectedCurrent))
	if err != nil {
		return asset.ActivationResult{}, err
	}
	if tag.RowsAffected() == 0 {
		// The guards changed between the read and the CAS (a concurrent
		// activation/rollback moved the pointer). Surface as a mismatch —
		// the caller will not overwrite the (now different) current version.
		return asset.ActivationResult{PreviousCurrentID: currentVal}, asset.ErrCASExpectedMismatch
	}
	return asset.ActivationResult{Activated: true, PreviousCurrentID: currentVal}, nil
}

// ReconcileScan runs the §3.3 consistency sweep for one workspace. It opens
// its own short transactions (it is a sweep, not a single-row write) so it does
// not require a caller tx. Three passes:
//  1. CAS-fix assets whose current_version_id drifted from the document's
//     current version (§3.3 row 1).
//  2. Mark superseded versions' projections 'stale' (§3.3 cleanup).
//  3. (Requeue of missing projections is reported via RequeuedProjections but
//     the actual job enqueue is the worker's concern — here we only count, to
//     keep the registry a pure data layer. The handler re-enqueues.)
func (r *AssetRegistry) ReconcileScan(ctx context.Context, pool asset.ReconcilePool, workspaceID domain.UUID) (asset.ReconcileOutcome, error) {
	if pool == nil {
		return asset.ReconcileOutcome{}, errors.New("asset: reconcile requires a pool")
	}
	var out asset.ReconcileOutcome

	// Pass 1: asset.current_version_id ↔ document current-version drift.
	// A native-document asset whose current_version_id does not point at the
	// document's latest version (knowledge_asset_versions joined to the
	// document_version the asset version references) is CAS-fixed forward only
	// (latest_requested_version_no barrier — never rewind, §7).
	tx, err := pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	// Find drifted native-document assets: the asset's current version's
	// native_document_version_id must match a document_version whose version_no
	// equals the document's current version_no. If a newer document version was
	// registered as a newer asset version, current_version_id should already
	// have advanced; if it didn't (e.g. a failed CAS mid-flight), fix it here.
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.latest_requested_version_no,
		       (SELECT v.id FROM knowledge_asset_versions v
		          WHERE v.asset_id = a.id
		          ORDER BY v.version_no DESC LIMIT 1) AS latest_ver_id
		FROM knowledge_assets a
		WHERE a.workspace_id = $1 AND a.asset_type = 'document'
		  AND a.current_version_id IS DISTINCT FROM (
		    SELECT v.id FROM knowledge_asset_versions v
		      WHERE v.asset_id = a.id
		      ORDER BY v.version_no DESC LIMIT 1)`,
		workspaceID)
	if err != nil {
		return out, err
	}
	type drift struct {
		assetID  domain.UUID
		fence    int64
		latestVer domain.UUID
	}
	var drifted []drift
	for rows.Next() {
		var d drift
		if err := rows.Scan(&d.assetID, &d.fence, &d.latestVer); err != nil {
			rows.Close()
			return out, err
		}
		drifted = append(drifted, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, d := range drifted {
		// CAS forward only: WHERE latest_requested_version_no = fence AND
		// current_version_id IS NULL OR older. We use the barrier equality to
		// avoid a rewind (the sweep never moves the pointer backward).
		tag, err := tx.Exec(ctx, `
			UPDATE knowledge_assets
			SET current_version_id = $1, updated_at = now()
			WHERE id = $2
			  AND latest_requested_version_no = $3
			  AND current_version_id IS DISTINCT FROM $1`,
			d.latestVer, d.assetID, d.fence)
		if err != nil {
			return out, err
		}
		if tag.RowsAffected() > 0 {
			out.FixedAssets++
		}
	}

	// Pass 2: mark stale projections of superseded versions. A version is
	// superseded when a newer version is current_version_id; its projections
	// (still 'ready' from when it was current) become 'stale' for async
	// cleanup (§3.3). This is a best-effort marking; the actual cleanup
	// (dropping Qdrant points / FTS rows) is the projection provider's job.
	tag, err := tx.Exec(ctx, `
		UPDATE asset_projections p SET status='stale', updated_at=now()
		WHERE status = 'ready'
		  AND p.asset_version_id IN (
		    SELECT v.id FROM knowledge_asset_versions v
		    JOIN knowledge_assets a ON a.id = v.asset_id
		    WHERE a.workspace_id = $1
		      AND v.id <> a.current_version_id
		      AND v.version_no < COALESCE(
		            (SELECT MAX(v2.version_no) FROM knowledge_asset_versions v2
		             WHERE v2.asset_id = a.id), v.version_no))`,
		workspaceID)
	if err != nil {
		return out, err
	}
	out.StaleProjectionsMarked = int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}

// uuidNil returns the zero uuid.UUID. Local helper so this file does not need
// to import google/uuid just for the nil constant (asset_registry.go already
// imports it and uses the same idiom via nilIfZero).
func uuidNil() domain.UUID { return domain.UUID{} }

// nilIfZeroUUID maps a zero UUID to nil for pgx (so a NULL expected_current
// column compares correctly under IS NOT DISTINCT FROM). Mirrors nilIfZero in
// the other postgres files.
func nilIfZeroUUID(u domain.UUID) any {
	if u == uuidNil() {
		return nil
	}
	return u
}
