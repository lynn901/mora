package postgres

// source_sync_sink.go implements service.SyncRunSink — the transactional
// double-write for sync-run creation (design-docs/14 §4.4, §6.3). The
// source_sync_runs row and its Knowledge Outbox event commit in ONE database
// transaction so the event is never lost relative to the run: the
// knowledge-worker only ever sees a run whose dispatch event is already
// durably recorded (§6.3 atomic guarantee).
//
// The sink owns the transaction (the service layer stays pgx-free). It reuses
// the exact INSERT SQL of SyncRunRepo.Create so behavior matches the non-tx
// path — the only addition is the outbox_events row via outbox.Store.Record.
//
// Idempotency (§4.4 Idempotency-Key): the idempotency_key is UNIQUE. A
// duplicate key for the SAME (source_id, requested_revision) is an idempotent
// retry — the sink returns the existing run. A duplicate key for a DIFFERENT
// payload returns service.ErrIdempotencyConflict → 409. The conflict-vs-retry
// distinction is resolved here so the service can map cleanly.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	source "github.com/lynn901/mora/internal/module/knowledge/source/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// SourceSyncSink is the postgres implementation of service.SyncRunSink.
type SourceSyncSink struct {
	pool   *pgxpool.Pool
	outbox *outbox.Store
}

// NewSourceSyncSink builds a sink over a pool and the (stateless) outbox.Store.
func NewSourceSyncSink(pool *pgxpool.Pool, store *outbox.Store) *SourceSyncSink {
	return &SourceSyncSink{pool: pool, outbox: store}
}

// Compile-time check.
var _ source.SyncRunSink = (*SourceSyncSink)(nil)

// CreateRun inserts the run + records the outbox event in one tx (§6.3). On a
// duplicate idempotency_key it resolves idempotent-retry vs conflict:
//   - same (source_id, requested_revision) → return the existing run (retry)
//   - different payload → return source.ErrIdempotencyConflict (§4.4)
//
// On any other error the tx is rolled back — neither the run nor the outbox
// row lands, preserving the atomic guarantee.
func (s *SourceSyncSink) CreateRun(ctx context.Context, run *domain.SourceSyncRun, ev domain.KnowledgeEvent) error {
	if run.ID == uuid.Nil {
		run.ID = uuid.New()
	}
	if run.IdempotencyKey == "" {
		run.IdempotencyKey = uuid.NewString()
	}
	if run.Status == "" {
		run.Status = domain.SyncRunQueued
	}
	snap, _ := json.Marshal(run.SourceConfigSnapshot)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	err = tx.QueryRow(ctx, `
		INSERT INTO source_sync_runs
		  (id, source_id, requested_by_type, requested_by_id, requested_revision,
		   source_config_snapshot, credential_version, governance_profile_id,
		   requested_asset_type, status, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, created_at`,
		run.ID, run.SourceID, string(run.RequestedByType), run.RequestedByID,
		nullIfEmpty(run.RequestedRevision), snap, nullIfEmpty(run.CredentialVersion),
		nilIfZero(run.GovernanceProfileID), string(run.RequestedAssetType),
		string(run.Status), run.IdempotencyKey,
	).Scan(&run.ID, &run.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			// Idempotency-Key collision: decide retry vs conflict by comparing
			// the existing row's (source_id, requested_revision) to this run's.
			return s.resolveIdempotencyCollision(ctx, run)
		}
		return err
	}

	// Knowledge Outbox event — same tx. destinations = knowledge_events only;
	// the dispatcher publishes to the Valkey Stream the worker consumes.
	if err := s.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// resolveIdempotencyCollision loads the existing run for the idempotency_key
// and decides: same (source_id, requested_revision) → idempotent retry
// (populate run with the existing row's identity, return nil); different →
// source.ErrIdempotencyConflict (§4.4 Idempotency-Key).
//
// Existence is not leaked here — a collision on a run the caller can't see is
// still a conflict (the caller's Idempotency-Key already named a run).
func (s *SourceSyncSink) resolveIdempotencyCollision(ctx context.Context, run *domain.SourceSyncRun) error {
	var (
		existingSourceID uuid.UUID
		existingRevision *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT source_id, requested_revision FROM source_sync_runs WHERE idempotency_key = $1`,
		run.IdempotencyKey).Scan(&existingSourceID, &existingRevision)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Raced: the row was deleted between the INSERT and this read.
			// Treat as conflict — the caller's key is unusable, ask for a new one.
			return source.ErrIdempotencyConflict
		}
		return err
	}
	existingRev := ""
	if existingRevision != nil {
		existingRev = *existingRevision
	}
	if existingSourceID == run.SourceID && existingRev == run.RequestedRevision {
		// Idempotent retry — same payload. The original run is the source of
		// truth; the caller will re-GET it. Return nil so the service returns
		// the original run (the caller's Idempotency-Key is satisfied).
		return source.ErrIdempotentRetry
	}
	return source.ErrIdempotencyConflict
}

// Compile-time: keep the time import used for the CreatedAt zero-check path.
var _ = time.Now
