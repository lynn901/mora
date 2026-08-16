package postgres

// memory_feedback.go implements evidence.FeedbackRepo over the
// 018_phase4_agent_memory.memory_feedback table (design-docs/18 §2.5, D8).
// Feedback never edits the unit statement — it only adjusts authority/freshness
// and may trigger a revalidate Job (the trigger is recorded here; the Job
// enqueue is the recall service's job above this repo).

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// FeedbackRepo adapts memory_feedback.
type FeedbackRepo struct{ db *DB }

// NewFeedbackRepo builds a FeedbackRepo.
func NewFeedbackRepo(db *DB) evidence.FeedbackRepo { return &FeedbackRepo{db: db} }

// Insert records a useful/incorrect/stale signal. The caller has already decided
// revalidate_triggered (true for incorrect/stale when a revalidate Job should fire).
func (r *FeedbackRepo) Insert(ctx context.Context, f domain.MemoryFeedback) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
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
	return id, nil
}

// ListForUnit returns the feedback history for a memory unit (newest first).
func (r *FeedbackRepo) ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryFeedback, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, memory_unit_id, feedback_type, given_by_type, given_by_id,
		       rationale_redacted, revalidate_triggered, created_at
		FROM memory_feedback WHERE memory_unit_id = $1
		ORDER BY created_at DESC`, memoryUnitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MemoryFeedback
	for rows.Next() {
		var f domain.MemoryFeedback
		var ftype, gtype string
		var rationale *string
		if err := rows.Scan(&f.ID, &f.MemoryUnitID, &ftype, &gtype, &f.GivenByID,
			&rationale, &f.RevalidateTriggered, &f.CreatedAt); err != nil {
			return nil, err
		}
		f.FeedbackType = domain.FeedbackType(ftype)
		f.GivenByType = domain.OwnerType(gtype)
		if rationale != nil {
			f.RationaleRedacted = *rationale
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

var _ evidence.FeedbackRepo = (*FeedbackRepo)(nil)
