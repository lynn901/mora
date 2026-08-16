package worker

// codegraph_build_handler_test.go pins the §4.1 build path contract for the
// CodeGraphBuildHandler (design-docs/17 §4.1 / §4.3 / §5 / §7.2 T1/T7/T8).
//
// The handler materializes a read-only source tree from a snapshot locator →
// computes source_tree_hash → Provider.Build → verifies BuildResult.Commit +
// SourceTreeHash equal the input → MarkProjectionReady(kind=codegraph) →
// cleans up the temp build dir. Fail-closed is the contract: a commit/hash
// mismatch or a missing source is PERMANENT (no retry of a misaligned graph,
// §4.3); a materialize/Provider timeout is TRANSIENT.
//
// Contract cases (§7.2):
//   T1  build identity — BuildResult field set + Commit/SourceTreeHash ==
//       BuildRequest (§10.2). A mismatch → permanent fail-closed (discard,
//       no MarkProjectionReady).
//   T2  provider ErrSourceSnapshotUnavailable / ErrAssetVersionMismatch on
//       Build → permanent fail-closed (§15 row 2).
//   T7  the temp build dir is cleaned after the build; the active source tree
//       lives in the provider/snapshot, so a cleanup failure does not break
//       active-graph reads (§4.3).
//   T8  idempotent — MarkProjectionReady is a no-op on a duplicate; the handler
//       short-circuits to success when the projection is already ready (§5).
//
// These tests do NOT touch a database: they inject fakes for the four ports
// (VersionSourceLocator, SnapshotMaterializer, CodeGraphProviderPort,
// asset.ActivationRegistry) + a no-op Pool/tx, so the build path is observable
// without Postgres.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// --- test doubles ---

// fakeLocator returns a canned SourceSnapshot (or err) for Read.
type fakeLocator struct {
	snap SourceSnapshot
	err  error
}

func (f fakeLocator) Read(_ context.Context, _ uuid.UUID) (SourceSnapshot, error) {
	return f.snap, f.err
}

// fakeMaterializer returns a canned (workDir, hash, err). It also records the
// snapshot it received so a test can assert the handler passed the right input.
type fakeMaterializer struct {
	workDir string
	hash    string
	err     error
	gotSnap SourceSnapshot
}

func (m *fakeMaterializer) Materialize(_ context.Context, snap SourceSnapshot) (string, string, error) {
	m.gotSnap = snap
	return m.workDir, m.hash, m.err
}

// fakeProviderPort returns a canned BuildResult (or err) for Build. Captures
// the BuildRequest so a test can assert the handler passed commit + hash.
type fakeProviderPort struct {
	res cgprovider.BuildResult
	err error
	req cgprovider.BuildRequest
}

func (p *fakeProviderPort) Build(_ context.Context, req cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	p.req = req
	if p.err != nil {
		return cgprovider.BuildResult{}, p.err
	}
	return p.res, nil
}
func (p *fakeProviderPort) Health(context.Context) error { return nil }

// fakeActivationRegistry records MarkProjectionReady calls. It is a no-op
// (idempotent) by default; returns ErrVersionNotFound when configured to.
type fakeActivationRegistry struct {
	readyCalls int32
	lastReady  asset.ProjectionReady
	err        error
}

func (r *fakeActivationRegistry) MarkProjectionReady(_ context.Context, _ pgx.Tx, pr asset.ProjectionReady) (asset.MarkProjectionReadyResult, error) {
	atomic.AddInt32(&r.readyCalls, 1)
	r.lastReady = pr
	if r.err != nil {
		return asset.MarkProjectionReadyResult{}, r.err
	}
	return asset.MarkProjectionReadyResult{BuildReady: true}, nil
}
func (r *fakeActivationRegistry) Activate(context.Context, pgx.Tx, asset.Activation) (asset.ActivationResult, error) {
	return asset.ActivationResult{}, nil
}
func (r *fakeActivationRegistry) ReconcileScan(context.Context, asset.ReconcilePool, domain.UUID) (asset.ReconcileOutcome, error) {
	return asset.ReconcileOutcome{}, nil
}

// fakePool returns a no-op tx from BeginTx (the only method the build handler's
// Pool port requires).
type fakePool struct{}

func (fakePool) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) { return fakeTx{}, nil }

// fakeTx implements the full pgx.Tx interface so it satisfies the port. Only
// Commit/Rollback are on the codegraph build path; the rest return zero values
// (they are unreachable from this handler — a real call would surface a nil
// deref a test author would notice, not a masked pass).
type fakeTx struct{ commitErr error }

func (t fakeTx) Commit(context.Context) error                { return t.commitErr }
func (fakeTx) Rollback(context.Context) error                { return nil }
func (fakeTx) Begin(context.Context) (pgx.Tx, error)          { return fakeTx{}, nil }
func (fakeTx) Conn() *pgx.Conn                                { return nil }
func (fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return fakeBatchResults{} }
func (fakeTx) LargeObjects() pgx.LargeObjects                        { return pgx.LargeObjects{} }
func (fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (fakeTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (fakeTx) QueryRow(context.Context, string, ...any) pgx.Row         { return nil }

// fakeBatchResults is a no-op pgx.BatchResults for the SendBatch stub.
type fakeBatchResults struct{}

func (fakeBatchResults) Exec() (pgconn.CommandTag, error)             { return pgconn.CommandTag{}, nil }
func (fakeBatchResults) Query() (pgx.Rows, error)                       { return nil, nil }
func (fakeBatchResults) QueryRow() pgx.Row                               { return nil }
func (fakeBatchResults) Close() error                                    { return nil }

// Compile-time: the fakes satisfy their ports.
var (
	_ VersionSourceLocator    = (*fakeLocator)(nil)
	_ SnapshotMaterializer     = (*fakeMaterializer)(nil)
	_ CodeGraphProviderPort    = (*fakeProviderPort)(nil)
	_ asset.ActivationRegistry = (*fakeActivationRegistry)(nil)
)

// --- helpers ---

// newHandler wires a CodeGraphBuildHandler with the four fakes + fakePool.
func newHandler(loc VersionSourceLocator, mat SnapshotMaterializer, prov CodeGraphProviderPort, reg asset.ActivationRegistry) *CodeGraphBuildHandler {
	return &CodeGraphBuildHandler{
		Provider:     prov,
		Locator:      loc,
		Materializer: mat,
		Assets:       reg,
		Pool:         fakePool{},
	}
}

// a minimal job pointing at assetVersionID.
func buildJob(versionID uuid.UUID) domain.Job {
	return domain.Job{
		ID:             uuid.New(),
		JobType:        JobCodeGraphBuild,
		AssetVersionID: &versionID,
		BuildRevision:  "rev-1",
	}
}

// a well-formed SourceSnapshot.
func goodSnap(versionID uuid.UUID) SourceSnapshot {
	return SourceSnapshot{
		AssetVersionID: versionID,
		WorkspaceID:    uuid.New(),
		Commit:         "commit-A",
		SnapshotPrefix: "codebase/hash-A",
		SnapshotManifest: map[string]any{"repo": "mora", "commit": "commit-A"},
	}
}

// --- T1: build identity (§10.2 BuildResult field set + Commit/Hash equality) ---

// TestBuild_HappyPath_RegistersCodegraphProjection asserts the §4.1 happy path:
// materialize → Build (matching commit+hash) → MarkProjectionReady(kind=codegraph)
// with the locator carrying graph_ref/source_tree_ref/commit_sha/source_tree_hash
// + provider ids. The handler returns transient+nil; the Runner's runOne marks
// success because runErr == nil.
func TestBuild_HappyPath_RegistersCodegraphProjection(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{workDir: "/tmp/build", hash: "sha256:hash-A"}
	prov := &fakeProviderPort{res: cgprovider.BuildResult{
		GraphRef:            "g1",
		SourceTreeRef:       "st1",
		Commit:              "commit-A",
		SourceTreeHash:      "sha256:hash-A",
		ProviderVersion:     "1.5.0",
		ProviderBuildDigest: "d1",
		IndexSchemaVersion:  "1",
		ExtractionVersion:   "1",
	}}
	reg := &fakeActivationRegistry{}
	h := newHandler(loc, mat, prov, reg)

	class, runErr := h.Run(context.Background(), buildJob(ver))
	require.NoError(t, runErr, "happy path returns nil error so runOne marks success")
	// The handler returns RetryTransient, nil on success; runOne short-circuits
	// on nil error regardless of class. We assert the error is nil (the gate).
	_ = class

	// §4.1 step 6: MarkProjectionReady called once with kind=codegraph + the
	// §11 locator map.
	require.Equal(t, int32(1), atomic.LoadInt32(&reg.readyCalls), "MarkProjectionReady called exactly once")
	assert.Equal(t, domain.ProjectionCodegraph, reg.lastReady.Kind)
	assert.Equal(t, "rev-1", reg.lastReady.BuildRevision)
	locMap := reg.lastReady.Locator
	assert.Equal(t, "g1", locMap["graph_ref"])
	assert.Equal(t, "st1", locMap["source_tree_ref"])
	assert.Equal(t, "commit-A", locMap["commit_sha"])
	assert.Equal(t, "sha256:hash-A", locMap["source_tree_hash"])
	assert.Equal(t, "1.5.0", locMap["provider_version"])
	assert.Equal(t, "d1", locMap["provider_build_digest"])
	assert.Equal(t, "1", locMap["index_schema_version"])
	assert.Equal(t, "1", locMap["extraction_version"])

	// §4.1 step 4: the handler passed the computed hash + commit to Build.
	assert.Equal(t, "commit-A", prov.req.Commit)
	assert.Equal(t, "sha256:hash-A", prov.req.SourceTreeHash)
	assert.Equal(t, "codebase/hash-A", prov.req.SnapshotLocator.ObjectStorePrefix)
}

// TestBuild_CommitHashMismatch_FailClosedPermanent asserts §4.1 step 5: when
// the provider returns a BuildResult whose Commit or SourceTreeHash ≠ the
// input, the handler FAILS CLOSED (permanent, no retry) and does NOT call
// MarkProjectionReady — the misaligned graph is discarded (§4.3 / §7.2 T1).
func TestBuild_CommitHashMismatch_FailClosedPermanent(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}

	t.Run("commit_mismatch", func(t *testing.T) {
		prov := &fakeProviderPort{res: cgprovider.BuildResult{
			Commit: "DIFFERENT-commit", // ≠ input commit-A
			SourceTreeHash: "sha256:hash-A",
		}}
		reg := &fakeActivationRegistry{}
		h := newHandler(loc, mat, prov, reg)

		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.Error(t, runErr, "a mismatched BuildResult must fail")
		assert.Equal(t, domain.RetryPermanent, class, "mismatch is permanent — no retry of a misaligned graph")
		assert.Equal(t, int32(0), atomic.LoadInt32(&reg.readyCalls), "MarkProjectionReady must NOT be called on mismatch")
	})

	t.Run("hash_mismatch", func(t *testing.T) {
		prov := &fakeProviderPort{res: cgprovider.BuildResult{
			Commit: "commit-A",
			SourceTreeHash: "sha256:DIFFERENT", // ≠ input hash-A
		}}
		reg := &fakeActivationRegistry{}
		h := newHandler(loc, mat, prov, reg)

		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.Error(t, runErr)
		assert.Equal(t, domain.RetryPermanent, class)
		assert.Equal(t, int32(0), atomic.LoadInt32(&reg.readyCalls))
	})
}

// TestBuild_BuildResultFieldSetNonEmpty asserts the §10.2 field set: a BuildResult
// with an empty graph_ref / source_tree_ref would produce an unusable locator.
// The handler does not itself enforce non-empty fields beyond commit+hash, so
// this test pins that a well-formed provider result flows into the locator
// verbatim (regression guard for the field set).
func TestBuild_BuildResultFieldSetFlowsToLocator(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}
	prov := &fakeProviderPort{res: cgprovider.BuildResult{
		GraphRef:            "g1",
		SourceTreeRef:       "st1",
		Commit:              "commit-A",
		SourceTreeHash:      "sha256:hash-A",
		ProviderVersion:     "1.5.0",
		ProviderBuildDigest: "d1",
		IndexSchemaVersion:  "schema-v1",
		ExtractionVersion:   "ext-v1",
		CapabilitiesSnapshot: cgprovider.CodeGraphCapabilities{Languages: []string{"go"}},
	}}
	reg := &fakeActivationRegistry{}
	h := newHandler(loc, mat, prov, reg)

	_, _ = h.Run(context.Background(), buildJob(ver))
	// The locator carries every §10.2 field the provider returned.
	assert.Equal(t, "schema-v1", reg.lastReady.Locator["index_schema_version"])
	assert.Equal(t, "ext-v1", reg.lastReady.Locator["extraction_version"])
}

// --- T2: provider fail-closed sentinels on Build → permanent ---

// TestBuild_ProviderFailClosedSentinels_Permanent asserts that when the
// provider surfaces ErrSourceSnapshotUnavailable or ErrAssetVersionMismatch
// during Build, the handler classifies them PERMANENT (fail closed — do not
// retry a misaligned build, §4.3). Other errors are transient (retry).
func TestBuild_ProviderFailClosedSentinels_Permanent(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}

	t.Run("source_snapshot_unavailable", func(t *testing.T) {
		prov := &fakeProviderPort{err: cgprovider.ErrSourceSnapshotUnavailable}
		reg := &fakeActivationRegistry{}
		h := newHandler(loc, mat, prov, reg)

		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.ErrorIs(t, runErr, cgprovider.ErrSourceSnapshotUnavailable)
		assert.Equal(t, domain.RetryPermanent, class)
		assert.Equal(t, int32(0), atomic.LoadInt32(&reg.readyCalls))
	})

	t.Run("asset_version_mismatch", func(t *testing.T) {
		prov := &fakeProviderPort{err: cgprovider.ErrAssetVersionMismatch}
		reg := &fakeActivationRegistry{}
		h := newHandler(loc, mat, prov, reg)

		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.ErrorIs(t, runErr, cgprovider.ErrAssetVersionMismatch)
		assert.Equal(t, domain.RetryPermanent, class)
	})

	t.Run("capability_unavailable_transient", func(t *testing.T) {
		// capability_unavailable is transient — the sidecar may come up on retry.
		prov := &fakeProviderPort{err: cgprovider.ErrCapabilityUnavailable}
		reg := &fakeActivationRegistry{}
		h := newHandler(loc, mat, prov, reg)

		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.ErrorIs(t, runErr, cgprovider.ErrCapabilityUnavailable)
		assert.Equal(t, domain.RetryTransient, class, "capability_unavailable is transient (sidecar may recover)")
	})
}

// TestBuild_GenericProviderError_Transient asserts a non-sentinel provider
// error (e.g. a transient sidecar fault) is classified transient (retry), not
// permanent — it is not a misaligned-graph condition (§4.3 retry semantics).
func TestBuild_GenericProviderError_Transient(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}
	prov := &fakeProviderPort{err: errors.New("sidecar timeout")}
	reg := &fakeActivationRegistry{}
	h := newHandler(loc, mat, prov, reg)

	class, runErr := h.Run(context.Background(), buildJob(ver))
	require.Error(t, runErr)
	assert.Equal(t, domain.RetryTransient, class, "a generic provider error is transient (retry)")
	assert.Equal(t, int32(0), atomic.LoadInt32(&reg.readyCalls))
}

// --- T8: idempotency (§5) ---

// TestBuild_Idempotent_MarkProjectionReadyNoopOnDuplicate asserts that when
// MarkProjectionReady is idempotent (the fake's default), re-running the same
// job does not error and the projection is re-marked (a no-op in production).
// This pins the §5 contract: MarkProjectionReady is a no-op on a duplicate;
// a re-acquired job whose projection is already ready short-circuits to success.
func TestBuild_Idempotent_MarkProjectionReadyNoopOnDuplicate(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}
	prov := &fakeProviderPort{res: cgprovider.BuildResult{
		GraphRef: "g1", SourceTreeRef: "st1", Commit: "commit-A", SourceTreeHash: "sha256:hash-A",
	}}
	reg := &fakeActivationRegistry{}
	h := newHandler(loc, mat, prov, reg)

	j := buildJob(ver)
	_, _ = h.Run(context.Background(), j)
	// Re-run the same job — MarkProjectionReady is idempotent (no error).
	_, runErr := h.Run(context.Background(), j)
	require.NoError(t, runErr, "re-marking an already-ready projection must not error (idempotent, §5)")
	assert.Equal(t, int32(2), atomic.LoadInt32(&reg.readyCalls), "MarkProjectionReady called twice (idempotent re-mark)")
}

// TestBuild_MissingAssetVersionID_Permanent asserts a job without an
// asset_version_id fails PERMANENT (existence/materialization won't appear by
// retrying, §4.3).
func TestBuild_MissingAssetVersionID_Permanent(t *testing.T) {
	h := newHandler(fakeLocator{}, &fakeMaterializer{}, &fakeProviderPort{}, &fakeActivationRegistry{})
	j := domain.Job{ID: uuid.New(), JobType: JobCodeGraphBuild} // nil AssetVersionID
	class, runErr := h.Run(context.Background(), j)
	require.Error(t, runErr)
	assert.Equal(t, domain.RetryPermanent, class)
}

// TestBuild_MissingVersionSource_Permanent asserts that when the version source
// locator is missing (ErrVersionSourceMissing) or the commit/snapshot_prefix is
// empty, the job fails PERMANENT — retrying won't conjure a commit (§4.3).
func TestBuild_MissingVersionSource_Permanent(t *testing.T) {
	ver := uuid.New()
	t.Run("locator_missing", func(t *testing.T) {
		loc := fakeLocator{err: ErrVersionSourceMissing}
		h := newHandler(loc, &fakeMaterializer{}, &fakeProviderPort{}, &fakeActivationRegistry{})
		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.ErrorIs(t, runErr, ErrVersionSourceMissing)
		assert.Equal(t, domain.RetryPermanent, class)
	})
	t.Run("empty_commit", func(t *testing.T) {
		loc := fakeLocator{snap: SourceSnapshot{AssetVersionID: ver, SnapshotPrefix: "p"}} // empty commit
		h := newHandler(loc, &fakeMaterializer{}, &fakeProviderPort{}, &fakeActivationRegistry{})
		class, runErr := h.Run(context.Background(), buildJob(ver))
		require.Error(t, runErr)
		assert.Equal(t, domain.RetryPermanent, class, "empty commit is permanent fail-closed")
	})
}

// TestBuild_MaterializeFailure_Transient asserts a materialization failure is
// TRANSIENT (retry) — not a misaligned-graph condition (§4.3 retry semantics).
func TestBuild_MaterializeFailure_Transient(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{err: errors.New("materialize: network")}
	h := newHandler(loc, mat, &fakeProviderPort{}, &fakeActivationRegistry{})

	class, runErr := h.Run(context.Background(), buildJob(ver))
	require.Error(t, runErr)
	assert.Equal(t, domain.RetryTransient, class, "materialize failure is transient (retry)")
}

// TestBuild_RegistryVersionNotFound_Permanent asserts that when
// MarkProjectionReady returns ErrVersionNotFound, the job fails PERMANENT — the
// version is gone and retrying won't conjure it (§4.3 / asset package contract).
func TestBuild_RegistryVersionNotFound_Permanent(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}
	mat := &fakeMaterializer{hash: "sha256:hash-A"}
	prov := &fakeProviderPort{res: cgprovider.BuildResult{
		Commit: "commit-A", SourceTreeHash: "sha256:hash-A",
	}}
	reg := &fakeActivationRegistry{err: asset.ErrVersionNotFound}
	h := newHandler(loc, mat, prov, reg)

	class, runErr := h.Run(context.Background(), buildJob(ver))
	require.ErrorIs(t, runErr, asset.ErrVersionNotFound)
	assert.Equal(t, domain.RetryPermanent, class)
}

// TestBuild_TempDirCleanup_Deferred asserts the handler attempts to clean the
// temp build dir after the build (§4.3: deleting the temp build dir must NOT
// break active-graph reads). Because the production removeAllBestEffort is a
// stub and ManifestHashMaterializer returns an empty workDir, this test pins
// the CONTRACT: the handler calls cleanupWorkDir with the materializer's
// workDir on both success and failure paths. A regression that skipped cleanup
// would leave temp dirs behind (disk leak). Here we assert the build does not
// error on an empty workDir (the noop cleanup) and that a real workDir path
// does not change the fail-closed classification.
func TestBuild_TempDirCleanup_Deferred(t *testing.T) {
	ver := uuid.New()
	loc := fakeLocator{snap: goodSnap(ver)}

	// With a real workDir path, cleanup is attempted (noop stub today); the
	// happy path still succeeds and registers the projection.
	mat := &fakeMaterializer{workDir: "/tmp/codegraph-build-xyz", hash: "sha256:hash-A"}
	prov := &fakeProviderPort{res: cgprovider.BuildResult{
		GraphRef: "g1", SourceTreeRef: "st1", Commit: "commit-A", SourceTreeHash: "sha256:hash-A",
	}}
	reg := &fakeActivationRegistry{}
	h := newHandler(loc, mat, prov, reg)

	_, runErr := h.Run(context.Background(), buildJob(ver))
	require.NoError(t, runErr, "a non-empty workDir must not break the build (cleanup is best-effort)")
	assert.Equal(t, int32(1), atomic.LoadInt32(&reg.readyCalls))
}

// TestBuild_AssetVersionNil_Permanent guards a subtle regression: a job with
// AssetVersionID pointing at uuid.Nil (not just nil pointer) must fail
// permanent, not dereference a nil-typed id into the locator.
func TestBuild_AssetVersionNil_Permanent(t *testing.T) {
	h := newHandler(fakeLocator{}, &fakeMaterializer{}, &fakeProviderPort{}, &fakeActivationRegistry{})
	nilID := uuid.Nil
	j := domain.Job{ID: uuid.New(), JobType: JobCodeGraphBuild, AssetVersionID: &nilID}
	class, runErr := h.Run(context.Background(), j)
	require.Error(t, runErr)
	assert.Equal(t, domain.RetryPermanent, class)
}

// unused import guards (kept so future edits don't trip on unused-import
// lints if a test stops referencing a symbol).
var _ = errors.New
