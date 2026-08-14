// Package worker — job handlers (design-docs/14 §5.2 dispatch table). Each
// Handler in this file owns one job_type's business logic. The Runner drives
// acquire→run→mark; handlers only implement Run.
//
// Handlers are intentionally thin: they compose existing ports (AssetRegistry,
// JobStore, RAG pipeline) rather than re-implementing them. A handler that
// fails transiently returns (RetryTransient, err); the Runner's MarkFailed
// handles the attempt/max_attempt math and dead-letters at the cap.
//
// The §6 CAS + §3.3 reconcile deliverable is wired here:
//   - ProjectionBuildHandler → AssetRegistry.MarkProjectionReady (§7 step
//     "projection ready"; flips build_status when the last required projection
//     lands, §10.3 用例 20).
//   - AssetActivateHandler   → AssetRegistry.Activate (§7 CAS; §10.3 用例 22
//     old version CAS-stale / 用例 24 expected_current mismatch).
//   - ReconcileHandler       → AssetRegistry.ReconcileScan (§3.3 sweep).
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// ErrNotWired marks a job_type whose backing port has not landed yet. It is
// permanent so the Runner dead-letters the job instead of retrying forever.
// (Kept for the Source/Backfill handlers still pending the Connector/migration
// deliverables; the CAS/reconcile path no longer returns it.)
var ErrNotWired = errors.New("worker: handler not wired for this job_type")

// --- §5.2 row 1: source_sync -------------------------------------------------
//
// "调 Connector 摄取" → asset_projections(fts,vector) pending jobs.
// Phase 1 skeleton: the Connector framework (§4.1) is a separate deliverable;
// this handler validates the Source exists, records the fetch manifest, and
// fans out the projection_build jobs that the ProjectionBuildHandler will
// execute. The actual Connector adapter (file/url_api/git) is stubbed to be
// filled when the Source API + Connector land.

// SourceSyncHandler drives a Source ingest run. It owns the source_events →
// projection_build fan-out.
type SourceSyncHandler struct {
	Pool *pgxpool.Pool
	Jobs JobStore
}

func (h *SourceSyncHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	_ = ctx
	if j.SourceID == nil || *j.SourceID == uuid.Nil {
		return domain.RetryPermanent, fmtErr(JobSourceSync, fmt.Errorf("missing source_id"))
	}
	// TODO(Phase1-Source): once the Connector port lands (§4.1), call it here
	// to fetch manifest → upsert SourceTarget/Asset → create version → enqueue
	// projection_build jobs (one per required projection). For now the handler
	// is a validated no-op so the dispatch table is wired and observable.
	return domain.RetryTransient, nil
}

// --- §5.2 row 2: projection_build -------------------------------------------
//
// "调 rag-worker Document pipeline 或 Provider" → asset_projections.status
// ready/failed. This handler is the rag-worker bridge (§7): when a projection
// build completes, it calls AssetRegistry.MarkProjectionReady so the gate can
// flip build_status='ready' and the activation CAS can proceed.

// ProjectionBuildHandler marks a single projection ready once its build is
// done. The actual build (FTS indexing, Qdrant upsert) is rag-worker's /
// the Provider's job; this handler is the write-back to asset_projections.
// §10.3 用例 20: a missing required projection blocks build_status='ready' —
// MarkProjectionReady only flips it when the LAST required projection lands.
type ProjectionBuildHandler struct {
	Pool   *pgxpool.Pool
	Jobs   JobStore
	Assets asset.ActivationRegistry
}

func (h *ProjectionBuildHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if j.AssetVersionID == nil {
		return domain.RetryPermanent, fmtErr(JobProjectionBuild, fmt.Errorf("missing asset_version_id"))
	}
	kind := domain.ProjectionKind(j.TargetKey)
	if kind == "" {
		return domain.RetryPermanent, fmtErr(JobProjectionBuild, fmt.Errorf("missing projection_kind target_key"))
	}
	// Run the projection write-back inside a short tx. A transient DB failure
	// retries; a missing version (ErrVersionNotFound) is permanent (existence
	// leak guard — the version will not appear by retrying).
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobProjectionBuild, err)
	}
	defer tx.Rollback(ctx)
	_, err = h.Assets.MarkProjectionReady(ctx, tx, asset.ProjectionReady{
		AssetVersionID:  *j.AssetVersionID,
		Kind:            kind,
		BuildRevision:   j.BuildRevision,
		Provider:        stringFromJobProgress(j, "provider"),
		ProviderVersion: stringFromJobProgress(j, "provider_version"),
		Locator:         mapFromJobProgress(j, "locator"),
	})
	if err != nil {
		if errors.Is(err, asset.ErrVersionNotFound) {
			_ = tx.Rollback(ctx)
			return domain.RetryPermanent, fmtErr(JobProjectionBuild, err)
		}
		_ = tx.Rollback(ctx)
		return domain.RetryTransient, fmtErr(JobProjectionBuild, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetryTransient, fmtErr(JobProjectionBuild, err)
	}
	return domain.RetryTransient, nil
}

// --- §5.2 row 3: asset_activate ---------------------------------------------
//
// "CAS 激活 current_version_id（§6）". This handler performs the CAS under the
// job's fence, with expected_current read from the asset row. Failed/CAS-stale
// are surfaced as job failure but the asset is untouched (the CAS is the
// final authority, §7 red-line).

// AssetActivateHandler runs the §7 CAS activation. It is the async counterpart
// to mora-api's activation write: mora-api requests the version (writes the
// fence + outbox event); the worker performs the CAS once projections are ready.
// §10.3 用例 22: a late-completing old version fails the CAS (stale) and is
// marked ready-only — current_version_id is NOT rewound. §10.3 用例 24: a
// rollback without the correct expected_current is rejected.
type AssetActivateHandler struct {
	Pool   *pgxpool.Pool
	Assets asset.ActivationRegistry
}

func (h *AssetActivateHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if j.AssetID == nil || j.AssetVersionID == nil {
		return domain.RetryPermanent, fmtErr(JobAssetActivate, fmt.Errorf("missing asset_id/asset_version_id"))
	}
	fence := int64FromJobProgress(j, "latest_requested_version_no")
	expectedCurrentStr := stringFromJobProgress(j, "expected_current")
	expectedCurrent := uuid.Nil
	if expectedCurrentStr != "" {
		if parsed, err := uuid.Parse(expectedCurrentStr); err == nil {
			expectedCurrent = parsed
		}
	}
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobAssetActivate, err)
	}
	defer tx.Rollback(ctx)
	_, err = h.Assets.Activate(ctx, tx, asset.Activation{
		AssetID:         *j.AssetID,
		VersionID:       *j.AssetVersionID,
		Fence:           fence,
		ExpectedCurrent: expectedCurrent,
	})
	if err != nil {
		// CAS-already-decided + governance errors are permanent — the asset
		// state won't change by retrying. ErrProjectionsNotReady is also
		// permanent from Activate's standpoint (the build path should have
		// gated on projections); the build handler re-enqueues. Everything
		// else (transient DB) retries.
		if errors.Is(err, asset.ErrCASVersionStale) ||
			errors.Is(err, asset.ErrCASExpectedMismatch) ||
			errors.Is(err, asset.ErrNotPublished) ||
			errors.Is(err, asset.ErrVersionNotFound) ||
			errors.Is(err, asset.ErrProjectionsNotReady) {
			_ = tx.Rollback(ctx)
			return domain.RetryPermanent, fmtErr(JobAssetActivate, err)
		}
		_ = tx.Rollback(ctx)
		return domain.RetryTransient, fmtErr(JobAssetActivate, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetryTransient, fmtErr(JobAssetActivate, err)
	}
	return domain.RetryTransient, nil
}

// --- §5.2 row 4: reconcile_scan ----------------------------------------------
//
// "对账扫描（§3.3）". Runs AssetRegistry.ReconcileScan for one workspace.
// §3.3: CAS-fix drifted current_version_id pointers and mark superseded
// versions' projections 'stale'. The sweep runs its own short transactions.

type ReconcileHandler struct {
	Pool   *pgxpool.Pool
	Assets asset.ActivationRegistry
}

func (h *ReconcileHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	wsID, err := uuid.Parse(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobReconcileScan, fmt.Errorf("missing workspace_id in target_key: %w", err))
	}
	// ReconcileScan opens its own tx(s) over the pool; the handler does not
	// need to manage a tx. A transient DB failure retries.
	if _, err := h.Assets.ReconcileScan(ctx, h.Pool, domain.UUID(wsID)); err != nil {
		return domain.RetryTransient, fmtErr(JobReconcileScan, err)
	}
	return domain.RetryTransient, nil
}

// --- §5.2 row 5: legacy_backfill ---------------------------------------------
//
// "存量文档登记（§3.2）". Phase 1 skeleton: registers one batch of existing
// documents as legacy_migration assets, emitting asset.version.requested for
// each so the projection pipeline backfills them. The full batch query is
// deferred to the migration deliverable; the handler shape is here so the
// dispatch table is complete.

type LegacyBackfillHandler struct {
	Pool *pgxpool.Pool
}

func (h *LegacyBackfillHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	_ = ctx
	// TODO(Phase1-Backfill): query documents lacking a knowledge_asset row,
	// insert legacy_migration assets, emit asset.version.requested via outbox.
	// Skeleton returns success so the dispatch table is observable.
	return domain.RetryTransient, nil
}

// --- helpers ---

// int64FromJobProgress reads an int64 field from j.Progress, tolerating the
// JSONB-derived map[string]any typing (float64 / json.Number / int).
func int64FromJobProgress(j domain.Job, key string) int64 {
	if j.Progress == nil {
		return 0
	}
	v, ok := j.Progress[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func stringFromJobProgress(j domain.Job, key string) string {
	if j.Progress == nil {
		return ""
	}
	if v, ok := j.Progress[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mapFromJobProgress reads a map[string]any field from j.Progress (e.g. a
// projection locator). Returns nil when absent.
func mapFromJobProgress(j domain.Job, key string) map[string]any {
	if j.Progress == nil {
		return nil
	}
	if v, ok := j.Progress[key]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return nil
}
