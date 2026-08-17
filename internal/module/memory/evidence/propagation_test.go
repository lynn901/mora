package evidence

// Unit tests for the deletion-propagation service (design-docs/18 §9.2, D3).
//
// These cover the §9.2 cascade with in-memory fakes (no DATABASE_URL needed):
//   - PurgeEvidence: active → purged, content erased, MinIO object deleted,
//     units flagged evidence_missing only when this was their last independent
//     support, projections invalidated, audit row written.
//   - idempotent re-purge (content already null → cascade + audit re-run, no
//     error; MinIO missing object is not an error).
//   - revoke source_asset → evidence_missing, no content erase, no audit.
//   - MarkPendingPurge passthrough.
// The integration tests (real SQL, the §9.2 chain end-to-end) are the test
// engineer's gate (§11); these unit tests pin the orchestration logic the
// integration tests can't cheaply exercise per-branch.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
)

// --- fakes ---

type fakeEvidenceRepo struct {
	get      map[uuid.UUID]domain.MemoryEvidence
	purged   map[uuid.UUID]bool
	pending  map[uuid.UUID]bool
	byAsset  map[uuid.UUID][]domain.MemoryEvidence
	getErr   error
	purgeErr error
	delErr   error
}

func newFakeEvidenceRepo() *fakeEvidenceRepo {
	return &fakeEvidenceRepo{
		get: map[uuid.UUID]domain.MemoryEvidence{}, purged: map[uuid.UUID]bool{}, pending: map[uuid.UUID]bool{}, byAsset: map[uuid.UUID][]domain.MemoryEvidence{},
	}
}

func (r *fakeEvidenceRepo) Insert(ctx context.Context, e domain.MemoryEvidence) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (r *fakeEvidenceRepo) Get(_ context.Context, id uuid.UUID) (domain.MemoryEvidence, error) {
	if r.getErr != nil {
		return domain.MemoryEvidence{}, r.getErr
	}
	e, ok := r.get[id]
	if !ok {
		return domain.MemoryEvidence{}, domain.ErrEvidenceNotFound
	}
	return e, nil
}
func (r *fakeEvidenceRepo) ListByOwner(context.Context, uuid.UUID, domain.OwnerType, uuid.UUID) ([]domain.MemoryEvidence, error) {
	return nil, nil
}
func (r *fakeEvidenceRepo) ListBySourceAsset(_ context.Context, assetID uuid.UUID) ([]domain.MemoryEvidence, error) {
	return r.byAsset[assetID], nil
}
func (r *fakeEvidenceRepo) MarkPendingPurge(_ context.Context, id uuid.UUID) error {
	if _, ok := r.get[id]; !ok {
		return nil
	}
	r.pending[id] = true
	return nil
}
func (r *fakeEvidenceRepo) Purge(_ context.Context, id uuid.UUID) error {
	if r.purgeErr != nil {
		return r.purgeErr
	}
	if _, ok := r.get[id]; !ok {
		return domain.ErrEvidenceNotFound
	}
	r.purged[id] = true
	// simulate content null on the stored row
	e := r.get[id]
	e.EncryptedContent = nil
	e.StorageKey = ""
	e.KeyVersion = nil
	r.get[id] = e
	return nil
}
func (r *fakeEvidenceRepo) ClearContent(context.Context, uuid.UUID) error { return nil }

type fakeUnitRepo struct {
	flagged map[uuid.UUID]bool
}

func newFakeUnitRepo() *fakeUnitRepo { return &fakeUnitRepo{flagged: map[uuid.UUID]bool{}} }

func (u *fakeUnitRepo) Insert(context.Context, domain.MemoryUnit) (uuid.UUID, error) {
	return uuid.Nil, nil
}
func (u *fakeUnitRepo) Get(context.Context, uuid.UUID) (domain.MemoryUnit, error) {
	return domain.MemoryUnit{}, domain.ErrMemoryUnitNotFound
}
func (u *fakeUnitRepo) ListByAsset(context.Context, uuid.UUID) ([]domain.MemoryUnit, error) {
	return nil, nil
}
func (u *fakeUnitRepo) ListCandidates(context.Context, uuid.UUID) ([]domain.MemoryUnit, error) {
	return nil, nil
}
func (u *fakeUnitRepo) SetState(context.Context, uuid.UUID, domain.MemoryUnitState) error {
	return nil
}
func (u *fakeUnitRepo) SetSupersededBy(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (u *fakeUnitRepo) MarkEvidenceMissing(_ context.Context, id uuid.UUID) error {
	u.flagged[id] = true
	return nil
}

type fakeLinkRepo struct {
	// per-evidence list of (unitID) links
	linksForEv map[uuid.UUID][]uuid.UUID
	// CountAvailableEvidence result table keyed by unitID (excluding the purged
	// evidence); lets a test script the "how much is left" answer per unit.
	available map[uuid.UUID]int
	linksErr  error
	countErr  error
}

func newFakeLinkRepo() *fakeLinkRepo {
	return &fakeLinkRepo{linksForEv: map[uuid.UUID][]uuid.UUID{}, available: map[uuid.UUID]int{}}
}

func (l *fakeLinkRepo) Insert(context.Context, domain.MemoryEvidenceLink) error { return nil }
func (l *fakeLinkRepo) ListForUnit(context.Context, uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	return nil, nil
}
func (l *fakeLinkRepo) ListForEvidence(_ context.Context, evID uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	if l.linksErr != nil {
		return nil, l.linksErr
	}
	out := make([]domain.MemoryEvidenceLink, 0, len(l.linksForEv[evID]))
	for _, uid := range l.linksForEv[evID] {
		out = append(out, domain.MemoryEvidenceLink{MemoryUnitID: uid, EvidenceID: evID})
	}
	return out, nil
}
func (l *fakeLinkRepo) CountAvailableEvidence(_ context.Context, unitID, _ uuid.UUID) (int, error) {
	if l.countErr != nil {
		return 0, l.countErr
	}
	return l.available[unitID], nil
}

type fakeObjectStore struct {
	deleted []string
	delErr  error
}

func (s *fakeObjectStore) Put(context.Context, uuid.UUID, uuid.UUID, []byte) (string, error) {
	return "", nil
}
func (s *fakeObjectStore) Read(context.Context, string) ([]byte, error) { return nil, nil }
func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	if s.delErr != nil {
		return s.delErr
	}
	s.deleted = append(s.deleted, key)
	return nil
}

type fakeInvalidator struct {
	called []uuid.UUID
	err    error
}

func (i *fakeInvalidator) InvalidateUnitProjections(_ context.Context, unitID uuid.UUID) error {
	if i.err != nil {
		return i.err
	}
	i.called = append(i.called, unitID)
	return nil
}

type fakeAudit struct {
	purged []domain.MemoryEvidence
	at     []time.Time
	err    error
}

func (a *fakeAudit) RecordEvidencePurged(_ context.Context, e domain.MemoryEvidence, t time.Time) error {
	if a.err != nil {
		return a.err
	}
	a.purged = append(a.purged, e)
	a.at = append(a.at, t)
	return nil
}

// --- helpers ---

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func inlineEvidence(id uuid.UUID, hash string) domain.MemoryEvidence {
	kv := 1
	return domain.MemoryEvidence{
		ID: id, WorkspaceID: uuid.New(), ContentHash: hash,
		EncryptedContent: []byte("cipher"), KeyVersion: &kv,
		RedactedExcerpt: "excerpt", State: domain.EvidenceActive,
	}
}

func largeObjectEvidence(id uuid.UUID, hash, key string) domain.MemoryEvidence {
	return domain.MemoryEvidence{
		ID: id, WorkspaceID: uuid.New(), ContentHash: hash,
		StorageKey: key, RedactedExcerpt: "excerpt", State: domain.EvidenceActive,
	}
}

// --- tests ---

// TestPurgeEvidence_InlineErasesContentAndAudits: a small inline evidence with
// no linked units → content erased (repo.Purge), no MinIO delete, audit row
// written with content_hash, no units flagged.
func TestPurgeEvidence_InlineErasesContentAndAudits(t *testing.T) {
	ev := inlineEvidence(uuid.New(), "hash-inline")
	er := newFakeEvidenceRepo()
	er.get[ev.ID] = ev
	audit := &fakeAudit{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: &fakeObjectStore{}, Projections: &fakeInvalidator{},
		Audit: audit, Now: fixedClock(time.Unix(1000, 0)),
	})

	out, err := svc.PurgeEvidence(context.Background(), ev.ID)
	require.NoError(t, err)
	assert.True(t, er.purged[ev.ID], "repo.Purge called")
	assert.False(t, out.WasLargeObject)
	assert.Empty(t, out.UnitsFlagged)
	require.Len(t, audit.purged, 1)
	assert.Equal(t, "hash-inline", audit.purged[0].ContentHash)
	assert.Equal(t, time.Unix(1000, 0), audit.at[0])
}

// TestPurgeEvidence_LargeObjectDeletesMinIO: a StorageKey evidence → MinIO
// Delete called with the key, content erased.
func TestPurgeEvidence_LargeObjectDeletesMinIO(t *testing.T) {
	ev := largeObjectEvidence(uuid.New(), "hash-lo", "mora-evidence/ws/id")
	er := newFakeEvidenceRepo()
	er.get[ev.ID] = ev
	objs := &fakeObjectStore{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: objs, Projections: &fakeInvalidator{}, Audit: &fakeAudit{},
		Now: fixedClock(time.Now()),
	})
	out, err := svc.PurgeEvidence(context.Background(), ev.ID)
	require.NoError(t, err)
	assert.True(t, out.WasLargeObject)
	assert.Equal(t, []string{"mora-evidence/ws/id"}, objs.deleted)
}

// TestPurgeEvidence_LastSupportFlagsUnit: the evidence was a unit's only
// support (available=0) → unit flagged evidence_missing; invalidator called.
func TestPurgeEvidence_LastSupportFlagsUnit(t *testing.T) {
	evID := uuid.New()
	unitID := uuid.New()
	ev := inlineEvidence(evID, "h")
	er := newFakeEvidenceRepo()
	er.get[evID] = ev
	links := newFakeLinkRepo()
	links.linksForEv[evID] = []uuid.UUID{unitID}
	links.available[unitID] = 0 // this evidence was the only support
	units := newFakeUnitRepo()
	inv := &fakeInvalidator{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: units, Links: links, Objects: &fakeObjectStore{},
		Projections: inv, Audit: &fakeAudit{}, Now: fixedClock(time.Now()),
	})
	out, err := svc.PurgeEvidence(context.Background(), evID)
	require.NoError(t, err)
	require.Len(t, out.UnitsFlagged, 1)
	assert.Equal(t, unitID, out.UnitsFlagged[0])
	assert.True(t, units.flagged[unitID])
	assert.Equal(t, []uuid.UUID{unitID}, inv.called)
}

// TestPurgeEvidence_OtherSupportDoesNotFlagUnit: the unit has another
// independent evidence (available=1) → unit NOT flagged, but its projections
// are still invalidated (the purged evidence's contribution is gone).
func TestPurgeEvidence_OtherSupportDoesNotFlagUnit(t *testing.T) {
	evID := uuid.New()
	unitID := uuid.New()
	ev := inlineEvidence(evID, "h")
	er := newFakeEvidenceRepo()
	er.get[evID] = ev
	links := newFakeLinkRepo()
	links.linksForEv[evID] = []uuid.UUID{unitID}
	links.available[unitID] = 1 // another evidence still supports it
	units := newFakeUnitRepo()
	inv := &fakeInvalidator{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: units, Links: links, Objects: &fakeObjectStore{},
		Projections: inv, Audit: &fakeAudit{}, Now: fixedClock(time.Now()),
	})
	out, err := svc.PurgeEvidence(context.Background(), evID)
	require.NoError(t, err)
	assert.Empty(t, out.UnitsFlagged, "unit still has support → not flagged")
	assert.False(t, units.flagged[unitID])
	assert.Equal(t, []uuid.UUID{unitID}, inv.called, "projections still invalidated")
}

// TestPurgeEvidence_IdempotentRePurge: purging an already-purged row (content
// now null, StorageKey empty) re-runs cascade + audit without error. This is
// the reaper-retry path: a crashed purge after the DB erase re-runs and must
// not error on the now-empty content.
func TestPurgeEvidence_IdempotentRePurge(t *testing.T) {
	evID := uuid.New()
	// Simulate a post-first-purge row: content already nulled, StorageKey empty.
	ev := inlineEvidence(evID, "h")
	ev.EncryptedContent = nil
	ev.StorageKey = ""
	ev.KeyVersion = nil
	ev.State = domain.EvidencePurged
	er := newFakeEvidenceRepo()
	er.get[evID] = ev
	objs := &fakeObjectStore{}
	audit := &fakeAudit{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: objs, Projections: &fakeInvalidator{}, Audit: audit,
		Now: fixedClock(time.Now()),
	})
	out, err := svc.PurgeEvidence(context.Background(), evID)
	require.NoError(t, err)
	assert.False(t, out.WasLargeObject, "no object delete on re-purge")
	assert.Empty(t, objs.deleted)
	require.Len(t, audit.purged, 1, "audit still written on re-purge")
}

// TestPurgeEvidence_MissingReturnsNotFound: an unknown id surfaces
// ErrEvidenceNotFound (leak-safe §9.3), no cascade runs.
func TestPurgeEvidence_MissingReturnsNotFound(t *testing.T) {
	er := newFakeEvidenceRepo()
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: &fakeObjectStore{}, Projections: &fakeInvalidator{}, Audit: &fakeAudit{},
		Now: fixedClock(time.Now()),
	})
	_, err := svc.PurgeEvidence(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrEvidenceNotFound)
}

// TestPurgeEvidence_ObjectDeleteErrorSurfaces: a MinIO delete failure after the
// DB erase is surfaced so the reaper retries the object delete before trusting
// the purge complete (§9.2 逐级传播).
func TestPurgeEvidence_ObjectDeleteErrorSurfaces(t *testing.T) {
	ev := largeObjectEvidence(uuid.New(), "h", "mora-evidence/ws/id")
	er := newFakeEvidenceRepo()
	er.get[ev.ID] = ev
	objs := &fakeObjectStore{delErr: errors.New("minio down")}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: objs, Projections: &fakeInvalidator{}, Audit: &fakeAudit{},
		Now: fixedClock(time.Now()),
	})
	_, err := svc.PurgeEvidence(context.Background(), ev.ID)
	require.Error(t, err)
	assert.True(t, er.purged[ev.ID], "DB erase committed before the object error")
}

// TestRevokeSourceAsset_FlagsEvidenceMissing: source_asset delete → linked
// units flagged, no content erase (no MinIO delete), no evidence.purged audit.
func TestRevokeSourceAsset_FlagsEvidenceMissing(t *testing.T) {
	assetID := uuid.New()
	evID := uuid.New()
	unitID := uuid.New()
	er := newFakeEvidenceRepo()
	er.byAsset[assetID] = []domain.MemoryEvidence{inlineEvidence(evID, "h")}
	links := newFakeLinkRepo()
	links.linksForEv[evID] = []uuid.UUID{unitID}
	links.available[unitID] = 0
	units := newFakeUnitRepo()
	objs := &fakeObjectStore{}
	inv := &fakeInvalidator{}
	audit := &fakeAudit{}
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: units, Links: links, Objects: objs,
		Projections: inv, Audit: audit, Now: fixedClock(time.Now()),
	})
	out, err := svc.RevokeSourceAsset(context.Background(), assetID)
	require.NoError(t, err)
	assert.True(t, units.flagged[unitID])
	assert.Equal(t, []uuid.UUID{unitID}, inv.called)
	assert.Empty(t, objs.deleted, "revoke does not erase content")
	assert.Empty(t, audit.purged, "revoke does not emit evidence.purged")
	assert.False(t, er.purged[evID], "revoke does not flip state to purged")
	require.Len(t, out.UnitsFlagged, 1)
}

// TestMarkPendingPurge_Passthrough: the service forwards to the repo (first
// lifecycle half). The reaper's Tick calls this on due rows.
func TestMarkPendingPurge_Passthrough(t *testing.T) {
	evID := uuid.New()
	er := newFakeEvidenceRepo()
	er.get[evID] = inlineEvidence(evID, "h")
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: &fakeObjectStore{}, Projections: &fakeInvalidator{}, Audit: &fakeAudit{},
		Now: fixedClock(time.Now()),
	})
	require.NoError(t, svc.MarkPendingPurge(context.Background(), evID))
	assert.True(t, er.pending[evID])
}

// TestPropagationService_DefaultsNilPorts: nil Projections → NoopInvalidator
// (no panic); nil Audit → no audit call. Lets the distill/publish adapters
// land later without the propagation path crashing pre-wiring.
func TestPropagationService_DefaultsNilPorts(t *testing.T) {
	ev := inlineEvidence(uuid.New(), "h")
	er := newFakeEvidenceRepo()
	er.get[ev.ID] = ev
	svc := NewPropagationService(PropagationConfig{
		Evidence: er, Units: newFakeUnitRepo(), Links: newFakeLinkRepo(),
		Objects: &fakeObjectStore{}, // Projections + Audit nil
		Now:     fixedClock(time.Now()),
	})
	out, err := svc.PurgeEvidence(context.Background(), ev.ID)
	require.NoError(t, err)
	assert.True(t, er.purged[ev.ID])
	assert.Empty(t, out.UnitsFlagged)
}
