package recall

// service_test.go covers the recall + read-excerpt leak-safe invariants
// (design-docs/18 §9.3, §8.5, §4.3) using in-package fakes — no DB.
//
// Tested invariants:
//   - §9.3: an unauthorized / unpublished / non-owner-private recall returns an
//     EMPTY slice, never an error (existence does not leak).
//   - §8.5: a non-owner's IncludeCandidates is silently downgraded to
//     published-only so a private candidate's existence never leaks.
//   - §4.3: a denied/missing evidence read returns Readable=false with the
//     redacted reference shape, never an error distinguishing 403/404.
//   - §9.5: the repo's ranking is preserved (service does not re-sort).
//   - admin bypass: an admin may opt into candidates (review view) + reads
//     evidence (owner shortcut absent, but IsAdmin short-circuits).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// fakeRecallRepo is a programmable RecallRepo for the leak-safe tests.
type fakeRecallRepo struct {
	// rows is the ranked set the repo returns for any query.
	rows []UnitRow
	// includeCandidatesSeen records the value the service passed through.
	includeCandidatesSeen bool
	maxItemsSeen          int
	err                   error
}

func (f *fakeRecallRepo) Recall(ctx context.Context, q KnowledgeQuery, includeCandidates bool, maxItems int) ([]UnitRow, error) {
	f.includeCandidatesSeen = includeCandidates
	f.maxItemsSeen = maxItems
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeEvidenceReader is a programmable EvidenceReader for the §4.3 path.
type fakeEvidenceReader struct {
	row domain.MemoryEvidence
	err error
}

func (e *fakeEvidenceReader) Get(ctx context.Context, id uuid.UUID) (domain.MemoryEvidence, error) {
	if e.err != nil {
		return domain.MemoryEvidence{}, e.err
	}
	return e.row, nil
}

// fakeLinkReader is a no-op LinkReader (the service does not call it in the
// first version — citations come from the repo's EvidenceLink on the UnitRow).
type fakeLinkReader struct{}

func (fakeLinkReader) ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	return nil, nil
}

// allowRBACRepo is a Repository that grants every action (§4.3 allow path).
// Unused in the current test set (the deny path uses denyRBACRepo), but kept
// for completeness: it returns an allow-effect grant so the engine resolves
// the subject as permitted on any target.
type allowRBACRepo struct{}

func (allowRBACRepo) GrantsFor(ctx context.Context, subjectID uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) ([]domain.Grant, error) {
	return []domain.Grant{{
		SubjectType: domain.SubjectUser,
		SubjectID:   subjectID,
		Actions:     []domain.Action{domain.ActionRead, domain.ActionUse},
		TargetType:  domain.TargetAsset,
		Effect:      domain.EffectAllow,
	}}, nil
}
func (allowRBACRepo) DirectoryAncestors(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (allowRBACRepo) DocumentLocation(ctx context.Context, documentID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}
func (allowRBACRepo) DocumentsInDirectorySubtree(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// denyRBACRepo returns no grants → the engine default-denies every check.
type denyRBACRepo struct{}

func (denyRBACRepo) GrantsFor(ctx context.Context, subjectID uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) ([]domain.Grant, error) {
	return nil, nil
}
func (denyRBACRepo) DirectoryAncestors(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (denyRBACRepo) DocumentLocation(ctx context.Context, documentID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}
func (denyRBACRepo) DocumentsInDirectorySubtree(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// makeUnit builds a MemoryUnit row for a published fact owned by ownerID.
func makeUnit(ownerID uuid.UUID, state domain.MemoryUnitState, evidenceMissing bool) domain.MemoryUnit {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return domain.MemoryUnit{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		AssetID:         uuid.New(),
		MemoryType:      domain.MemoryFact,
		Statement:       "mora-api 监听 :8990，工作区隔离走 ltree path。",
		State:           state,
		EvidenceMissing: evidenceMissing,
		Authority:       0.8,
		CreatedByType:   domain.OwnerAgent,
		CreatedByID:     ownerID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// TestRecall_RepoErrorPropagatesToHandler verifies the service faithfully
// surfaces a repo infrastructure error (a transient DB failure). The §9.3
// leak-safe collapse (empty list, never an error code) is the HANDLER's job
// (memory_recall.go maps any non-ErrInvalidQuery error to an empty list) — the
// service does not silently swallow infra errors, so a real outage is
// observable upstream rather than masquerading as "no rows".
func TestRecall_RepoErrorPropagatesToHandler(t *testing.T) {
	t.Parallel()
	repo := &fakeRecallRepo{err: errors.New("connection reset")}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	out, err := svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{
		WorkspaceID: uuid.New(),
	})
	if err == nil {
		t.Fatal("repo infra error must propagate to the handler (not silently empty)")
	}
	if out != nil {
		t.Fatalf("repo error must yield nil slice, got %d items", len(out))
	}
}

// TestRecall_EmptyWorkspaceYieldsEmpty verifies the §9.3 leak-safe empty: a
// workspace the caller cannot see (no rows) returns an empty slice, never an
// error. The empty case is indistinguishable from an unauthorized workspace.
func TestRecall_EmptyWorkspaceYieldsEmpty(t *testing.T) {
	t.Parallel()
	repo := &fakeRecallRepo{rows: nil}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	out, err := svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{
		WorkspaceID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("empty workspace must not error, got %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty workspace must yield empty slice, got %d", len(out))
	}
}

// TestRecall_NonOwnerCandidatesDowngraded verifies §8.5/§9.3: a non-owner's
// IncludeCandidates=true is silently downgraded to published-only so a
// private candidate's existence never leaks. The repo must receive
// includeCandidates=false for a non-owner non-admin caller.
func TestRecall_NonOwnerCandidatesDowngraded(t *testing.T) {
	t.Parallel()
	repo := &fakeRecallRepo{
		rows: []UnitRow{{Unit: makeUnit(uuid.New(), domain.MemoryPublished, false)}},
	}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	// A non-owner non-admin caller asks for candidates.
	_, _ = svc.Recall(context.Background(), AuthContext{PrincipalID: uuid.New()}, KnowledgeQuery{
		WorkspaceID:       uuid.New(),
		IncludeCandidates: true,
	})
	if repo.includeCandidatesSeen {
		t.Fatal("non-owner IncludeCandidates must be downgraded to false (§8.5 leak-safe)")
	}
}

// TestRecall_OwnerMayOptIntoCandidates verifies §8.5: the owner (query.OwnerID
// == auth.PrincipalID) may opt into unpublished candidates. The repo receives
// includeCandidates=true.
func TestRecall_OwnerMayOptIntoCandidates(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	repo := &fakeRecallRepo{
		rows: []UnitRow{{Unit: makeUnit(owner, domain.MemoryCandidate, false)}},
	}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	_, _ = svc.Recall(context.Background(), AuthContext{PrincipalID: owner}, KnowledgeQuery{
		WorkspaceID:       uuid.New(),
		OwnerID:           &owner,
		IncludeCandidates: true,
	})
	if !repo.includeCandidatesSeen {
		t.Fatal("owner IncludeCandidates must be honored (§8.5)")
	}
}

// TestRecall_AdminMayOptIntoCandidates verifies §8.5: an admin may opt into
// candidates (the review view) even when not the owner.
func TestRecall_AdminMayOptIntoCandidates(t *testing.T) {
	t.Parallel()
	repo := &fakeRecallRepo{
		rows: []UnitRow{{Unit: makeUnit(uuid.New(), domain.MemoryCandidate, false)}},
	}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	_, _ = svc.Recall(context.Background(), AuthContext{
		PrincipalID: uuid.New(),
		IsAdmin:     true,
	}, KnowledgeQuery{
		WorkspaceID:       uuid.New(),
		IncludeCandidates: true,
	})
	if !repo.includeCandidatesSeen {
		t.Fatal("admin IncludeCandidates must be honored (§8.5 review view)")
	}
}

// TestRecall_MissingWorkspaceIsInvalidQuery verifies the workspace is
// mandatory (§8.1 — recall is always workspace-scoped). A nil workspace
// surfaces as ErrInvalidQuery (a 400, not a leak-safe empty — the caller
// supplied a malformed request, not a denial).
func TestRecall_MissingWorkspaceIsInvalidQuery(t *testing.T) {
	t.Parallel()
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, &fakeEvidenceReader{})
	_, err := svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{})
	if !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("nil workspace must yield ErrInvalidQuery, got %v", err)
	}
}

// TestRecall_DefaultMaxItemsApplied verifies a missing MaxItems is capped to
// defaultRecallCap (§9.6 budget — a single asset cannot dominate).
func TestRecall_DefaultMaxItemsApplied(t *testing.T) {
	t.Parallel()
	repo := &fakeRecallRepo{
		rows: []UnitRow{{Unit: makeUnit(uuid.New(), domain.MemoryPublished, false)}},
	}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	_, _ = svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{
		WorkspaceID: uuid.New(),
	})
	if repo.maxItemsSeen != defaultRecallCap {
		t.Fatalf("MaxItems=0 must default to %d, got %d", defaultRecallCap, repo.maxItemsSeen)
	}
}

// TestRecall_PreservesRepoRanking verifies §9.5: the service does NOT re-sort —
// it returns candidates in the repo's ranked order. Two units with descending
// authority stay in repo order.
func TestRecall_PreservesRepoRanking(t *testing.T) {
	t.Parallel()
	u1 := makeUnit(uuid.New(), domain.MemoryPublished, false)
	u1.Authority = 0.9
	u2 := makeUnit(uuid.New(), domain.MemoryPublished, false)
	u2.Authority = 0.5
	repo := &fakeRecallRepo{rows: []UnitRow{{Unit: u1}, {Unit: u2}}}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	out, err := svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{
		WorkspaceID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(out))
	}
	if out[0].UnitID != u1.ID || out[1].UnitID != u2.ID {
		t.Fatal("service must preserve repo ranking order (§9.5)")
	}
}

// TestRecall_CarriesCitation verifies §8.1: a candidate with a surviving
// evidence link carries the traceable citation (evidence_id + quote_locator +
// support_type). The ProjectionRef is never serialized (unexported field).
func TestRecall_CarriesCitation(t *testing.T) {
	t.Parallel()
	u := makeUnit(uuid.New(), domain.MemoryPublished, false)
	evID := uuid.New()
	repo := &fakeRecallRepo{rows: []UnitRow{{
		Unit: u,
		EvidenceLink: &domain.MemoryEvidenceLink{
			MemoryUnitID: u.ID,
			EvidenceID:   evID,
			QuoteLocator: map[string]any{"offset": 12, "len": 40},
			SupportType:  domain.Supports,
		},
	}}}
	svc := NewRecallService(repo, fakeLinkReader{}, &fakeEvidenceReader{})
	out, err := svc.Recall(context.Background(), AuthContext{}, KnowledgeQuery{WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1, got %d", len(out))
	}
	if out[0].Citation.EvidenceID != evID {
		t.Fatal("citation must carry the surviving evidence_id (§8.1)")
	}
	if out[0].Citation.SupportType != string(domain.Supports) {
		t.Fatal("citation must carry support_type (§8.1)")
	}
	if out[0].Citation.QuoteLocator["offset"] != 12 {
		t.Fatal("citation must carry quote_locator (§8.1)")
	}
}

// TestReadExcerpt_MissingEvidenceIsLeakSafe verifies §4.3/§9.3: a missing
// evidence row (ErrEvidenceNotFound) returns Readable=false +
// EvidenceMissing=true, NEVER an error — indistinguishable from a deny.
func TestReadExcerpt_MissingEvidenceIsLeakSafe(t *testing.T) {
	t.Parallel()
	reader := &fakeEvidenceReader{err: domain.ErrEvidenceNotFound}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{}, ReadExcerptRequest{
		WorkspaceID:  uuid.New(),
		EvidenceID:   uuid.New(),
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("missing evidence must not error (§9.3), got %v", err)
	}
	if res.Readable {
		t.Fatal("missing evidence must be Readable=false")
	}
	if !res.EvidenceMissing {
		t.Fatal("missing evidence must set EvidenceMissing=true")
	}
}

// TestReadExcerpt_DeniedIsLeakSafe verifies §4.3/§9.3: a caller who fails the
// Evidence ACL chain gets Readable=false + the redacted reference
// (evidence_type + verification_status), never the original content, never an
// error. The deny is indistinguishable from a missing row.
func TestReadExcerpt_DeniedIsLeakSafe(t *testing.T) {
	t.Parallel()
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceToolCall,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "决策：mora-api 监听 :8990。",
		OwnerID:         uuid.New(), // not the caller
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{PrincipalID: uuid.New()}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("denied read must not error (§9.3), got %v", err)
	}
	if res.Readable {
		t.Fatal("denied read must be Readable=false")
	}
	if res.Excerpt != "" {
		t.Fatal("denied read must NOT return the original content (§4.3)")
	}
	// The redacted reference (evidence_type + verification_status) is safe to
	// surface even on a deny (§4.3).
	if res.EvidenceType != string(domain.EvidenceSourceToolCall) {
		t.Fatalf("denied read must carry evidence_type, got %q", res.EvidenceType)
	}
	if res.VerificationStatus != string(domain.EvidenceActive) {
		t.Fatalf("denied read must carry verification_status, got %q", res.VerificationStatus)
	}
}

// TestReadExcerpt_OwnerShortcutAllowed verifies §4.3/§8.3: the owner of a
// private evidence row may read it (owner shortcut) even under a deny-all
// engine — the owner is always allowed on their own private evidence.
func TestReadExcerpt_OwnerShortcutAllowed(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceSession,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "决策：admin 旁路 RBAC。",
		OwnerID:         owner,
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{PrincipalID: owner}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if !res.Readable {
		t.Fatal("owner must be allowed to read own evidence (§8.3 shortcut)")
	}
	if res.Excerpt != ev.RedactedExcerpt {
		t.Fatal("owner read must return the redacted excerpt (§4.3)")
	}
}

// TestReadExcerpt_AdminBypass verifies §4.3: an admin caller short-circuits
// the ACL chain to allowed — the review view may read any evidence.
func TestReadExcerpt_AdminBypass(t *testing.T) {
	t.Parallel()
	ev := domain.MemoryEvidence{
		ID:              uuid.New(),
		WorkspaceID:     uuid.New(),
		SourceKind:      domain.EvidenceSourceMessage,
		State:           domain.EvidenceActive,
		RedactedExcerpt: "联系 redacted 获取 runbook。",
		OwnerID:         uuid.New(), // not the admin
	}
	reader := &fakeEvidenceReader{row: ev}
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, reader).
		WithAuthz(rbac.NewEngine(denyRBACRepo{}), nil)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{
		PrincipalID: uuid.New(),
		IsAdmin:     true,
	}, ReadExcerptRequest{
		WorkspaceID:  ev.WorkspaceID,
		EvidenceID:   ev.ID,
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("admin read: %v", err)
	}
	if !res.Readable {
		t.Fatal("admin must bypass the ACL chain (§4.3)")
	}
}

// TestReadExcerpt_NilEvidenceReaderFailsClosed verifies §4.3: with no
// EvidenceReader wired, the service fails closed — Readable=false,
// EvidenceMissing=true, no content. It does NOT leak by expanding a citation
// it cannot resolve.
func TestReadExcerpt_NilEvidenceReaderFailsClosed(t *testing.T) {
	t.Parallel()
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, nil)
	res, err := svc.ReadExcerpt(context.Background(), AuthContext{}, ReadExcerptRequest{
		WorkspaceID:  uuid.New(),
		EvidenceID:   uuid.New(),
		MemoryUnitID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("nil reader must not error (§9.3), got %v", err)
	}
	if res.Readable {
		t.Fatal("nil reader must fail closed (Readable=false)")
	}
	if !res.EvidenceMissing {
		t.Fatal("nil reader must set EvidenceMissing=true")
	}
}

// TestReadExcerpt_InvalidRequestSurfacesAsError verifies the ONE case that is
// NOT leak-safe: a malformed request (nil workspace / nil evidence id) surfaces
// as ErrEvidenceReadInvalid. The caller supplied a bad request — that is a 400,
// not a denial, and surfacing it does not leak existence (no id was supplied).
func TestReadExcerpt_InvalidRequestSurfacesAsError(t *testing.T) {
	t.Parallel()
	svc := NewRecallService(&fakeRecallRepo{}, fakeLinkReader{}, &fakeEvidenceReader{})
	_, err := svc.ReadExcerpt(context.Background(), AuthContext{}, ReadExcerptRequest{})
	if !errors.Is(err, ErrEvidenceReadInvalid) {
		t.Fatalf("nil workspace/evidence must yield ErrEvidenceReadInvalid, got %v", err)
	}
}
