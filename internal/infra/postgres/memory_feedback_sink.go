package postgres

// memory_feedback_sink.go implements recall.FeedbackSink — the transactional
// boundary for feedback submission (design-docs/18 §8.3, §6.3).
//
// SubmitFeedback persists the memory_feedback row, applies the authority
// delta to memory_units.authority (clamped to [0,1]), and — when ev carries a
// revalidate event — records the KEEvidenceRevalidate event to memory_events
// in the SAME tx (§6.3). The statement is never touched (§8.5 — feedback only
// adjusts authority/freshness + triggers revalidate).
//
// All SQL is parameterized — no string-concatenated user input (07-security
// §10).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// MemoryFeedbackSink is the postgres implementation of recall.FeedbackSink.
type MemoryFeedbackSink struct {
	db     *DB
	outbox *outbox.Store
}

// NewMemoryFeedbackSink builds the feedback sink over a pool + the outbox
// Store. outbox may be nil ONLY in dev/test when revalidate is never
// triggered; production MUST inject it so the §6.3 outbox-in-tx boundary runs.
func NewMemoryFeedbackSink(db *DB, store *outbox.Store) *MemoryFeedbackSink {
	return &MemoryFeedbackSink{db: db, outbox: store}
}

// Compile-time check: MemoryFeedbackSink satisfies recall.FeedbackSink.
var _ recall.FeedbackSink = (*MemoryFeedbackSink)(nil)

// SubmitFeedback persists the feedback row + the authority delta + (when
// present) the revalidate outbox event, all in one tx (§6.3). ev is the
// pre-built KEEvidenceRevalidate envelope; a zero-value EventType means "no
// revalidate" — only the feedback row + authority adjust are written.
func (s *MemoryFeedbackSink) SubmitFeedback(ctx context.Context, f domain.MemoryFeedback, delta float64, ev domain.KnowledgeEvent) (uuid.UUID, error) {
	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Insert the feedback row (§2.5).
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO memory_feedback
		  (memory_unit_id, feedback_type, given_by_type, given_by_id,
		   rationale_redacted, revalidate_triggered)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id`,
		f.MemoryUnitID, string(f.FeedbackType),
		string(f.GivenByType), f.GivenByID,
		strPtr(f.RationaleRedacted), f.RevalidateTriggered,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}

	// 2. Apply the authority delta (§8.3), clamped to [0,1]. The statement
	//    is never touched (§8.5). A delta of 0 skips the UPDATE (useful signal
	//    only — no authority change). We check existence via the rows-affected
	//    count: a missing unit returns a FK violation from the feedback INSERT
	//    above, so a zero-affect here is a race we surface as not-found.
	if delta != 0 {
		tag, err := tx.Exec(ctx, `
			UPDATE memory_units
			   SET authority = LEAST(1.0, GREATEST(0.0, authority + $2)),
			       updated_at = now()
			 WHERE id = $1`, f.MemoryUnitID, delta)
		if err != nil {
			return uuid.Nil, err
		}
		if tag.RowsAffected() == 0 {
			return uuid.Nil, domain.ErrMemoryUnitNotFound
		}
	}

	// 3. Record the revalidate outbox event when present (§6.3, §3.3 →
	//    memory_revalidate Job). The event + the feedback row share this tx
	//    so the event is never lost. A zero-value EventType means no
	//    revalidate (useful signal) — skip.
	if ev.EventType != "" && s.outbox != nil {
		if err := s.outbox.Record(ctx, tx, ev, []string{domain.MemoryEventsStream}); err != nil {
			return uuid.Nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Compile-time: the outbox Store's Record signature is tx-bound; the sink
// composes it. Ensure the reference stays correct if outbox moves.
var _ = pgx.ErrNoRows
