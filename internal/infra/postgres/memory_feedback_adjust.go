package postgres

// memory_feedback_adjust.go adds the recall.FeedbackRepo authority-adjust
// method (design-docs/18 §8.3) to the existing FeedbackRepo (which already
// satisfies evidence.FeedbackRepo over the same memory_feedback table).
//
// AdjustAuthority applies a feedback-driven authority delta to a unit (§8.3:
// useful → +δ, incorrect/stale → −δ), clamped to [0,1] by the DB. It does NOT
// touch the statement — feedback never edits the fact body (§8.5). This is
// the non-tx dev/test fallback path; the production path runs the adjust
// inside the FeedbackSink's tx (memory_feedback_sink.go).

import (
	"context"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
)

// AdjustAuthority applies a clamped authority delta to a memory unit (§8.3).
// A missing unit returns ErrMemoryUnitNotFound (existence leak-safe at the
// service layer; the repo surfaces the not-found sentinel).
func (r *FeedbackRepo) AdjustAuthority(ctx context.Context, unitID uuid.UUID, delta float64) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_units
		   SET authority = LEAST(1.0, GREATEST(0.0, authority + $2)),
		       updated_at = now()
		 WHERE id = $1`, unitID, delta)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrMemoryUnitNotFound
	}
	return nil
}

// Compile-time check: FeedbackRepo also satisfies recall.FeedbackRepo (its
// Insert + ListForUnit come from memory_feedback.go; AdjustAuthority is above).
// The recall.FeedbackRepo port is the narrow write-side port the
// FeedbackService uses, distinct from the full evidence.FeedbackRepo CRUD.
var _ recall.FeedbackRepo = (*FeedbackRepo)(nil)
