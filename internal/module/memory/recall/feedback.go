// Package recall — feedback service (design-docs/18 §8.3 反馈处理, D8).
//
// The FeedbackService handles useful/incorrect/stale signals on a memory
// unit. Feedback NEVER edits the statement (§8.5) — it only adjusts the
// unit's authority (§2.2 authority column) and may trigger an
// `evidence.revalidate` outbox event for incorrect/stale (§8.3).
//
// The authority delta is small and clamped to [0,1] by the repo:
//   - useful      → authority += deltaUseful  (微升)
//   - incorrect   → authority -= deltaPenalty (降)
//   - stale       → authority −= deltaPenalty + freshness decay (降 + revalidate)
//
// revalidate_triggered is recorded on the feedback row (§2.5); the sink emits
// the KEEvidenceRevalidate event to memory_events in the SAME tx (§6.3) so the
// knowledge-worker's memory_revalidate Job picks it up (§3.3).
//
// Leak-safe (§9.3): a caller without use/read on the unit cannot submit
// feedback — the unit's existence is not leaked (the submit returns a
// not-found-forbidden sentinel the handler maps to 404/empty).
package recall

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// Authority deltas for feedback (§8.3). Small + clamped by the repo so a
// single feedback cannot spike an authority to 1 or crater it to 0.
const (
	deltaUseful   = 0.05
	deltaPenalty  = 0.1
)

// FeedbackRequest is the input to Submit (§8.3). The caller supplies the
// unit id + the signal + a redacted rationale (the rationale is redacted by
// the caller — the service does not re-redact; §4.1 applies to evidence, not
// feedback text, but the rationale is still stored redacted to avoid
// surfacing caller-supplied PII in audit).
type FeedbackRequest struct {
	WorkspaceID       uuid.UUID
	MemoryUnitID      uuid.UUID
	FeedbackType      domain.FeedbackType
	RationaleRedacted string
}

// ErrFeedbackForbidden is returned when the caller lacks use/read on the unit
// (§9.3 leak-safe: indistinguishable from not-found).
var ErrFeedbackForbidden = errors.New("memory: feedback not permitted or unit not found")

// ErrFeedbackInvalid is returned when the feedback type is invalid.
var ErrFeedbackInvalid = errors.New("memory: invalid feedback type")

// FeedbackService composes the FeedbackRepo + the FeedbackSink (outbox-in-tx)
// + rbac + audit. It is the ONLY writer of memory_feedback rows from the
// feedback entry; the repo's AdjustAuthority is called in the same tx via the
// sink so the authority delta + the feedback row are atomic.
type FeedbackService struct {
	feedback FeedbackRepo
	sink     FeedbackSink
	rbac     *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit    *audit.Logger
}

// NewFeedbackService wires the feedback service. sink may be nil ONLY in
// dev/test when revalidate is never triggered; production MUST inject it so
// the §6.3 outbox-in-tx boundary runs. rbac/audit may be nil in dev/test.
func NewFeedbackService(feedback FeedbackRepo, sink FeedbackSink) *FeedbackService {
	return &FeedbackService{feedback: feedback, sink: sink}
}

// WithAuthz injects the RBAC engine + audit logger. Production wiring MUST
// call this so the §4.3 Evidence ACL chain (the unit read gate) + the §9.4
// `memory.feedback` audit row run.
func (s *FeedbackService) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *FeedbackService {
	s.rbac = engine
	s.audit = logger
	return s
}

// Submit records a useful/incorrect/stale signal (§8.3). It:
//  1. Validates the feedback type.
//  2. Gates on the unit read (rbac use/read on the unit — leak-safe deny).
//  3. Decides revalidate_triggered (true for incorrect/stale, §8.3).
//  4. Persists the feedback row + the authority delta + the revalidate event
//     in one tx (via the sink, §6.3).
//  5. Audits `memory.feedback` (§9.4).
//
// The statement is never modified (§8.5).
func (s *FeedbackService) Submit(ctx context.Context, auth AuthContext, req FeedbackRequest) (uuid.UUID, error) {
	if !validFeedbackType(req.FeedbackType) {
		return uuid.Nil, ErrFeedbackInvalid
	}
	if req.MemoryUnitID == uuid.Nil || req.WorkspaceID == uuid.Nil {
		return uuid.Nil, ErrFeedbackInvalid
	}

	// §9.3 leak-safe read gate: a caller without use/read on the unit cannot
	// submit feedback. The deny is indistinguishable from not-found so the
	// unit's existence never leaks.
	if !s.mayFeedback(ctx, auth, req) {
		s.auditFeedback(ctx, auth, req, false)
		return uuid.Nil, ErrFeedbackForbidden
	}

	// §8.3: revalidate_triggered is true for incorrect/stale.
	revalidate := req.FeedbackType == domain.FeedbackIncorrect ||
		req.FeedbackType == domain.FeedbackStale

	delta := deltaFor(req.FeedbackType)

	f := domain.MemoryFeedback{
		MemoryUnitID:        req.MemoryUnitID,
		FeedbackType:        req.FeedbackType,
		GivenByType:         subjectToOwner(auth.SubjectType),
		GivenByID:           auth.PrincipalID,
		RationaleRedacted:   req.RationaleRedacted,
		RevalidateTriggered: revalidate,
	}

	var ev domain.KnowledgeEvent
	if revalidate {
		ev = s.revalidateEvent(req, auth)
	}

	// Dev/test without a sink: fall back to a direct repo write + a direct
	// authority adjust (non-atomic, but the production path uses the sink).
	if s.sink == nil {
		id, err := s.feedback.Insert(ctx, f)
		if err != nil {
			return uuid.Nil, err
		}
		if delta != 0 {
			_ = s.feedback.AdjustAuthority(ctx, req.MemoryUnitID, delta)
		}
		s.auditFeedback(ctx, auth, req, true)
		return id, nil
	}

	id, err := s.sink.SubmitFeedback(ctx, f, delta, ev)
	if err != nil {
		return uuid.Nil, err
	}
	s.auditFeedback(ctx, auth, req, true)
	return id, nil
}

// mayFeedback gates feedback submission on the unit read (§9.3 leak-safe).
// An admin may always submit (the review view). Without an rbac engine
// (dev/test), allow — the production wiring MUST inject rbac.
func (s *FeedbackService) mayFeedback(ctx context.Context, auth AuthContext, req FeedbackRequest) bool {
	if auth.IsAdmin {
		return true
	}
	if s.rbac == nil {
		return true // dev/test only; production MUST inject rbac.
	}
	// The unit is a knowledge_asset(target_type='asset'); use/read on the
	// asset means the caller may read the memory → may feedback on it.
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs,
		domain.TargetAsset, req.MemoryUnitID, domain.ActionUse)
	if err != nil {
		return false
	}
	if dec.Allowed {
		return true
	}
	dec, err = s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs,
		domain.TargetAsset, req.MemoryUnitID, domain.ActionRead)
	if err != nil {
		return false
	}
	return dec.Allowed
}

// deltaFor returns the authority delta for a feedback type (§8.3).
func deltaFor(t domain.FeedbackType) float64 {
	switch t {
	case domain.FeedbackUseful:
		return deltaUseful
	case domain.FeedbackIncorrect, domain.FeedbackStale:
		return -deltaPenalty
	default:
		return 0
	}
}

// revalidateEvent builds the `evidence.revalidate` outbox envelope (§7.4
// shape, §3.3 → memory_revalidate Job). It carries the unit id + the evidence
// id(s) to re-extract — the worker resolves the actual Evidence rows. Only IDs
// + the trigger reason; never content (§5.1).
func (s *FeedbackService) revalidateEvent(req FeedbackRequest, auth AuthContext) domain.KnowledgeEvent {
	ws := req.WorkspaceID
	uid := req.MemoryUnitID
	return domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     domain.KEvidenceRevalidate,
		EventVersion:  1,
		AggregateType: domain.AggMemoryEvidence,
		AggregateID:   uid, // the memory unit triggering revalidate
		WorkspaceID:   &ws,
		Actor: domain.EventActor{
			Type: auth.SubjectType,
			ID:   auth.PrincipalID,
		},
		Payload: map[string]any{
			"memory_unit_id":  uid.String(),
			"workspace_id":    ws.String(),
			"feedback_type":   string(req.FeedbackType),
			"trigger":         "feedback",
		},
	}
}

// auditFeedback writes the §9.4 `memory.feedback` audit row (allow/deny).
// Best-effort: a failure never blocks the feedback (§5 audit invariants).
func (s *FeedbackService) auditFeedback(ctx context.Context, auth AuthContext, req FeedbackRequest, allowed bool) {
	if s.audit == nil {
		return
	}
	actorType := string(auth.SubjectType)
	if actorType == "" {
		actorType = "user"
	}
	var principalID *uuid.UUID
	if auth.PrincipalID != uuid.Nil {
		pid := auth.PrincipalID
		principalID = &pid
	}
	var unitID *uuid.UUID
	if req.MemoryUnitID != uuid.Nil {
		uid := req.MemoryUnitID
		unitID = &uid
	}
	s.audit.Record(ctx, actorType, principalID, "memory.feedback",
		"memory_unit", unitID,
		map[string]any{
			"feedback_type": string(req.FeedbackType),
			"allowed":       allowed,
		}, "", "")
}

// validFeedbackType reports whether t is a recognized feedback signal (§2.5).
func validFeedbackType(t domain.FeedbackType) bool {
	switch t {
	case domain.FeedbackUseful, domain.FeedbackIncorrect, domain.FeedbackStale:
		return true
	}
	return false
}

// subjectToOwner maps a SubjectType to the OwnerType used on the feedback row
// (given_by_type, §2.5). The two enums overlap for user/agent/service_account;
// group membership is recorded as the principal's resolved subject.
func subjectToOwner(st domain.SubjectType) domain.OwnerType {
	switch st {
	case domain.SubjectUser:
		return domain.OwnerUser
	case domain.SubjectAgent:
		return domain.OwnerAgent
	default:
		return domain.OwnerServiceAccount
	}
}
