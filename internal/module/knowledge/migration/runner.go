// Package migration implements the legacy-document online migration protocol
// (design-docs/14 §3.2 backfill, §3.3 reconciliation). It registers existing
// documents — already on disk before Phase 1 — as Document knowledge assets
// WITHOUT copying their content: the asset version only stores a
// native_document_version_id reference, and content stays read from
// documents.content / document_versions.content (§3.3 不复制正文).
//
// The same protocol serves three callers:
//   - backfill one-shot: register every existing document version (§3.2);
//   - reconciliation scan: repair asset↔document drift (§3.3);
//   - knowledge-worker ticker (Phase 1 §5.2): schedules reconcile_scan jobs.
//
// Backfill is idempotent and resumable: the dedupe_key=
// 'document_version:'||version.id UNIQUE guard means a re-run over already-
// migrated documents is a no-op, and a crash mid-batch leaves the committed
// batches durable and the rest pickable up by a re-run. Batches use
// FOR UPDATE SKIP LOCKED so concurrent backfill runs (or a run racing the
// DocWriteSink dual-write) never contend on the same row.
package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// BatchSize is the default documents-per-transaction batch (14 §3.2 batch=500).
const BatchSize = 500

// Options configures a Runner.
type Options struct {
	BatchSize int
	// MigrationServiceAccountID is the service account that approves backfill
	// review requests (§3.4). Required for backfill: the legacy_migration
	// governance decision is recorded as approved_by=service_account. Must be a
	// real service_accounts row (the §3.4 system approve is auditable).
	MigrationServiceAccountID domain.UUID
}

// Runner executes the online migration protocol against a database.
type Runner struct {
	pool     *pgxpool.Pool
	registry asset.Registry
	outbox   *outbox.Store
	opts     Options
	log      *slog.Logger
}

// NewRunner builds a migration Runner. registry and outbox are the same
// stateless adapters the DocWriteSink uses; the pool is the mora database.
func NewRunner(pool *pgxpool.Pool, registry asset.Registry, store *outbox.Store, opts Options, log *slog.Logger) *Runner {
	if opts.BatchSize <= 0 {
		opts.BatchSize = BatchSize
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{pool: pool, registry: registry, outbox: store, opts: opts, log: log}
}

// --- doc row scanned during backfill ---

type docVersionRow struct {
	DocID       uuid.UUID
	WorkspaceID uuid.UUID
	Title       string
	CreatedBy   uuid.UUID
	VersionNo   int64
	VersionID   uuid.UUID
}

// BackfillAll runs the §3.2 backfill across every workspace, registering every
// non-deleted document that has no asset yet. Returns the count of newly
// registered document versions and a slice of per-workspace errors (a failing
// workspace does not abort the others — the run is resumable).
//
// Pause/Resume: the method is naturally resumable — re-running it over an
// already-migrated workspace is a no-op (dedupe_key idempotency). A long run
// can be interrupted (context cancel) and restarted; committed batches persist.
func (r *Runner) BackfillAll(ctx context.Context) (int, error) {
	if r.opts.MigrationServiceAccountID == uuid.Nil {
		return 0, errors.New("migration: MigrationServiceAccountID is required (§3.4 system approve)")
	}
	workspaceIDs, err := r.listWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	r.log.Info("backfill: starting", "workspaces", len(workspaceIDs), "batch_size", r.opts.BatchSize)

	total := 0
	for _, wsID := range workspaceIDs {
		n, err := r.BackfillWorkspace(ctx, wsID)
		if err != nil {
			// A failed workspace is logged but does not abort the rest — the run
			// stays resumable (re-run picks up where it left off).
			r.log.Error("backfill: workspace failed (continuing)", "workspace_id", wsID, "err", err)
			continue
		}
		total += n
	}
	r.log.Info("backfill: done", "registered", total)
	return total, nil
}

// BackfillWorkspace runs the §3.2 backfill for a single workspace in batches of
// opts.BatchSize documents per transaction, FOR UPDATE SKIP LOCKED. Returns the
// count of newly registered document versions.
func (r *Runner) BackfillWorkspace(ctx context.Context, wsID uuid.UUID) (int, error) {
	registered := 0
	for {
		if err := ctx.Err(); err != nil {
			return registered, err
		}
		n, err := r.backfillBatch(ctx, wsID)
		if err != nil {
			return registered, err
		}
		registered += n
		if n == 0 {
			return registered, nil // workspace drained
		}
		r.log.Info("backfill: batch done", "workspace_id", wsID, "batch_registered", n, "total_registered", registered)
	}
}

// backfillBatch selects one batch of unregistered non-deleted documents in wsID
// (FOR UPDATE SKIP LOCKED) and registers each in the same transaction. The
// SELECT is the §3.2 query exactly: documents JOIN their current
// document_version, LEFT JOIN knowledge_assets on native_document_id, NULL =
// unregistered. The FOR UPDATE SKIP LOCKED on documents keeps concurrent
// backfill / dual-write from contending.
//
// Each registered version emits an asset.version.requested outbox event to
// knowledge_events so the projection pipeline can build FTS/vector projections
// for the now-registered asset (§3.2 末段).
func (r *Runner) backfillBatch(ctx context.Context, wsID uuid.UUID) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `
		SELECT d.id, d.workspace_id, d.title, d.created_by, d.version_no::bigint, v.id
		FROM documents d
		JOIN document_versions v
		  ON v.document_id = d.id AND v.version_no = d.version_no
		LEFT JOIN knowledge_assets a
		  ON a.native_document_id = d.id AND a.asset_type = 'document'
		WHERE d.workspace_id = $1
		  AND d.status != 'deleted'
		  AND a.id IS NULL
		ORDER BY d.created_at
		LIMIT $2
		FOR UPDATE OF d SKIP LOCKED`, wsID, r.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	batch := make([]docVersionRow, 0, r.opts.BatchSize)
	for rows.Next() {
		var d docVersionRow
		if err := rows.Scan(&d.DocID, &d.WorkspaceID, &d.Title, &d.CreatedBy, &d.VersionNo, &d.VersionID); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, d)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	if len(batch) == 0 {
		return 0, nil
	}

	// Resolve the workspace's legacy_migration system profile inside the same tx
	// (§3.4): every backfill batch's review decisions reference it.
	profileID, err := r.registry.LegacyMigrationProfileID(ctx, tx, wsID)
	if err != nil {
		return 0, err
	}

	registered := 0
	for _, d := range batch {
		res, err := r.registry.RegisterDocumentAsset(ctx, tx, asset.Registration{
			DocumentID:               d.DocID,
			WorkspaceID:              d.WorkspaceID,
			VersionID:                d.VersionID,
			VersionNo:               d.VersionNo,
			Title:                   d.Title,
			CreatedByType:           domain.SubjectServiceAccount,
			CreatedByID:             r.opts.MigrationServiceAccountID,
			GovernanceProfileID:     &profileID,
			MigrationServiceAccountID: &r.opts.MigrationServiceAccountID,
		})
		if err != nil {
			return registered, err
		}
		// Emit asset.version.requested for the newly registered version so the
		// knowledge_events consumer builds its projections (§3.2). res.Created
		// is false if the version was registered by a racing dual-write — in that
		// case the dual-write already emitted its own event, so we skip to avoid
		// a duplicate (the dedupe_key on the projection job absorbs it anyway).
		if res.Created {
			if err := r.emitVersionRequested(ctx, tx, d.WorkspaceID, res.AssetID, res.AssetVersionID, d.VersionNo, d.VersionID); err != nil {
				return registered, err
			}
			registered++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return registered, nil
}

// listWorkspaces returns every workspace id (§3.2 "对每个 workspace").
func (r *Runner) listWorkspaces(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM workspaces ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// emitVersionRequested records an asset.version.requested Knowledge Outbox
// event for a backfilled version (§3.2). destinations = knowledge_events only;
// the rag-worker's doc_events stream is untouched (§10.5 regression red line).
func (r *Runner) emitVersionRequested(ctx context.Context, tx pgx.Tx, wsID, assetID, assetVersionID uuid.UUID, versionNo int64, docVersionID uuid.UUID) error {
	ev := domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     domain.KEAssetVersionRequested,
		EventVersion:  1,
		AggregateType: domain.AggKnowledgeAsset,
		AggregateID:   assetID,
		WorkspaceID:   &wsID,
		Actor:         domain.EventActor{Type: domain.SubjectServiceAccount, ID: r.opts.MigrationServiceAccountID},
		Payload: map[string]any{
			"asset_id":           assetID.String(),
			"asset_version_id":   assetVersionID.String(),
			"version_no":          versionNo,
			"native_document_id":  docVersionID,
			"origin":              "legacy_migration",
		},
		OccurredAt: time.Now().UTC(),
	}
	return r.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream})
}

// --- reconciliation (§3.3) ---

// ReconcileReport summarizes one reconciliation pass.
type ReconcileReport struct {
	MissingAssets     int // documents with no knowledge_assets row → registered
	MissingVersions   int // document_versions with no asset version → registered
	VersionMismatches int // current_version_id != documents.version_no → CAS-repaired
	Repaired          int // total successful repairs
}

// Reconcile runs the §3.3 consistency scan across all workspaces. It repairs:
//   - documents present but no asset → backfill path (idempotent);
//   - document_versions present but no asset version → register (dedupe_key idempotent);
//   - knowledge_assets.current_version_id != documents.version_no for published
//     assets → CAS update to the document's current version.
//
// Per §3.3 it does NOT repair: assets whose status is not 'published' (may be
// human-deprecated), or projections that persistently fail (Provider fault) —
// those go to human handling. A failing workspace does not abort the others.
func (r *Runner) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	workspaceIDs, err := r.listWorkspaces(ctx)
	if err != nil {
		return rep, err
	}
	for _, wsID := range workspaceIDs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		n, m, v, err := r.reconcileWorkspace(ctx, wsID)
		if err != nil {
			r.log.Error("reconcile: workspace failed (continuing)", "workspace_id", wsID, "err", err)
			continue
		}
		rep.MissingAssets += n
		rep.MissingVersions += m
		rep.VersionMismatches += v
		rep.Repaired += n + m + v
	}
	r.log.Info("reconcile: done", "missing_assets", rep.MissingAssets, "missing_versions", rep.MissingVersions, "mismatches_repaired", rep.VersionMismatches)
	return rep, nil
}

// reconcileWorkspace runs the §3.3 scans for one workspace.
func (r *Runner) reconcileWorkspace(ctx context.Context, wsID uuid.UUID) (missingAssets, missingVersions, mismatches int, err error) {
	// 1. Missing assets → backfill path (resumable, idempotent).
	n, err := r.BackfillWorkspace(ctx, wsID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("reconcile backfill: %w", err)
	}
	missingAssets = n

	// 2. Missing versions: document_versions that have no matching
	// knowledge_asset_versions.native_document_version_id. These are versions
	// written before Phase 1 (or after a partial backfill). Register each.
	mv, err := r.registerMissingVersions(ctx, wsID)
	if err != nil {
		return missingAssets, 0, 0, fmt.Errorf("reconcile versions: %w", err)
	}
	missingVersions = mv

	// 3. current_version_id vs documents.version_no mismatch (§3.3 row 3).
	// Only repair 'published' assets — non-published may be intentionally
	// deprecated by a human (§3.3 不修复：资产状态非 published).
	mm, err := r.repairVersionMismatches(ctx, wsID)
	if err != nil {
		return missingAssets, missingVersions, 0, fmt.Errorf("reconcile mismatches: %w", err)
	}
	mismatches = mm
	return missingAssets, missingVersions, mismatches, nil
}

// registerMissingVersions scans for document_versions lacking an asset version
// and registers each in its own small tx (dedupe_key idempotent). Uses the
// document's current asset row (created by backfill if missing).
func (r *Runner) registerMissingVersions(ctx context.Context, wsID uuid.UUID) (int, error) {
	if r.opts.MigrationServiceAccountID == uuid.Nil {
		return 0, errors.New("migration: MigrationServiceAccountID is required")
	}
	registered := 0
	for {
		if err := ctx.Err(); err != nil {
			return registered, err
		}
		n, err := r.registerMissingVersionBatch(ctx, wsID)
		if err != nil {
			return registered, err
		}
		registered += n
		if n == 0 {
			return registered, nil
		}
	}
}

func (r *Runner) registerMissingVersionBatch(ctx context.Context, wsID uuid.UUID) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `
		SELECT v.id, v.document_id, d.workspace_id, d.title, d.created_by, v.version_no::bigint
		FROM document_versions v
		JOIN documents d ON d.id = v.document_id
		LEFT JOIN knowledge_asset_versions kav ON kav.native_document_version_id = v.id
		WHERE d.workspace_id = $1 AND d.status != 'deleted' AND kav.id IS NULL
		ORDER BY v.created_at
		LIMIT $2 FOR UPDATE OF v SKIP LOCKED`, wsID, r.opts.BatchSize)
	if err != nil {
		return 0, err
	}
	type missRow struct {
		VersionID, DocID, WorkspaceID, CreatedBy uuid.UUID
		Title     string
		VersionNo int64
	}
	batch := make([]missRow, 0, r.opts.BatchSize)
	for rows.Next() {
		var m missRow
		if err := rows.Scan(&m.VersionID, &m.DocID, &m.WorkspaceID, &m.Title, &m.CreatedBy, &m.VersionNo); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, m)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	if len(batch) == 0 {
		return 0, nil
	}
	profileID, err := r.registry.LegacyMigrationProfileID(ctx, tx, wsID)
	if err != nil {
		return 0, err
	}
	registered := 0
	for _, m := range batch {
		res, err := r.registry.RegisterDocumentAsset(ctx, tx, asset.Registration{
			DocumentID:               m.DocID,
			WorkspaceID:              m.WorkspaceID,
			VersionID:                m.VersionID,
			VersionNo:               m.VersionNo,
			Title:                   m.Title,
			CreatedByType:           domain.SubjectServiceAccount,
			CreatedByID:             r.opts.MigrationServiceAccountID,
			GovernanceProfileID:     &profileID,
			MigrationServiceAccountID: &r.opts.MigrationServiceAccountID,
		})
		if err != nil {
			return registered, err
		}
		if res.Created {
			if err := r.emitVersionRequested(ctx, tx, m.WorkspaceID, res.AssetID, res.AssetVersionID, m.VersionNo, m.VersionID); err != nil {
				return registered, err
			}
			registered++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return registered, nil
}

// repairVersionMismatches fixes knowledge_assets.current_version_id so it
// points to the asset version whose native_document_version_id is the
// document's CURRENT document_version (v.version_no = d.version_no). It only
// touches published assets (§3.3). Uses a guarded UPDATE (WHERE the current
// pointer already differs) so a no-op mismatch does not rewrite rows.
func (r *Runner) repairVersionMismatches(ctx context.Context, wsID uuid.UUID) (int, error) {
	// Find published document assets whose current_version_id points at a
	// version that is NOT the document's current version_no.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `
		SELECT a.id, kav_cur.id AS cur_vid, kav_target.id AS target_vid
		FROM knowledge_assets a
		JOIN documents d ON d.id = a.native_document_id AND a.asset_type = 'document'
		LEFT JOIN knowledge_asset_versions kav_cur ON kav_cur.id = a.current_version_id
		JOIN document_versions v_target ON v_target.document_id = d.id AND v_target.version_no = d.version_no
		LEFT JOIN knowledge_asset_versions kav_target ON kav_target.native_document_version_id = v_target.id
		WHERE a.workspace_id = $1
		  AND a.status = 'published'
		  AND a.current_version_id IS DISTINCT FROM kav_target.id`, wsID)
	if err != nil {
		return 0, err
	}
	type mismatch struct {
		assetID, targetVID uuid.UUID
	}
	ms := make([]mismatch, 0)
	for rows.Next() {
		var (
			assetID, curVID, targetVID uuid.UUID
		)
		if err := rows.Scan(&assetID, &curVID, &targetVID); err != nil {
			rows.Close()
			return 0, err
		}
		// targetVID may be NULL if the version isn't registered yet — that's a
		// missing-version case, repaired by registerMissingVersions, skip here.
		if targetVID == uuid.Nil {
			continue
		}
		ms = append(ms, mismatch{assetID: assetID, targetVID: targetVID})
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, rows.Err()
	}
	repaired := 0
	for _, m := range ms {
		// CAS: only set current_version_id to the target if it still differs.
		// latest_requested_version_no is NOT advanced by reconcile — it tracks
		// the highest version ever requested, and a mismatch repair is not a new
		// request (§6.4 单调栅栏). We update the pointer only.
		tag, err := tx.Exec(ctx, `
			UPDATE knowledge_assets
			SET current_version_id = $1, updated_at = now()
			WHERE id = $2 AND current_version_id IS DISTINCT FROM $1`,
			m.targetVID, m.assetID)
		if err != nil {
			return repaired, err
		}
		if tag.RowsAffected() > 0 {
			repaired++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return repaired, nil
}
