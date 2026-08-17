package recall

// feedback_test.go covers the feedback service invariants (design-docs/18
// §8.3, §8.5, §9.3) using in-package fakes — no DB.
//
// Tested invariants:
//   - §8.3: useful → +deltaUseful, incorrect/stale → −deltaPenalty, and
//     revalidate_triggered is true ONLY for incorrect/stale.
//   - §8.5: feedback never edits the statement — the sink receives the
//     feedback row + the authority delta, never a statement mutation.
//   - §9.3: a caller without use/read on the unit gets ErrFeedbackForbidden
//     (indistinguishable from not-found → handler maps to 404).
//   - §8.3: the §6.3 outbox-in-tx boundary runs when a sink is wired (the
//     feedback row + authority delta + revalidate event are one tx).
//   - dev/test fallback: without a sink, the repo Insert + AdjustAuthority
//     run directly (non-atomic, but the contract holds).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// fakeFeedbackRepo records Insert + AdjustAuthority calls. It is the
// dev/test FeedbackRepo (no sink) path.
type fakeFeedbackRepo struct {
	mu        sync.Mutex
	inserted  []domain.MemoryFeedback
	adjUnit   uuid.UUID
	adjDelta  float64
	insertID  uuid.UUID
	insertErr error
}

func (f *fakeFeedbackRepo) Insert(ctx context.Context, fb domain.MemoryFeedback) (uuid.UUID, error) {
	if f.insertErr != nil {
		return uuid.Nil, f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserted = append(f.inserted, fb)
	if f.insertID == uuid.Nil {
		f.insertID = uuid.New()
	}
	fb.ID = f.insertID
	return f.insertID, nil
}

func (f *fakeFeedbackRepo) AdjustAuthority(ctx context.Context, unitID uuid.UUID, delta float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adjUnit = unitID
	f.adjDelta = delta
	return nil
}

// fakeFeedbackSink records the SubmitFeedback call — the §6.3 production path.
type fakeFeedbackSink struct {
	mu       sync.Mutex
	gotFB    domain.MemoryFeedback
	gotDelta float64
	gotEvent domain.KnowledgeEvent
	id       uuid.UUID
	err      error
}

func (s *fakeFeedbackSink) SubmitFeedback(ctx context.Context, f domain.MemoryFeedback, delta float64, ev domain.KnowledgeEvent) (uuid.UUID, error) {
	if s.err != nil {
		return uuid.Nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotFB = f
	s.gotDelta = delta
	s.gotEvent = ev
	if s.id == uuid.Nil {
		s.id = uuid.New()
	}
	return s.id, nil
}

// TestFeedback_UsefulNoRevalidate verifies §8.3: a "useful" signal applies
// +deltaUseful and does NOT trigger revalidate (revalidate is for
// incorrect/stale only). The statement is never mutated (§8.5).
func TestFeedback_UsefulNoRevalidate(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink)
	unitID := uuid.New()
	wsID := uuid.New()
	id, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  wsID,
		MemoryUnitID: unitID,
		FeedbackType: domain.FeedbackUseful,
	})
	if err != nil {
		t.Fatalf("useful submit: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("useful submit must return a feedback id")
	}
	if sink.gotDelta != deltaUseful {
		t.Fatalf("useful delta = %v, want %v", sink.gotDelta, deltaUseful)
	}
	if sink.gotEvent.EventType != "" {
		t.Fatal("useful must NOT trigger a revalidate event (§8.3)")
	}
	if sink.gotFB.RevalidateTriggered {
		t.Fatal("useful must set revalidate_triggered=false (§8.3)")
	}
}

// TestFeedback_IncorrectTriggersRevalidate verifies §8.3: an "incorrect"
// signal applies −deltaPenalty AND triggers the `evidence.revalidate` outbox
// event (KEEvidenceRevalidate). The statement is never mutated (§8.5).
func TestFeedback_IncorrectTriggersRevalidate(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink)
	unitID := uuid.New()
	wsID := uuid.New()
	_, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  wsID,
		MemoryUnitID: unitID,
		FeedbackType: domain.FeedbackIncorrect,
	})
	if err != nil {
		t.Fatalf("incorrect submit: %v", err)
	}
	if sink.gotDelta != -deltaPenalty {
		t.Fatalf("incorrect delta = %v, want %v", sink.gotDelta, -deltaPenalty)
	}
	if sink.gotEvent.EventType != domain.KEvidenceRevalidate {
		t.Fatalf("incorrect must trigger revalidate event, got %q", sink.gotEvent.EventType)
	}
	if !sink.gotFB.RevalidateTriggered {
		t.Fatal("incorrect must set revalidate_triggered=true (§8.3)")
	}
	// The revalidate event carries the unit id + workspace + feedback_type —
	// never content (§5.1).
	if sink.gotEvent.AggregateID != unitID {
		t.Fatal("revalidate event aggregate must be the memory unit id")
	}
	if sink.gotEvent.Payload["memory_unit_id"] != unitID.String() {
		t.Fatal("revalidate event payload must carry memory_unit_id")
	}
	if sink.gotEvent.Payload["feedback_type"] != string(domain.FeedbackIncorrect) {
		t.Fatal("revalidate event payload must carry feedback_type")
	}
}

// TestFeedback_StaleTriggersRevalidate verifies §8.3: a "stale" signal also
// triggers revalidate (same as incorrect).
func TestFeedback_StaleTriggersRevalidate(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink)
	_, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: uuid.New(),
		FeedbackType: domain.FeedbackStale,
	})
	if err != nil {
		t.Fatalf("stale submit: %v", err)
	}
	if sink.gotEvent.EventType != domain.KEvidenceRevalidate {
		t.Fatalf("stale must trigger revalidate event, got %q", sink.gotEvent.EventType)
	}
	if sink.gotDelta != -deltaPenalty {
		t.Fatalf("stale delta = %v, want %v", sink.gotDelta, -deltaPenalty)
	}
}

// TestFeedback_NeverMutatesStatement verifies §8.5: the feedback row carries
// NO statement field — feedback only adjusts authority + records the signal.
// The sink's received feedback has an empty Statement-equivalent (MemoryFeedback
// has no Statement field by schema — enforced by the domain type).
func TestFeedback_NeverMutatesStatement(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink)
	_, _ = svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: uuid.New(),
		FeedbackType: domain.FeedbackIncorrect,
	})
	// MemoryFeedback has no Statement field — the type system enforces §8.5.
	// Assert the row only carries the signal + revalidate flag + rationale.
	if sink.gotFB.FeedbackType != domain.FeedbackIncorrect {
		t.Fatalf("feedback row type = %q, want incorrect", sink.gotFB.FeedbackType)
	}
	if sink.gotFB.RationaleRedacted != "" {
		// allowed, just sanity: we did not set one.
	}
}

// TestFeedback_LeakSafeDeny verifies §9.3: a caller without use/read on the
// unit gets ErrFeedbackForbidden (the unit's existence never leaks — the
// handler maps this to a 404 indistinguishable from not-found). The sink is
// NOT called (no half-stored feedback).
func TestFeedback_LeakSafeDeny(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink).WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	_, err := svc.Submit(context.Background(), AuthContext{PrincipalID: uuid.New()}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: uuid.New(),
		FeedbackType: domain.FeedbackUseful,
	})
	if !errors.Is(err, ErrFeedbackForbidden) {
		t.Fatalf("denied feedback must yield ErrFeedbackForbidden, got %v", err)
	}
	if sink.gotFB.FeedbackType != "" {
		t.Fatal("denied feedback must NOT reach the sink (no half-stored feedback, §9.1)")
	}
}

// TestFeedback_AdminBypassesRBAC verifies the admin short-circuit: an admin
// may submit feedback even under a deny-all engine (the review view).
func TestFeedback_AdminBypassesRBAC(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink).WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	id, err := svc.Submit(context.Background(), AuthContext{
		PrincipalID: uuid.New(),
		IsAdmin:     true,
	}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: uuid.New(),
		FeedbackType: domain.FeedbackUseful,
	})
	if err != nil {
		t.Fatalf("admin feedback: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("admin feedback must succeed (feedback_id set)")
	}
}

// TestFeedback_InvalidTypeRejected verifies an unrecognized feedback_type is
// rejected as ErrFeedbackInvalid before any sink/repo call.
func TestFeedback_InvalidTypeRejected(t *testing.T) {
	t.Parallel()
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink)
	_, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: uuid.New(),
		FeedbackType: "bogus",
	})
	if !errors.Is(err, ErrFeedbackInvalid) {
		t.Fatalf("invalid type must yield ErrFeedbackInvalid, got %v", err)
	}
	if sink.gotFB.FeedbackType != "" {
		t.Fatal("invalid type must NOT reach the sink")
	}
}

// TestFeedback_NilIDsRejected verifies a malformed request (nil unit / nil
// workspace) is rejected as ErrFeedbackInvalid — a 400, not a leak-safe deny
// (the caller supplied a bad request, not a denial).
func TestFeedback_NilIDsRejected(t *testing.T) {
	t.Parallel()
	svc := NewFeedbackService(&fakeFeedbackRepo{}, &fakeFeedbackSink{})
	_, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		FeedbackType: domain.FeedbackUseful,
	})
	if !errors.Is(err, ErrFeedbackInvalid) {
		t.Fatalf("nil IDs must yield ErrFeedbackInvalid, got %v", err)
	}
}

// TestFeedback_DevFallbackWithoutSink verifies the dev/test path: with a nil
// sink, the service falls back to a direct repo Insert + AdjustAuthority
// (non-atomic). The contract (delta applied, revalidate not emitted as an
// event since there is no sink) still holds.
func TestFeedback_DevFallbackWithoutSink(t *testing.T) {
	t.Parallel()
	repo := &fakeFeedbackRepo{}
	svc := NewFeedbackService(repo, nil) // nil sink → dev/test fallback
	unitID := uuid.New()
	id, err := svc.Submit(context.Background(), AuthContext{}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID: unitID,
		FeedbackType: domain.FeedbackUseful,
	})
	if err != nil {
		t.Fatalf("dev fallback submit: %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("dev fallback must return a feedback id")
	}
	if repo.adjUnit != unitID {
		t.Fatal("dev fallback must call AdjustAuthority on the unit")
	}
	if repo.adjDelta != deltaUseful {
		t.Fatalf("dev fallback delta = %v, want %v", repo.adjDelta, deltaUseful)
	}
	if len(repo.inserted) != 1 {
		t.Fatalf("dev fallback must Insert once, got %d", len(repo.inserted))
	}
}
