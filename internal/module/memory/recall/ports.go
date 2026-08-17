// Package recall — ports (design-docs/18 §8.1, §8.3).
//
// The recall service depends on narrow ports, not the full evidence CRUD
// repos, so the service stays testable with fakes and the infra layer
// (postgres/memory_recall.go) owns the actual query + ranking SQL. The
// FeedbackSink is the transactional boundary for feedback submission (same
// outbox-in-tx pattern as evidence.EvidenceSink, §6.3).
package recall

import (
	"context"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// UnitRow is one row the RecallRepo returns before the service shapes it into
// a KnowledgeCandidate. It carries the unit + its aggregated authority signal
// + the first backing evidence link (for the traceable citation, §8.1).
// EvidenceLink may be zero-valued when the unit has no surviving evidence
// (evidence_missing=true) — the citation still names the unit, just without
// an evidence_id.
type UnitRow struct {
	Unit          domain.MemoryUnit
	EvidenceLink  *domain.MemoryEvidenceLink // nil when no surviving link
	RelationHints []RelationHint             // contradicts/supersedes relations to surface (§8.2)
}

// RelationHint is one relation the recall query found for a unit (§8.2). The
// service shapes these into RelationSummary; the repo only loads the raw
// relation_type + target id + the target's statement (as the title).
type RelationHint struct {
	RelationType string
	TargetID     uuid.UUID
	TargetTitle  string
}

// RecallRepo is the persistence port over the memory recall query
// (design-docs/18 §8.1, implemented in infra/postgres/memory_recall.go).
//
// It applies the §8.1 filter axes (workspace / owner / memory_type / time /
// validity / linked-asset), the §9.5 authority ranking (evidence_missing
// desc → confidence → freshness → authority), and the §9.3 leak-safe default:
// by default only published units are returned; IncludeCandidates is honored
// ONLY when the caller is the owner (the service enforces that guard before
// calling here, passing includeCandidates=false for non-owners).
//
// The repo does NOT do per-row Evidence ACL expansion — that is the service's
// job (it filters candidates whose backing evidence the caller cannot read).
// The repo returns the unit + its evidence link id; the service decides
// whether the citation is readable.
type RecallRepo interface {
	// Recall returns ranked memory unit rows matching the query. A caller
	// without any visible units gets an empty slice (never an error) so
	// existence does not leak (§9.3). MaxItems<=0 defaults to a sane cap.
	Recall(ctx context.Context, q KnowledgeQuery, includeCandidates bool, maxItems int) ([]UnitRow, error)
}

// FeedbackRepo is the persistence port over memory_feedback (§2.5). It is the
// narrow write port the FeedbackService uses — distinct from the full
// evidence.FeedbackRepo CRUD so the recall package does not import the evidence
// package's wider surface (same narrow-port precedent as authz.SourceRepo).
type FeedbackRepo interface {
	// Insert records a useful/incorrect/stale signal. The caller has already
	// decided revalidate_triggered (true for incorrect/stale per §8.3).
	Insert(ctx context.Context, f domain.MemoryFeedback) (uuid.UUID, error)
	// AdjustAuthority applies a feedback-driven authority delta to a unit
	// (§8.3: useful → +δ, incorrect/stale → −δ). It does NOT touch the
	// statement — feedback never edits the fact body (§8.5). Clamped to
	// [0,1] by the implementation.
	AdjustAuthority(ctx context.Context, unitID uuid.UUID, delta float64) error
}

// FeedbackSink is the transactional boundary for feedback submission (§6.3):
// it persists the memory_feedback row, applies the authority delta, and — when
// revalidate is triggered — writes the `evidence.revalidate` outbox event to
// memory_events, all in one tx. Same outbox-in-tx pattern as
// evidence.EvidenceSink. When no revalidate is needed (delta != 0 only), the
// sink still inserts the feedback row + adjusts authority in one tx for
// atomicity. A non-zero delta is applied to memory_units.authority (clamped to
// [0,1] by the implementation); the statement is never touched (§8.5).
type FeedbackSink interface {
	// SubmitFeedback persists the feedback row, applies the authority delta,
	// and — when ev.EventType is non-empty — records the revalidate event to
	// memory_events in the SAME tx (§6.3). Returns the feedback id. ev is the
	// pre-built KEEvidenceRevalidate envelope (zero-value when no revalidate).
	SubmitFeedback(ctx context.Context, f domain.MemoryFeedback, delta float64, ev domain.KnowledgeEvent) (uuid.UUID, error)
}

// EvidenceReader is the minimal read port the recall service uses for the
// memory_evidence_read ACL path (§4.3) and to resolve a unit's backing
// evidence for the citation. It returns the redacted excerpt + the row's
// visibility/owner/source_asset so the §4.3 ACL chain can be evaluated. A
// missing/deleted/purged row returns ErrEvidenceNotFound (existence never
// leaks, §9.3).
type EvidenceReader interface {
	Get(ctx context.Context, id uuid.UUID) (domain.MemoryEvidence, error)
}

// LinkReader is the minimal read port over memory_evidence_links the recall
// service uses to resolve a unit's backing evidence (for the citation + the
// §4.3 ACL chain). Distinct from the full evidence.EvidenceLinkRepo.
type LinkReader interface {
	ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryEvidenceLink, error)
}
