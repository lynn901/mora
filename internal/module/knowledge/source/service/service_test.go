package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ---

type fakeSourceRepo struct {
	created *domain.KnowledgeSource
	get     *domain.KnowledgeSource
	getErr  error
	patch   SourcePatch
	patched *domain.KnowledgeSource
	patchErr error
	disabled uuid.UUID
	credRef  string
	credErr  error
}

func (f *fakeSourceRepo) Create(_ context.Context, s *domain.KnowledgeSource) error {
	f.created = s
	return nil
}
func (f *fakeSourceRepo) Get(_ context.Context, _ uuid.UUID) (*domain.KnowledgeSource, error) {
	return f.get, f.getErr
}
func (f *fakeSourceRepo) GetWorkspace(context.Context, uuid.UUID) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}
func (f *fakeSourceRepo) List(context.Context, SourceListQuery) ([]*domain.KnowledgeSource, string, error) {
	return nil, "", nil
}
func (f *fakeSourceRepo) Update(_ context.Context, _ uuid.UUID, _ int64, p SourcePatch) (*domain.KnowledgeSource, error) {
	f.patch = p
	return f.patched, f.patchErr
}
func (f *fakeSourceRepo) Disable(_ context.Context, id uuid.UUID) error {
	f.disabled = id
	return nil
}
func (f *fakeSourceRepo) SetCredential(_ context.Context, _ uuid.UUID, ref, _ string) error {
	f.credRef = ref
	return f.credErr
}

type fakeRunRepo struct {
	byKey *domain.SourceSyncRun
	byKeyErr error
}

func (f *fakeRunRepo) Create(context.Context, *domain.SourceSyncRun) error { return nil }
func (f *fakeRunRepo) Get(context.Context, uuid.UUID) (*domain.SourceSyncRun, error) {
	return nil, nil
}
func (f *fakeRunRepo) GetByIdempotencyKey(_ context.Context, _ string) (*domain.SourceSyncRun, error) {
	return f.byKey, f.byKeyErr
}
func (f *fakeRunRepo) List(context.Context, SyncRunListQuery) ([]*domain.SourceSyncRun, string, error) {
	return nil, "", nil
}
func (f *fakeRunRepo) UpdateStatus(context.Context, uuid.UUID, domain.SyncRunStatus, string, string, string) error {
	return nil
}

type fakeReviewRepo struct{}

func (fakeReviewRepo) CreateRequest(context.Context, *domain.ReviewRequest) error { return nil }
func (fakeReviewRepo) GetRequest(context.Context, uuid.UUID) (*domain.ReviewRequest, error) {
	return nil, nil
}
func (fakeReviewRepo) ListPending(context.Context, uuid.UUID, string, int) ([]*domain.ReviewRequest, string, error) {
	return nil, "", nil
}
func (fakeReviewRepo) AppendDecision(context.Context, *domain.ReviewDecisionRecord) error { return nil }
func (fakeReviewRepo) GetWorkspace(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.Nil, nil
}

// fakeRunSink records the CreateRun call + lets a test force the return error.
type fakeRunSink struct {
	run  *domain.SourceSyncRun
	ev   domain.KnowledgeEvent
	err  error
}

func (f *fakeRunSink) CreateRun(_ context.Context, run *domain.SourceSyncRun, ev domain.KnowledgeEvent) error {
	f.run, f.ev = run, ev
	return f.err
}

// --- tests ---

// TestCreateSource_StripsNoCredentials asserts Create stores what the caller
// passed for URINormalized (the handler strips credentials before calling);
// the service does not re-parse the URI. It defaults trust to untrusted and
// enabled to true, and seeds an empty sync_policy when none is provided.
func TestCreateSource_StripsNoCredentials(t *testing.T) {
	srcs := &fakeSourceRepo{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, &fakeRunSink{}, nil)
	ws := uuid.New()
	creator := uuid.New()
	src, err := svc.CreateSource(context.Background(), CreateSourceInput{
		WorkspaceID:   ws,
		SourceType:    domain.SourceGit,
		Name:          "repo",
		URINormalized:  "https://github.com/acme/repo.git",
		CreatedByType: domain.SubjectUser,
		CreatedByID:   creator,
	})
	require.NoError(t, err)
	require.NotNil(t, src)
	assert.Equal(t, domain.SourceGit, srcs.created.SourceType)
	assert.Equal(t, "https://github.com/acme/repo.git", srcs.created.URINormalized)
	assert.True(t, srcs.created.Enabled)
	assert.Equal(t, domain.TrustUntrusted, srcs.created.TrustLevel)
	assert.Equal(t, map[string]any{}, srcs.created.SyncPolicy)
	assert.Equal(t, ws, src.WorkspaceID)
	assert.Equal(t, creator, src.CreatedByID)
}

// TestSetCredential_NoStoreStoresRef verifies that when no CredentialStore is
// wired (the Phase 1 file path), the caller-supplied ref is pinned directly.
// Plaintext is never consumed because there is no store to receive it.
func TestSetCredential_NoStoreStoresRef(t *testing.T) {
	srcs := &fakeSourceRepo{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, &fakeRunSink{}, nil)
	err := svc.SetCredential(context.Background(), uuid.New(), uuid.New(), []byte("shh"), "secret:v1")
	require.NoError(t, err)
	assert.Equal(t, "secret:v1", srcs.credRef)
}

// TestTriggerSync_IdempotentRetryReturnsOriginal verifies the §4.4 idempotent-
// retry contract: when the sink signals ErrIdempotentRetry (same payload,
// duplicate Idempotency-Key), the service re-GETs the original run by key and
// returns it — instead of erroring or creating a second run.
func TestTriggerSync_IdempotentRetryReturnsOriginal(t *testing.T) {
	original := &domain.SourceSyncRun{ID: uuid.New(), IdempotencyKey: "key-1", Status: domain.SyncRunQueued}
	srcs := &fakeSourceRepo{
		get: &domain.KnowledgeSource{
			ID: uuid.New(), WorkspaceID: uuid.New(), Enabled: true,
			SourceType: domain.SourceGit, TrustLevel: domain.TrustInternal,
		},
	}
	runs := &fakeRunRepo{byKey: original}
	sink := &fakeRunSink{err: ErrIdempotentRetry}
	svc := NewService(srcs, runs, fakeReviewRepo{}, sink, nil)

	out, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           srcs.get.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "key-1",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, original.ID, out.ID, "idempotent retry returns the original run id")
	assert.Equal(t, "key-1", out.IdempotencyKey)
	// The sink saw the attempted run, but the original is what the caller got.
	require.NotNil(t, sink.run)
	assert.Equal(t, "key-1", sink.run.IdempotencyKey)
}

// TestTriggerSync_IdempotencyConflictPropagates verifies a DIFFERENT payload
// under a reused Idempotency-Key surfaces as ErrIdempotencyConflict (→ 409).
func TestTriggerSync_IdempotencyConflictPropagates(t *testing.T) {
	srcs := &fakeSourceRepo{
		get: &domain.KnowledgeSource{
			ID: uuid.New(), WorkspaceID: uuid.New(), Enabled: true,
			SourceType: domain.SourceGit,
		},
	}
	sink := &fakeRunSink{err: ErrIdempotencyConflict}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, sink, nil)
	_, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           srcs.get.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "dup-key",
	})
	assert.ErrorIs(t, err, ErrIdempotencyConflict)
}

// TestTriggerSync_DisabledSourceIsNotFound asserts a disabled source cannot
// be synced — the service surfaces ErrSourceNotFound (→ 404, no leak) rather
// than revealing the source exists but is disabled.
func TestTriggerSync_DisabledSourceIsNotFound(t *testing.T) {
	srcs := &fakeSourceRepo{
		get: &domain.KnowledgeSource{
			ID: uuid.New(), WorkspaceID: uuid.New(), Enabled: false,
			SourceType: domain.SourceGit,
		},
	}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, &fakeRunSink{}, nil)
	_, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           srcs.get.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		IdempotencyKey:    "k",
	})
	assert.ErrorIs(t, err, ErrSourceNotFound)
}

// TestTriggerSync_MissingSourceIsNotFound asserts a missing source surfaces
// as ErrSourceNotFound (no existence leak, §8.2).
func TestTriggerSync_MissingSourceIsNotFound(t *testing.T) {
	srcs := &fakeSourceRepo{getErr: ErrSourceNotFound}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, &fakeRunSink{}, nil)
	_, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           uuid.New(),
		RequestedAssetType: domain.RequestedAssetDocument,
		IdempotencyKey:    "k",
	})
	assert.ErrorIs(t, err, ErrSourceNotFound)
}

// TestTriggerSync_RetryRaceFallsBackToConflict covers the race where the
// original run vanishes between collision-detect and re-GET: the service
// must surface ErrIdempotencyConflict (the key is unusable), not a nil run.
func TestTriggerSync_RetryRaceFallsBackToConflict(t *testing.T) {
	srcs := &fakeSourceRepo{
		get: &domain.KnowledgeSource{
			ID: uuid.New(), WorkspaceID: uuid.New(), Enabled: true,
			SourceType: domain.SourceGit,
		},
	}
	runs := &fakeRunRepo{byKeyErr: ErrRunNotFound} // the original vanished
	sink := &fakeRunSink{err: ErrIdempotentRetry}
	svc := NewService(srcs, runs, fakeReviewRepo{}, sink, nil)
	_, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           srcs.get.ID,
		RequestedAssetType: domain.RequestedAssetDocument,
		RequestedByType:   domain.SubjectUser,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "raced",
	})
	assert.ErrorIs(t, err, ErrIdempotencyConflict)
}

// TestTriggerSync_OutboxEventCarriesRunID verifies the Knowledge Outbox event
// is the dispatch aggregate: its aggregate_id is the run id, its event_type is
// asset.version.requested, and its actor is the requesting principal. This is
// what the knowledge-worker consumes (§5).
func TestTriggerSync_OutboxEventCarriesRunID(t *testing.T) {
	src := &domain.KnowledgeSource{
		ID: uuid.New(), WorkspaceID: uuid.New(), Enabled: true,
		SourceType: domain.SourceGit, TrustLevel: domain.TrustInternal,
		URINormalized: "https://github.com/acme/repo.git",
	}
	srcs := &fakeSourceRepo{get: src}
	sink := &fakeRunSink{}
	svc := NewService(srcs, &fakeRunRepo{}, fakeReviewRepo{}, sink, nil)
	_, err := svc.TriggerSync(context.Background(), TriggerSyncInput{
		SourceID:           src.ID,
		RequestedAssetType: domain.RequestedAssetCodebase,
		RequestedByType:   domain.SubjectAgent,
		RequestedByID:     uuid.New(),
		IdempotencyKey:    "evt-key",
	})
	require.NoError(t, err)
	require.NotNil(t, sink.ev)
	assert.Equal(t, domain.KEAssetVersionRequested, sink.ev.EventType)
	assert.Equal(t, domain.AggKnowledgeAsset, sink.ev.AggregateType)
	assert.Equal(t, sink.run.ID, sink.ev.AggregateID, "aggregate_id = run id (dispatch aggregate)")
	assert.Equal(t, "evt-key", sink.ev.EventID, "event id = idempotency key")
	require.NotNil(t, sink.ev.WorkspaceID)
	assert.Equal(t, src.WorkspaceID, *sink.ev.WorkspaceID)
	assert.Equal(t, domain.SubjectAgent, sink.ev.Actor.Type)
	assert.Equal(t, src.ID.String(), sink.ev.Payload["source_id"])
	assert.Equal(t, "codebase", sink.ev.Payload["asset_type"])
}
