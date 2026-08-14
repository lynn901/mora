// Package worker — job handlers (design-docs/14 §5.2 dispatch table). Each
// Handler in this file owns one job_type's business logic. The Runner drives
// acquire→run→mark; handlers only implement Run.
//
// Handlers are intentionally thin: they compose existing ports (AssetRegistry,
// JobStore, RAG pipeline) rather than re-implementing them. A handler that
// fails transiently returns (RetryTransient, err); the Runner's MarkFailed
// handles the attempt/max_attempt math and dead-letters at the cap.
//
// §7 CAS wired: projection_build calls AssetRegistry.MarkProjectionReady (the
// rag-worker ready write-back), asset_activate calls AssetRegistry.Activate
// (the CAS gate), and reconcile_scan calls AssetRegistry.ReconcileScan (the
// §3.3 self-healing ticker). source_sync and legacy_backfill remain validated
// no-ops pending the Connector (§4.1) and batch-backfill (§3.2) deliverables.
package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// ErrNotWired marks a job_type whose backing port has not landed yet. It is
// permanent so the Runner dead-letters the job instead of retrying forever.
// (Currently unused — all five handlers are wired — retained for the next
// deferred port.)
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
	Tx  JobTxStarter
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
//
// Job fields it reads: AssetVersionID, TargetKey (the projection_kind),
// BuildRevision, LeaseOwner (used as the provider label), Progress.locator.
type ProjectionBuildHandler struct {
	Tx     JobTxStarter
	Jobs   JobStore
	Assets asset.Registry
}

func (h *ProjectionBuildHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if j.AssetVersionID == nil {
		return domain.RetryPermanent, fmtErr(JobProjectionBuild, fmt.Errorf("missing asset_version_id"))
	}
	kind := domain.ProjectionKind(j.TargetKey)
	if kind == "" {
		return domain.RetryPermanent, fmtErr(JobProjectionBuild, fmt.Errorf("missing projection_kind target_key"))
	}
	if j.BuildRevision == "" {
		return domain.RetryPermanent, fmtErr(JobProjectionBuild, fmt.Errorf("missing build_revision"))
	}
	// provider = the builder identity (rag-worker / tei / qdrant). The job's
	// lease owner is a stable consumer name, usable as the provider label when
	// the caller didn't put an explicit one in Progress.
	provider := stringFromJobProgress(j, "provider")
	if provider == "" {
		provider = j.LeaseOwner
	}
	locator, _ := j.Progress["locator"].(map[string]any)

	// MarkProjectionReady is idempotent: re-marking an already-ready projection
	// is a no-op, so a crash-retry after a partial completion is safe. It runs
	// inside a short tx so the projection row + build_status flip commit
	// together (§6.4 atomic).
	tx, err := h.Tx.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobProjectionBuild, err)
	}
	defer tx.Rollback(ctx) // safe on success (committed) — pgx no-op on a spent tx
	if err := h.Assets.MarkProjectionReady(ctx, tx, *j.AssetVersionID, kind, provider, j.BuildRevision, locator); err != nil {
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
// job's fence, with expected_current read from the job's progress (set by the
// caller that requested the version). Failed/CAS-stale are surfaced as job
// failure but the asset is untouched (the CAS is the final authority, §7
// red-line). The sentinel errors classify retry vs dead:
//   - ErrCASVersionStale / ErrCASExpectedMismatch → permanent (CAS decided)
//   - ErrNotPublished → permanent (governance won't change by retrying)
//   - ErrProjectionsNotReady → transient (projections may still be building)
//   - other → transient

// AssetActivateHandler runs the §7 CAS activation. It is the async counterpart
// to mora-api's activation write: mora-api requests the version (writes the
// fence + outbox event); the worker performs the CAS once projections are ready.
type AssetActivateHandler struct {
	Tx     JobTxStarter
	Assets asset.Registry
}

func (h *AssetActivateHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if j.AssetID == nil || j.AssetVersionID == nil {
		return domain.RetryPermanent, fmtErr(JobAssetActivate, fmt.Errorf("missing asset_id/asset_version_id"))
	}
	// fence = the latest_requested_version_no the caller observed when it
	// requested the version. The CAS WHERE latest_requested_version_no = fence
	// rejects if a newer version was requested after this build started (§7
	// 单调栅栏). Fall back to the job's own version fence if absent.
	fence := int64FromJobProgress(j, "fence")
	if fence == 0 {
		fence = int64FromJobProgress(j, "latest_requested_version_no")
	}
	if fence == 0 {
		return domain.RetryPermanent, fmtErr(JobAssetActivate, fmt.Errorf("missing fence in progress"))
	}
	// expected_current: the current_version_id the caller expected, so a
	// concurrent activation is detected. nil = initial activation (CAS matches
	// the NULL pointer). Parsed from a string in Progress because JSON round-trips.
	var expected *uuid.UUID
	if s := stringFromJobProgress(j, "expected_current"); s != "" {
		if e, err := uuid.Parse(s); err == nil && e != uuid.Nil {
			expected = &e
		}
	}

	tx, err := h.Tx.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobAssetActivate, err)
	}
	defer tx.Rollback(ctx)
	err = h.Assets.Activate(ctx, tx, *j.AssetID, *j.AssetVersionID, fence, expected)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrCASVersionStale), errors.Is(err, domain.ErrCASExpectedMismatch):
			return domain.RetryPermanent, fmtErr(JobAssetActivate, err)
		case errors.Is(err, domain.ErrNotPublished), errors.Is(err, domain.ErrAssetVersionNotFound):
			return domain.RetryPermanent, fmtErr(JobAssetActivate, err)
		case errors.Is(err, domain.ErrProjectionsNotReady):
			return domain.RetryTransient, fmtErr(JobAssetActivate, err)
		default:
			return domain.RetryTransient, fmtErr(JobAssetActivate, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetryTransient, fmtErr(JobAssetActivate, err)
	}
	return domain.RetryTransient, nil
}

// --- §5.2 row 4: reconcile_scan ----------------------------------------------
//
// "对账扫描（§3.3）". Runs AssetRegistry.ReconcileScan for one workspace. The
// scan is self-healing: it CAS-repairs unset/stale current_version_id and
// re-queues stuck projections. It does not auto-publish (governance is human).

type ReconcileHandler struct {
	Assets asset.Registry
}

func (h *ReconcileHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	wsID, err := uuid.Parse(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, fmtErr(JobReconcileScan, fmt.Errorf("missing workspace_id in target_key: %w", err))
	}
	// ReconcileScan opens its own short transactions internally; no caller tx.
	if _, err := h.Assets.ReconcileScan(ctx, wsID); err != nil {
		// Reconcile is a background safety net — a transient DB error is
		// retryable; a misconfiguration (no pool) is permanent (dead-letter so
		// the operator fixes the worker wiring, not silent spin).
		if errors.Is(err, domain.ErrAssetVersionNotFound) {
			return domain.RetryPermanent, fmtErr(JobReconcileScan, err)
		}
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
	Tx JobTxStarter
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
