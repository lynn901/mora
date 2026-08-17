// Package evidence — retention reaper (design-docs/18 §9.2 / §2.4 / §3.3 D3).
//
// The reaper drives the two halves of the evidence lifecycle on a ticker:
//
//  1. Expire: active evidence whose expires_at has passed → pending_purge
//     (stop expansion, content still auditable). Uses RetentionPolicyRepo.PurgeDue
//     + PropagationService.MarkPendingPurge.
//  2. Erase: pending_purge evidence whose purge_after grace window has elapsed
//     → purged (erase content + cascade + audit). Uses RetentionPolicyRepo.PurgeReady
//     + PropagationService.PurgeEvidence.
//
// It is idempotent at every step: MarkPendingPurge and PurgeEvidence are no-ops
// on already-transitioned rows, MinIO Delete is best-effort on a missing
// object, and the audit row is append-only. A transient failure on one row
// surfaces in the tick's returned error (logged by the caller) but does not
// abort the remaining rows of that tick — each row is purged in its own
// recoverable unit so a single bad row doesn't starve the backlog.
//
// The reaper also satisfies worker.Handler (memory_purge job_type) so the
// knowledge-worker dispatch table can drive a per-evidence purge on demand
// (e.g. an explicit-delete event from the outbox), in addition to the ticker.
// The ticker is the reconcile safety net (§3.3 pattern); the job path is the
// event-driven primary.
package evidence

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// DefaultReaperBatch is the per-tick row cap so a backlog doesn't starve other
// workers (same pattern as 018 idx_evidence_purge_due LIMIT).
const DefaultReaperBatch = 100

// DefaultPurgeGrace is the fallback purge_after window for pending_purge
// evidence that has no retention_policy row (explicit delete with no policy).
// Matches the 018 migration seed's 30-day system default.
const DefaultPurgeGrace = 30 * 24 * time.Hour

// ReaperConfig wires the reaper.
type ReaperConfig struct {
	Retention RetentionPolicyRepo
	Propagate *PropagationService
	Batch     int           // per-tick row cap; DefaultReaperBatch when zero
	Grace     time.Duration // fallback purge_after; DefaultPurgeGrace when zero
	Now       func() time.Time
	Logger    *log.Logger // nil → log.Default()
}

// Reaper scans due evidence and drives it through the purge lifecycle.
type Reaper struct {
	cfg ReaperConfig
}

// NewReaper builds a Reaper. Batch/Grace/Now/Logger default when zero/nil so a
// minimal config (Retention + Propagate) is valid.
func NewReaper(cfg ReaperConfig) *Reaper {
	if cfg.Batch == 0 {
		cfg.Batch = DefaultReaperBatch
	}
	if cfg.Grace == 0 {
		cfg.Grace = DefaultPurgeGrace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Reaper{cfg: cfg}
}

// Tick runs one expiry + erase pass and returns the counts + the first error
// encountered (non-fatal: the tick processes remaining rows after a row error
// so one bad row doesn't block the backlog). The caller (a ticker loop) logs
// err and continues; idempotency makes a retry-tick safe.
func (r *Reaper) Tick(ctx context.Context) (expired, erased int, err error) {
	now := r.cfg.Now()

	// 1. Expire: active → pending_purge.
	due, derr := r.cfg.Retention.PurgeDue(ctx, now, r.cfg.Batch)
	if derr != nil {
		return expired, erased, derr
	}
	for _, e := range due {
		if merr := r.cfg.Propagate.MarkPendingPurge(ctx, e.ID); merr != nil {
			// Surface the first error but keep going — the row stays active
			// and the next tick retries it.
			if err == nil {
				err = merr
			}
			continue
		}
		expired++
	}

	// 2. Erase: pending_purge (grace elapsed) → purged + cascade.
	ready, rerr := r.cfg.Retention.PurgeReady(ctx, now, r.cfg.Grace, r.cfg.Batch)
	if rerr != nil {
		return expired, erased, rerr
	}
	for _, e := range ready {
		if _, perr := r.cfg.Propagate.PurgeEvidence(ctx, e.ID); perr != nil {
			if err == nil {
				err = perr
			}
			continue
		}
		erased++
	}
	return expired, erased, err
}

// RunLoop runs Tick on an interval until ctx is cancelled. It is the reconcile
// safety net (§3.3): the event-driven job path is the primary, this ticker
// catches anything the outbox missed (a crashed purge, a missed event). Each
// tick's error is logged, not fatal.
func (r *Reaper) RunLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			expired, erased, err := r.Tick(ctx)
			if err != nil {
				r.cfg.Logger.Printf("evidence reaper: tick err (expired=%d erased=%d): %v", expired, erased, err)
			}
		}
	}
}

// JobMemoryPurge is the worker.Handler job_type for an on-demand per-evidence
// purge (explicit delete / retention event). Dedupe key: memory_purge:{id}.
const JobMemoryPurge = "memory_purge"

// Run satisfies worker.Handler for the memory_purge job_type. The job's
// TargetKey carries the evidence id to purge; a missing/empty TargetKey is a
// permanent (dead) failure, a transient purge failure is retryable. This lets
// the knowledge-worker dispatch table drive purges from outbox events while
// the ticker remains the reconcile backstop.
//
// Implemented as a method on Reaper so the same PropagationService wiring
// serves both paths (no second composition). The job path is a thin shim:
// it parses the id and calls PurgeEvidence — the cascade + audit + idempotency
// all live in PropagationService.PurgeEvidence.
func (r *Reaper) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	id, err := parseEvidenceID(j.TargetKey)
	if err != nil {
		return domain.RetryPermanent, err
	}
	if _, err := r.cfg.Propagate.PurgeEvidence(ctx, id); err != nil {
		// ErrEvidenceNotFound on a re-delivery (already purged + deleted) is
		// success, not a retry — the job's work is done.
		if err == domain.ErrEvidenceNotFound {
			return domain.RetryTransient, nil
		}
		return domain.RetryTransient, err
	}
	return domain.RetryTransient, nil
}

// parseEvidenceID extracts the evidence id from a job's TargetKey. Accepts a
// bare uuid (the memory_purge job shape) — the worker's ReconcileHandler uses
// the same bare-uuid-in-TargetKey convention (cmd/knowledge-worker ReconcileHandler).
func parseEvidenceID(targetKey string) (uuid.UUID, error) {
	if targetKey == "" {
		return uuid.Nil, errors.New("memory_purge: empty target_key (evidence id)")
	}
	id, err := uuid.Parse(targetKey)
	if err != nil {
		return uuid.Nil, fmt.Errorf("memory_purge: invalid evidence id in target_key: %w", err)
	}
	return id, nil
}
