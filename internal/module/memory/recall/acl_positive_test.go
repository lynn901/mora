package recall

// acl_positive_test.go covers the positive-ACL paths the defect-fix added
// (design-docs/18 §8.3, §4.3). The prior suite only exercised leak-safe deny
// (denyRBACRepo) + admin bypass — it never asserted a caller WITH read
// permission could actually submit feedback or read an excerpt. That gap let
// defect-1 (mayFeedback passed the unit id as TargetAsset) slip through.
//
// Tested paths:
//   - §8.3 owner shortcut: the unit's creator (non-admin) may Submit feedback.
//     Exercises the fakeUnitReader → asset_id + created_by_id resolution; the
//     owner short-circuits before any rbac.Check (defect-1 fix). Wired with a
//     DENY engine + the unitReader — without the owner shortcut the deny
//     engine would block, so a pass proves the shortcut fired against the
//     unit's real asset (not the unit id, which the AssetLocator cannot resolve).
//   - §8.3 deny for non-owner: a non-owner, non-admin caller with the SAME deny
//     engine + unitReader is DENIED (fail closed). Proves the owner shortcut is
//     principal-specific (not a blanket allow), and the unit id is never passed
//     as TargetAsset (denyRBACRepo grants nothing → rbac.Check on the unit's
//     asset_id returns deny → ErrFeedbackForbidden, sink untouched).
//   - §4.3 step 1 owner shortcut: the unit's creator may ReadExcerpt the
//     evidence referenced by their own unit (step 1 owner allow, no rbac).
//   - §4.3 step 1 blocks a non-owner without unit read: a caller who CAN read
//     the evidence (owner of the evidence) but is NOT the unit owner and has
//     no rbac → step 1 fails closed → Readable=false (defect-2 regression).
//   - §4.3 step 1 missing unit: an unresolvable unit → leak-safe deny at
//     step 1, indistinguishable from a denial (§9.3).
//
// These are unit-level; the real AssetLocator resolution + grant evaluation is
// covered by the integration tests the test engineer adds (issue 协同项).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// fakeUnitReader is a programmable UnitReader for the §4.3 step-1 + §8.3 gates.
// It returns a unit whose AssetID + CreatedByID the services use to evaluate
// the gate (defect-1/2 fix: resolve unit.id → asset_id before rbac.Check).
type fakeUnitReader struct {
	unit domain.MemoryUnit
	err  error
}

func (f *fakeUnitReader) Get(ctx context.Context, id uuid.UUID) (domain.MemoryUnit, error) {
	if f.err != nil {
		return domain.MemoryUnit{}, f.err
	}
	// Echo the requested id so the caller can correlate; keep the configured
	// asset_id + created_by_id (the gate uses those, not the unit id).
	f.unit.ID = id
	return f.unit, nil
}

// TestFeedback_OwnerShortcutAllows verifies §8.3 + the defect-1 fix: a non-admin
// caller who IS the unit's creator may submit feedback. The deny engine would
// block any rbac.Check, so a pass PROVES the owner shortcut fired after the
// unitReader resolved the unit's asset_id + created_by_id — the unit id is
// NEVER passed as TargetAsset (defect-1 root cause). Without the fix, mayFeedback
// passed the unit id; the AssetLocator cannot resolve a memory_unit.id, so the
// engine errored → deny → ErrFeedbackForbidden even for the owner.
func TestFeedback_OwnerShortcutAllows(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	assetID := uuid.New()
	unit := domain.MemoryUnit{
		ID:            uuid.New(),
		AssetID:       assetID,
		CreatedByID:   owner,
		CreatedByType: domain.OwnerAgent,
		State:         domain.MemoryPublished,
	}
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink).
		WithUnits(&fakeUnitReader{unit: unit}).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil) // deny → only the owner shortcut can allow
	id, err := svc.Submit(context.Background(), AuthContext{PrincipalID: owner}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID:  unit.ID,
		FeedbackType:  domain.FeedbackUseful,
	})
	if err != nil {
		t.Fatalf("owner submit must succeed (§8.3 owner shortcut): %v", err)
	}
	if id == uuid.Nil {
		t.Fatal("owner submit must return a feedback_id")
	}
}

// TestFeedback_NonOwnerDeniedByEngine verifies the defect-1 regression: a
// non-owner, non-admin caller with a deny engine + a wired unitReader is DENIED
// (fail closed). mayFeedback resolves the unit's asset_id (not the unit id),
// the owner shortcut does NOT fire (different principal), and rbac.Check on the
// asset_id returns deny → ErrFeedbackForbidden. The deny is leak-safe and the
// sink is not called (no half-stored feedback). Before the fix, the unit id was
// passed as TargetAsset; the engine still denied, so this case passed by
// accident — but the owner case above is the real regression guard.
func TestFeedback_NonOwnerDeniedByEngine(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	assetID := uuid.New()
	unit := domain.MemoryUnit{
		ID:          uuid.New(),
		AssetID:     assetID,
		CreatedByID: owner, // the unit owner — NOT the caller below
		State:       domain.MemoryPublished,
	}
	sink := &fakeFeedbackSink{}
	svc := NewFeedbackService(&fakeFeedbackRepo{}, sink).
		WithUnits(&fakeUnitReader{unit: unit}).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil) // deny → non-owner fails closed
	_, err := svc.Submit(context.Background(), AuthContext{PrincipalID: uuid.New()}, FeedbackRequest{
		WorkspaceID:  uuid.New(),
		MemoryUnitID:  unit.ID,
		FeedbackType:  domain.FeedbackUseful,
	})
	if !errors.Is(err, ErrFeedbackForbidden) {
		t.Fatalf("non-owner under deny engine must be denied (fail closed), got %v", err)
	}
	if sink.gotFB.FeedbackType != "" {
		t.Fatal("denied feedback must NOT reach the sink")
	}
}

// TestReadExcerpt_UnitOwnerShortcutAllows verifies §4.3 step 1 + the defect-2
// fix: the unit's creator (non-admin) passes step 1 (Memory use/read). Step 2
// (Evidence read) is satisfied by the SAME principal owning the evidence (owner
// shortcut in canReadEvidence, §8.3). With nil rbac, the ONLY allow paths are
// the two owner shortcuts — so a pass proves the unitReader resolved the unit's
// asset_id + created_by_id in step 1 (defect-2 fix) AND the evidence owner
// shortcut fired in step 2.
func TestReadExcerpt_UnitOwnerShortcutAllows(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	assetID := uuid.New()
	unit := domain.MemoryUnit{
		ID:          uuid.New(),
		AssetID:     assetID,
		CreatedByID: owner,
		State:       domain.MemoryPublished,
	}
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceToolCall,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "决策：mora-api 监听 :8990。",
		OwnerID:         owner, // same principal → step 2 owner shortcut
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithUnits(&fakeUnitReader{unit: unit}).
		WithAuthz(nil, nil) // no engine → step 1 + step 2 owner shortcuts are the only allow
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{PrincipalID: owner}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: unit.ID,
	})
	if err != nil {
		t.Fatalf("unit-owner + evidence-owner read must succeed: %v", err)
	}
	if !res.Readable {
		t.Fatal("unit owner + evidence owner must pass the full §4.3 chain (Readable=true)")
	}
	if res.Excerpt != ev.RedactedExcerpt {
		t.Fatal("unit owner read must return the redacted excerpt")
	}
}

// TestReadExcerpt_Step1BlocksEvidenceOwner verifies the defect-2 regression:
// a caller who CAN read the evidence (is its owner) but is NOT the unit owner
// and has NO rbac → step 1 (Memory use/read) DENIES before step 2 even runs.
// Before the fix, step 1 did not exist — this caller would have gotten
// Readable=true via the step 2 owner shortcut. After the fix, step 1 fails
// closed (nil rbac, non-unit-owner) → Readable=false. The redacted reference
// (evidence_type) is still surfaced (§4.3).
func TestReadExcerpt_Step1BlocksEvidenceOwner(t *testing.T) {
	t.Parallel()
	unitOwner := uuid.New()
	assetID := uuid.New()
	unit := domain.MemoryUnit{
		ID:          uuid.New(),
		AssetID:     assetID,
		CreatedByID: unitOwner,
		State:       domain.MemoryPublished,
	}
	evOwner := uuid.New() // a DIFFERENT principal owns the evidence
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceToolCall,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "决策：不应被此调用者读取。",
		OwnerID:         evOwner,
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithUnits(&fakeUnitReader{unit: unit}).
		WithAuthz(nil, nil) // no engine → step 1 fail-closed for non-unit-owner
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{PrincipalID: evOwner}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: unit.ID,
	})
	if err != nil {
		t.Fatalf("step-1 deny must not error (§9.3 leak-safe), got %v", err)
	}
	if res.Readable {
		t.Fatal("evidence owner who is NOT the unit owner must be blocked at step 1 (defect-2 fix)")
	}
	// The redacted reference is safe to surface on a deny (§4.3).
	if res.EvidenceType != string(domain.EvidenceSourceToolCall) {
		t.Fatalf("deny must still carry evidence_type, got %q", res.EvidenceType)
	}
}

// TestReadExcerpt_MissingUnitBlocksAtStep1 verifies §4.3 step 1 + §9.3: a
// missing/unresolvable unit (unitReader returns ErrMemoryUnitNotFound) yields
// a leak-safe deny at step 1 — Readable=false, no content, no error
// distinguishing 403/404. The evidence is never expanded.
func TestReadExcerpt_MissingUnitBlocksAtStep1(t *testing.T) {
	t.Parallel()
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceSession,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "决策：unit 不存在。",
		OwnerID:         uuid.New(),
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithUnits(&fakeUnitReader{err: domain.ErrMemoryUnitNotFound}).
		WithAuthz(nil, nil)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{PrincipalID: uuid.New()}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("missing unit must not error (§9.3 leak-safe), got %v", err)
	}
	if res.Readable {
		t.Fatal("missing unit must block at step 1 (Readable=false)")
	}
}
