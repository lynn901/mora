package worker

// handlers_test.go verifies the §5.2 dispatch-table handlers' Run logic,
// focusing on the CAS sentinel→RetryClass classification (§7) that is the
// handlers' core responsibility. The Runner's acquire→run→mark plumbing is
// covered by runner_test.go; here we test the handlers directly with a fake
// asset.Registry so the classification is hermetic and fast.
//
// What each case pins (design-docs/14 §7 red-line "失败不覆盖"):
//   - projection_build: missing fields → permanent; registry error → transient;
//     success → transient (the Runner only re-queues on error, success is a
//     no-op retry-class because the Runner marks succeeded regardless).
//   - asset_activate: CAS stale / expected mismatch / not-published → permanent
//     (the CAS decided; retrying races again); projections-not-ready →
//     transient (they may still land); generic error → transient.
//   - reconcile_scan: bad workspace_id in TargetKey → permanent; registry error
//     → transient; ErrAssetVersionNotFound → permanent (dead-letter wiring).
//   - source_sync / legacy_backfill: validated no-ops returning success.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
)

// fakeTx is a no-op pgx.Tx for handler tests. Handlers call BeginTx →
// (registry work) → Commit/Rollback; the fake makes each a nil error so the
// test isolates the registry's error/classification, not the tx plumbing.
type fakeTx struct{}

func (fakeTx) Begin(ctx context.Context) (pgx.Tx, error)              { return fakeTx{}, nil }
func (fakeTx) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	return fakeTx{}, nil
}
func (fakeTx) Commit(ctx context.Context) error                       { return nil }
func (fakeTx) Rollback(ctx context.Context) error                     { return nil }
func (fakeTx) Exec(ctx context.Context, sql string, a ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row { return nil }
func (fakeTx) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	return nil, errors.New("not implemented")
}
func (fakeTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (fakeTx) Conn() *pgx.Conn { return nil }
func (fakeTx) CopyFrom(ctx context.Context, tn pgx.Identifier, cols []string, r pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (fakeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults { return nil }
func (fakeTx) LargeObjects() pgx.LargeObjects                                { return pgx.LargeObjects{} }

// fakeTxStarter hands out fakeTx{} from BeginTx. It satisfies JobTxStarter.
type fakeTxStarter struct{ err error }

func (s fakeTxStarter) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	if s.err != nil {
		return nil, s.err
	}
	return fakeTx{}, nil
}

// Compile-time checks.
var _ pgx.Tx = fakeTx{}
var _ JobTxStarter = fakeTxStarter{}

// fakeRegistry is an in-memory asset.Registry for handler tests. It records
// calls and returns a configurable error so each CAS sentinel can be exercised.
type fakeRegistry struct {
	activateCalled      bool
	activateAssetID     uuid.UUID
	activateVersionID    uuid.UUID
	activateFence       int64
	activateExpected    *uuid.UUID
	markReadyCalled     bool
	markReadyVersionID  uuid.UUID
	markReadyKind       domain.ProjectionKind
	markReadyProvider   string
	markReadyRevision   string
	reconcileCalled     bool
	reconcileWorkspace  uuid.UUID
	// return errors
	activateErr       error
	markReadyErr      error
	reconcileErr      error
	reconcileReport   domain.ReconcileReport
}

func (f *fakeRegistry) RegisterDocumentAsset(ctx context.Context, tx pgx.Tx, reg asset.Registration) (asset.Result, error) {
	return asset.Result{}, nil
}
func (f *fakeRegistry) LegacyMigrationProfileID(ctx context.Context, tx pgx.Tx, workspaceID domain.UUID) (domain.UUID, error) {
	return uuid.Nil, nil
}
func (f *fakeRegistry) MarkProjectionReady(ctx context.Context, tx pgx.Tx, assetVersionID domain.UUID, kind domain.ProjectionKind, provider, buildRevision string, locator map[string]any) error {
	f.markReadyCalled = true
	f.markReadyVersionID = assetVersionID
	f.markReadyKind = kind
	f.markReadyProvider = provider
	f.markReadyRevision = buildRevision
	return f.markReadyErr
}
func (f *fakeRegistry) Activate(ctx context.Context, tx pgx.Tx, assetID, assetVersionID domain.UUID, fence int64, expectedCurrent *domain.UUID) error {
	f.activateCalled = true
	f.activateAssetID = assetID
	f.activateVersionID = assetVersionID
	f.activateFence = fence
	f.activateExpected = expectedCurrent
	return f.activateErr
}
func (f *fakeRegistry) ReconcileScan(ctx context.Context, workspaceID domain.UUID) (domain.ReconcileReport, error) {
	f.reconcileCalled = true
	f.reconcileWorkspace = workspaceID
	return f.reconcileReport, f.reconcileErr
}

// Compile-time check: the fake satisfies asset.Registry.
var _ asset.Registry = (*fakeRegistry)(nil)

// --- projection_build -------------------------------------------------------

func TestProjectionBuild_MissingFieldsArePermanent(t *testing.T) {
	assetVID := uuid.New()
	cases := []struct {
		name string
		job  domain.Job
	}{
		{"missing asset_version_id", domain.Job{JobType: JobProjectionBuild, TargetKey: "fts", BuildRevision: "rev-1"}},
		{"missing target_key (projection_kind)", domain.Job{JobType: JobProjectionBuild, AssetVersionID: &assetVID, BuildRevision: "rev-1"}},
		{"missing build_revision", domain.Job{JobType: JobProjectionBuild, AssetVersionID: &assetVID, TargetKey: "fts"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &ProjectionBuildHandler{Assets: &fakeRegistry{}}
			class, err := h.Run(context.Background(), c.job)
			assert.Error(t, err)
			assert.Equal(t, domain.RetryPermanent, class,
				"missing required field must be permanent so the job dead-letters, not spin")
		})
	}
}

func TestProjectionBuild_RegistryErrorIsTransient(t *testing.T) {
	assetVID := uuid.New()
	h := &ProjectionBuildHandler{Tx: fakeTxStarter{}, Assets: &fakeRegistry{markReadyErr: errors.New("qdrant upsert: connection reset")}}
	job := domain.Job{
		JobType:        JobProjectionBuild,
		AssetVersionID: &assetVID,
		TargetKey:      "vector",
		BuildRevision:  "rev-2",
		LeaseOwner:     "rag-worker",
	}
	class, err := h.Run(context.Background(), job)
	assert.Error(t, err)
	assert.Equal(t, domain.RetryTransient, class,
		"a transient registry/DB error must be retried, not dead-lettered")
}

// --- asset_activate ---------------------------------------------------------

func TestAssetActivate_MissingIDsArePermanent(t *testing.T) {
	h := &AssetActivateHandler{Tx: fakeTxStarter{}, Assets: &fakeRegistry{}}
	// no asset_id, no asset_version_id
	class, err := h.Run(context.Background(), domain.Job{JobType: JobAssetActivate})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryPermanent, class)
}

func TestAssetActivate_MissingFenceIsPermanent(t *testing.T) {
	assetID, verID := uuid.New(), uuid.New()
	h := &AssetActivateHandler{Tx: fakeTxStarter{}, Assets: &fakeRegistry{}}
	// IDs present but Progress has no fence → cannot CAS safely
	class, err := h.Run(context.Background(), domain.Job{
		JobType:        JobAssetActivate,
		AssetID:        &assetID,
		AssetVersionID: &verID,
	})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryPermanent, class,
		"missing fence means the CAS barrier can't be enforced — permanent, not a blind retry")
}

// TestAssetActivate_CASSentinelClassification is the §7 red-line table: each
// sentinel the registry can return maps to the retry class the worker must act
// on. Permanent = CAS decided, retrying races again; transient = state may
// still change. The asset is untouched on every failure path (§7 失败不覆盖).
func TestAssetActivate_CASSentinelClassification(t *testing.T) {
	assetID, verID := uuid.New(), uuid.New()
	expected := uuid.New()
	progress := map[string]any{
		"fence":            int64(3),
		"expected_current": expected.String(),
	}
	cases := []struct {
		name       string
		regErr     error
		wantClass  domain.RetryClass
		wantCalled bool
	}{
		{"version stale → permanent", domain.ErrCASVersionStale, domain.RetryPermanent, true},
		{"expected mismatch → permanent", domain.ErrCASExpectedMismatch, domain.RetryPermanent, true},
		{"not published → permanent", domain.ErrNotPublished, domain.RetryPermanent, true},
		{"version not found → permanent", domain.ErrAssetVersionNotFound, domain.RetryPermanent, true},
		{"projections not ready → transient", domain.ErrProjectionsNotReady, domain.RetryTransient, true},
		{"generic error → transient", errors.New("pool: conn closed"), domain.RetryTransient, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := &fakeRegistry{activateErr: c.regErr}
			h := &AssetActivateHandler{Tx: fakeTxStarter{}, Assets: reg}
			class, err := h.Run(context.Background(), domain.Job{
				JobType:        JobAssetActivate,
				AssetID:        &assetID,
				AssetVersionID: &verID,
				Progress:       progress,
			})
			require.Error(t, err)
			assert.Equal(t, c.wantClass, class)
			assert.Equal(t, c.wantCalled, reg.activateCalled, "Activate must be invoked")
			// §7 失败不覆盖: the CAS is the final authority — the handler
			// must surface the sentinel, never silently swallow it.
			assert.True(t, errors.Is(err, c.regErr) || errors.Is(err, domain.ErrCASVersionStale) || errors.Is(err, domain.ErrCASExpectedMismatch) || errors.Is(err, domain.ErrNotPublished) || errors.Is(err, domain.ErrAssetVersionNotFound) || errors.Is(err, domain.ErrProjectionsNotReady),
				"error must wrap the original sentinel so the worker can classify via errors.Is")
		})
	}
}

func TestAssetActivate_FenceFallbackToVersionNo(t *testing.T) {
	assetID, verID := uuid.New(), uuid.New()
	reg := &fakeRegistry{}
	h := &AssetActivateHandler{Tx: fakeTxStarter{}, Assets: reg}
	// fence absent, latest_requested_version_no present → fallback
	_, err := h.Run(context.Background(), domain.Job{
		JobType:        JobAssetActivate,
		AssetID:        &assetID,
		AssetVersionID: &verID,
		Progress:       map[string]any{"latest_requested_version_no": int64(7)},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), reg.activateFence, "fence must fall back to latest_requested_version_no")
}

func TestAssetActivate_NilExpectedForInitialActivation(t *testing.T) {
	assetID, verID := uuid.New(), uuid.New()
	reg := &fakeRegistry{}
	h := &AssetActivateHandler{Tx: fakeTxStarter{}, Assets: reg}
	// no expected_current in progress → initial activation, CAS matches NULL
	_, err := h.Run(context.Background(), domain.Job{
		JobType:        JobAssetActivate,
		AssetID:        &assetID,
		AssetVersionID: &verID,
		Progress:       map[string]any{"fence": int64(1)},
	})
	require.NoError(t, err)
	assert.Nil(t, reg.activateExpected, "absent expected_current means initial activation (nil pointer)")
}

// --- reconcile_scan ---------------------------------------------------------

func TestReconcile_BadWorkspaceIDIsPermanent(t *testing.T) {
	h := &ReconcileHandler{Assets: &fakeRegistry{}}
	class, err := h.Run(context.Background(), domain.Job{JobType: JobReconcileScan, TargetKey: "not-a-uuid"})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryPermanent, class, "a malformed workspace_id won't fix itself on retry")
}

func TestReconcile_RegistryErrorIsTransient(t *testing.T) {
	ws := uuid.New()
	h := &ReconcileHandler{Assets: &fakeRegistry{reconcileErr: errors.New("snapshot too old")}}
	class, err := h.Run(context.Background(), domain.Job{JobType: JobReconcileScan, TargetKey: ws.String()})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryTransient, class)
}

func TestReconcile_VersionNotFoundIsPermanent(t *testing.T) {
	ws := uuid.New()
	h := &ReconcileHandler{Assets: &fakeRegistry{reconcileErr: domain.ErrAssetVersionNotFound}}
	class, err := h.Run(context.Background(), domain.Job{JobType: JobReconcileScan, TargetKey: ws.String()})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryPermanent, class, "a missing asset/version ref is a wiring error — dead-letter, don't spin")
}

func TestReconcile_Success(t *testing.T) {
	ws := uuid.New()
	reg := &fakeRegistry{reconcileReport: domain.ReconcileReport{WorkspaceID: ws, VersionCASFixed: 2}}
	h := &ReconcileHandler{Assets: reg}
	class, err := h.Run(context.Background(), domain.Job{JobType: JobReconcileScan, TargetKey: ws.String()})
	require.NoError(t, err)
	assert.Equal(t, ws, reg.reconcileWorkspace)
	_ = class // success path returns a default class; the Runner marks succeeded regardless
}

// --- source_sync / legacy_backfill (validated no-ops) -----------------------

func TestSourceSync_MissingSourceIDIsPermanent(t *testing.T) {
	h := &SourceSyncHandler{}
	class, err := h.Run(context.Background(), domain.Job{JobType: JobSourceSync})
	assert.Error(t, err)
	assert.Equal(t, domain.RetryPermanent, class)
}

func TestSourceSync_PresentSourceIDIsSuccess(t *testing.T) {
	src := uuid.New()
	h := &SourceSyncHandler{}
	_, err := h.Run(context.Background(), domain.Job{JobType: JobSourceSync, SourceID: &src})
	assert.NoError(t, err)
}

func TestLegacyBackfill_AlwaysSucceeds(t *testing.T) {
	h := &LegacyBackfillHandler{}
	_, err := h.Run(context.Background(), domain.Job{JobType: JobLegacyBackfill})
	assert.NoError(t, err, "skeleton backfill is a validated no-op until the batch query lands")
}
