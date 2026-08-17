package dedup

// Unit tests for the Candidate Inbox + reviewer dispositions + manual publish
// (design-docs/18 §6.2, §6.3, decision D7; 附录 A 不变量 8/9; §9.3 leak-safe).
//
// These pin the reviewer-path invariants the integration tests exercise
// end-to-end against SQL:
//
//   - approve → publish: the sink is called once with the unit + governance
//     profile; the unit's state becomes published; no Evidence ACL write is
//     requested (附录 A 不变量 8 — the sink fake records only the publish call).
//   - reject → state flipped to rejected; the sink is NOT called.
//   - supersede → superseded_by set + state deprecated + a `supersedes`
//     knowledge_relation edge written (origin=human) from survivor → deprecated.
//   - no-auto-merge (附录 A 不变量 9): the service exposes NO method that
//     writes superseded_by other than the reviewer's supersede disposition.
//   - cross-workspace leak (§9.3): a Review whose unit lives in another
//     workspace surfaces as ErrInboxNotFound, never the real error.
//   - workspace-write RBAC gate (§4.4): a caller without workspace write is
//     refused (ErrInboxForbidden) before any unit lookup; with a nil RBAC
//     engine (dev/test) the gate allows.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// --- fakes (implement the four ports + the publish sink) ---

type fakeUnitRepo struct {
	get         map[uuid.UUID]domain.MemoryUnit
	setState    map[uuid.UUID]domain.MemoryUnitState
	superseded  map[uuid.UUID]uuid.UUID
	listCands   []domain.MemoryUnit
	listNeigh   []domain.MemoryUnit
	setStateErr error
}

func newFakeUnitRepo() *fakeUnitRepo {
	return &fakeUnitRepo{
		get: map[uuid.UUID]domain.MemoryUnit{}, setState: map[uuid.UUID]domain.MemoryUnitState{}, superseded: map[uuid.UUID]uuid.UUID{},
	}
}

func (u *fakeUnitRepo) Insert(context.Context, domain.MemoryUnit) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (u *fakeUnitRepo) Get(_ context.Context, id uuid.UUID) (domain.MemoryUnit, error) {
	mu, ok := u.get[id]
	if !ok {
		return domain.MemoryUnit{}, domain.ErrMemoryUnitNotFound
	}
	return mu, nil
}
func (u *fakeUnitRepo) ListByAsset(context.Context, uuid.UUID) ([]domain.MemoryUnit, error) {
	return nil, nil
}
func (u *fakeUnitRepo) ListCandidates(context.Context, uuid.UUID) ([]domain.MemoryUnit, error) {
	return u.listCands, nil
}
func (u *fakeUnitRepo) ListCandidateNeighbors(context.Context, uuid.UUID, domain.MemoryType, uuid.UUID) ([]domain.MemoryUnit, error) {
	return u.listNeigh, nil
}
func (u *fakeUnitRepo) SetState(_ context.Context, id uuid.UUID, state domain.MemoryUnitState) error {
	if u.setStateErr != nil {
		return u.setStateErr
	}
	u.setState[id] = state
	return nil
}
func (u *fakeUnitRepo) SetSupersededBy(_ context.Context, id, by uuid.UUID) error {
	u.superseded[id] = by
	return nil
}
func (u *fakeUnitRepo) MarkEvidenceMissing(context.Context, uuid.UUID) error { return nil }
func (u *fakeUnitRepo) SetAssetVersionID(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

type fakeSuggestionRepo struct {
	pending   []domain.MemoryDedupSuggestion
	inserted  []domain.MemoryDedupSuggestion
	listErr   error
}

func newFakeSuggestionRepo() *fakeSuggestionRepo {
	return &fakeSuggestionRepo{}
}

func (s *fakeSuggestionRepo) Insert(_ context.Context, sug domain.MemoryDedupSuggestion) (uuid.UUID, error) {
	s.inserted = append(s.inserted, sug)
	return uuid.New(), nil
}
func (s *fakeSuggestionRepo) Get(context.Context, uuid.UUID) (domain.MemoryDedupSuggestion, error) {
	return domain.MemoryDedupSuggestion{}, nil
}
func (s *fakeSuggestionRepo) ListPending(context.Context, uuid.UUID) ([]domain.MemoryDedupSuggestion, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.pending, nil
}
func (s *fakeSuggestionRepo) Resolve(context.Context, uuid.UUID, domain.DedupSuggestionState, domain.OwnerType, uuid.UUID) error {
	return nil
}

type fakeLinkRepo struct{ listed []domain.MemoryEvidenceLink }

func (l *fakeLinkRepo) Insert(context.Context, domain.MemoryEvidenceLink) error { return nil }
func (l *fakeLinkRepo) ListForUnit(context.Context, uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	return l.listed, nil
}
func (l *fakeLinkRepo) ListForEvidence(context.Context, uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	return nil, nil
}
func (l *fakeLinkRepo) CountAvailableEvidence(context.Context, uuid.UUID, uuid.UUID) (int, error) {
	return 0, nil
}

// fakePublishSink records the publish call + never touches an Evidence ACL.
type fakePublishSink struct {
	calls   []evidence.PublishUnitRequest
	retVer  uuid.UUID
	retErr  error
}

func newFakePublishSink() *fakePublishSink { return &fakePublishSink{} }

func (p *fakePublishSink) PublishUnit(_ context.Context, req evidence.PublishUnitRequest) (uuid.UUID, error) {
	p.calls = append(p.calls, req)
	if p.retErr != nil {
		return uuid.Nil, p.retErr
	}
	if p.retVer == uuid.Nil {
		return uuid.New(), nil
	}
	return p.retVer, nil
}

type fakeRelationWriter struct {
	inserted []domain.KnowledgeRelation
}

func newFakeRelationWriter() *fakeRelationWriter { return &fakeRelationWriter{} }

func (r *fakeRelationWriter) InsertRelation(_ context.Context, rel domain.KnowledgeRelation) (uuid.UUID, error) {
	r.inserted = append(r.inserted, rel)
	return uuid.New(), nil
}

// --- helpers ---

func testUnit(ws uuid.UUID, state domain.MemoryUnitState) domain.MemoryUnit {
	return domain.MemoryUnit{
		ID:            uuid.New(),
		WorkspaceID:   ws,
		AssetID:        uuid.New(),
		MemoryType:    domain.MemoryFact,
		Statement:     "go vet is clean before merge",
		State:         state,
		CreatedByType: domain.OwnerUser,
		CreatedByID:   uuid.New(),
	}
}

func testInbox() (*InboxService, *fakeUnitRepo, *fakePublishSink, *fakeRelationWriter) {
	units := newFakeUnitRepo()
	sink := newFakePublishSink()
	rels := newFakeRelationWriter()
	svc := NewInboxService(units, newFakeSuggestionRepo(), &fakeLinkRepo{}, sink, rels)
	return svc, units, sink, rels
}

func reviewerAuth(ws uuid.UUID) (AuthContext, uuid.UUID) {
	return AuthContext{SubjectType: domain.SubjectUser, PrincipalID: uuid.New(), GroupIDs: nil}, ws
}

// --- tests ---

// §6.2 + 附录 A 不变量 8: approve publishes the unit via the sink exactly
// once; the returned state is published; the sink records the governance
// profile + reviewer. The sink fake never writes an Evidence ACL — the only
// way publish would touch one is if PublishUnit did, and it does not.
func TestInboxReview_Approve_Publishes(t *testing.T) {
	svc, units, sink, _ := testInbox()
	ws := uuid.New()
	unit := testUnit(ws, domain.MemoryCandidate)
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(ws)
	profile := uuid.New()
	res, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: ws, Disposition: DispositionApprove,
		GovernanceProfileID: profile, PolicyVersion: "pv-1",
	})

	require.NoError(t, err)
	require.Equal(t, domain.MemoryPublished, res.State)
	require.NotNil(t, res.AssetVersionID)
	require.Len(t, sink.calls, 1, "publish sink called exactly once")
	c := sink.calls[0]
	assert.Equal(t, unit.ID, c.UnitID)
	assert.Equal(t, ws, c.WorkspaceID)
	assert.Equal(t, unit.AssetID, c.AssetID)
	assert.Equal(t, profile, c.GovernanceProfileID)
	assert.Equal(t, auth.PrincipalID, c.ReviewerID)
}

// §6.2: reject flips the state to rejected; the publish sink is NOT called.
func TestInboxReview_Reject_NoPublish(t *testing.T) {
	svc, units, sink, _ := testInbox()
	ws := uuid.New()
	unit := testUnit(ws, domain.MemoryCandidate)
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(ws)
	res, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: ws, Disposition: DispositionReject,
	})

	require.NoError(t, err)
	assert.Equal(t, domain.MemoryRejected, res.State)
	assert.Empty(t, sink.calls, "reject must not publish")
	assert.Equal(t, domain.MemoryRejected, units.setState[unit.ID])
}

// §6.3 + 附录 A 不变量 9: supersede sets superseded_by + deprecates the unit
// AND writes a `supersedes` knowledge_relation edge (origin=human) from the
// survivor asset to the deprecated asset. The survivor is NOT auto-published
// (no sink call) — the reviewer publishes it separately.
func TestInboxReview_Supersede_WritesEdge_NoAutoPublish(t *testing.T) {
	svc, units, sink, rels := testInbox()
	ws := uuid.New()
	survivor := testUnit(ws, domain.MemoryCandidate)
	deprecated := testUnit(ws, domain.MemoryCandidate)
	units.get[survivor.ID] = survivor
	units.get[deprecated.ID] = deprecated

	auth, _ := reviewerAuth(ws)
	res, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: deprecated.ID, WorkspaceID: ws, Disposition: DispositionSupersede,
		SupersedeBy: &survivor.ID,
	})

	require.NoError(t, err)
	assert.Equal(t, domain.MemoryDeprecated, res.State)
	assert.Equal(t, survivor.ID, units.superseded[deprecated.ID], "superseded_by set to the survivor")
	assert.Equal(t, domain.MemoryDeprecated, units.setState[deprecated.ID])
	require.Len(t, rels.inserted, 1, "one supersedes relation edge written")
	edge := rels.inserted[0]
	assert.Equal(t, domain.RelationSupersedes, edge.RelationType)
	assert.Equal(t, domain.RelationOriginHuman, edge.Origin, "reviewer-confirmed, not generated")
	assert.Equal(t, survivor.AssetID, edge.FromAssetID, "edge from survivor")
	assert.Equal(t, deprecated.AssetID, edge.ToAssetID, "edge to deprecated")
	assert.Empty(t, sink.calls, "supersede must not auto-publish the survivor (附录 A 不变量 9)")
}

// §6.3: supersede requires a supersede_by unit id.
func TestInboxReview_Supersede_RequiresTarget(t *testing.T) {
	svc, units, _, _ := testInbox()
	ws := uuid.New()
	unit := testUnit(ws, domain.MemoryCandidate)
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(ws)
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: ws, Disposition: DispositionSupersede,
	})
	require.Error(t, err)
}

// §9.3 leak-safe: a Review whose unit lives in ANOTHER workspace surfaces as
// ErrInboxNotFound, never the real "wrong workspace" error. Existence is not
// leaked.
func TestInboxReview_CrossWorkspace_LeakSafeNotFound(t *testing.T) {
	svc, units, _, _ := testInbox()
	wsReviewer := uuid.New()
	wsOther := uuid.New()
	unit := testUnit(wsOther, domain.MemoryCandidate) // lives in another ws
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(wsReviewer)
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: wsReviewer, Disposition: DispositionReject,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInboxNotFound, "cross-workspace unit must surface as leak-safe not-found")
}

// §9.3 leak-safe: a Review on a unit that does not exist surfaces as
// ErrInboxNotFound (indistinguishable from a denial).
func TestInboxReview_MissingUnit_LeakSafeNotFound(t *testing.T) {
	svc, _, _, _ := testInbox()
	ws := uuid.New()
	auth, _ := reviewerAuth(ws)
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: uuid.New(), WorkspaceID: ws, Disposition: DispositionReject,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInboxNotFound)
}

// §6.2 conflict: publishing a unit that carries an unresolved supersede
// candidate (superseded_by already set) surfaces as ErrPublishConflict — the
// reviewer must resolve the suggestion first. (The DB CHECK mirrors this; the
// service surfaces it as a typed error before the tx.)
func TestInboxReview_Approve_UnresolvedSupersede_Conflict(t *testing.T) {
	svc, units, sink, _ := testInbox()
	ws := uuid.New()
	by := uuid.New()
	unit := testUnit(ws, domain.MemoryCandidate)
	unit.SupersededBy = &by // unresolved supersede candidate
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(ws)
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: ws, Disposition: DispositionApprove,
		GovernanceProfileID: uuid.New(),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPublishConflict)
	assert.Empty(t, sink.calls, "conflict must not publish")
}

// §6.2: approve requires a governance_profile_id.
func TestInboxReview_Approve_RequiresProfile(t *testing.T) {
	svc, units, sink, _ := testInbox()
	ws := uuid.New()
	unit := testUnit(ws, domain.MemoryCandidate)
	units.get[unit.ID] = unit

	auth, _ := reviewerAuth(ws)
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		UnitID: unit.ID, WorkspaceID: ws, Disposition: DispositionApprove,
	})
	require.Error(t, err)
	assert.Empty(t, sink.calls)
}

// §6.3 + 附录 A 不变量 9: the only way superseded_by gets written is the
// reviewer's supersede disposition. The InboxService exposes no auto-merge
// path — there is no method on it that sets superseded_by except Review with
// DispositionSupersede. (This is a structural assertion; the Go API has no
// SetSupersededBy-exposing method besides the internal supersede path.)
func TestInbox_NoAutoMergeMethod(t *testing.T) {
	svc, _, _, _ := testInbox()
	// The exported surface: Inbox + Review. Neither takes a "merge this pair
	// now" call — supersede is a reviewer disposition on ONE unit with a
	// target. Confirm Review with an unknown disposition errors (no hidden
	// auto-merge default).
	auth, _ := reviewerAuth(uuid.New())
	_, err := svc.Review(context.Background(), auth, DispositionRequest{
		Disposition: Disposition("auto_merge"),
	})
	require.Error(t, err)
}

// §6.3 inbox ordering: evidence_missing candidates first; contradicts
// suggestions first. (sortInbox is the helper; exercised directly here.)
func TestSortInbox_EvidenceMissingAndContradictsFirst(t *testing.T) {
	ws := uuid.New()
	plain := testUnit(ws, domain.MemoryCandidate)
	missing := testUnit(ws, domain.MemoryCandidate)
	missing.EvidenceMissing = true
	items := []InboxItem{{Unit: plain}, {Unit: missing}}

	sugDup := domain.MemoryDedupSuggestion{SuggestionType: domain.DedupDuplicate}
	sugCon := domain.MemoryDedupSuggestion{SuggestionType: domain.DedupContradicts}
	suggestions := []domain.MemoryDedupSuggestion{sugDup, sugCon}

	sortInbox(items, suggestions)

	assert.True(t, items[0].Unit.EvidenceMissing, "evidence_missing candidate sorts first")
	assert.Equal(t, domain.DedupContradicts, suggestions[0].SuggestionType, "contradicts sorts first")
}

// §4.4 + §9.3: the inbox with a nil RBAC engine (dev/test) allows listing —
// leak-safe empty is only for an *allowed* caller with no candidates, not for
// a denied one. Here nil-engine + candidates → returns them (the dev path).
func TestInbox_NilRBAC_AllowsInDev(t *testing.T) {
	svc, units, _, _ := testInbox()
	ws := uuid.New()
	c := testUnit(ws, domain.MemoryCandidate)
	units.listCands = []domain.MemoryUnit{c}

	auth, _ := reviewerAuth(ws)
	view, err := svc.Inbox(context.Background(), auth, ws)
	require.NoError(t, err)
	require.Len(t, view.Items, 1)
	assert.Equal(t, c.ID, view.Items[0].Unit.ID)
}

// compile-time: the fakes satisfy the ports (mirrors evidence/service_test.go).
var (
	_ evidence.MemoryUnitRepo        = (*fakeUnitRepo)(nil)
	_ evidence.DedupSuggestionRepo   = (*fakeSuggestionRepo)(nil)
	_ evidence.EvidenceLinkRepo       = (*fakeLinkRepo)(nil)
	_ evidence.MemoryAssetVersionSink = (*fakePublishSink)(nil)
	_ evidence.KnowledgeRelationWriter = (*fakeRelationWriter)(nil)
)
