//go:build integration

// asset_registry_cas_test.go covers the §10.3 version atomic-switch invariants
// (design-docs/14 §10.3 用例 20–24) against a real Postgres with the 013+014
// schema applied. The CAS is a SQL UPDATE … WHERE clause and the ready-gate is
// a count(DISTINCT projection_kind) assertion, so the invariants only hold
// against the real engine — these are integration tests gated on DATABASE_URL
// (same convention as the migration runner tests).
//
// SUT SCOPE (what IS implemented and tested here):
//  - RegisterDocumentAsset (§3.1 native dual-write): bumps
//    latest_requested_version_no monotonically (GREATEST barrier) and
//    CAS-activates current_version_id under WHERE latest_requested_version_no
//    = $3 — so an old version completing LATE cannot overwrite a newer
//    current_version_id (用例 21 "失败不覆盖 / 旧版本不切换").
//  - Native documents are stamped build_status='ready', governance_status=
//    'published' at write time; current_version_id therefore always points at
//    a usable version (用例 23 candidate-not-returned invariant).
//  - MarkProjectionReady + Activate (§7 async path, D4): MarkProjectionReady
//    flips build_status='ready' ONLY when every required projection has a
//    ready row (用例 20 — a missing required projection blocks ready); Activate
//    performs the CAS under the monotonic fence + expected_current and refuses
//    to switch current_version_id on failure (用例 22 stale-CAS-fail,
//    用例 24 missing-expected-current-rejected). Activate also re-asserts the
//    required-projections-ready gate as defense-in-depth.
//
// HISTORY: 用例 20/22/24 were previously listed as a SUT gap because the
// async path (MarkProjectionReady/Activate) was not yet implemented. D4 landed
// the implementation; these three tests now automate the invariants against
// the real SQL. (P0 D4-1 — a missing required projection row was treated as
// "ready" by an earlier count-non-ready gate — was found by the §10 review and
// fixed in the same change as these tests.)
package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// casTestPool gates on DATABASE_URL, same convention as the migration / sink
// integration tests.
func casTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedCASWorkspace inserts the minimal dependencies a knowledge_asset row needs
// (user + workspace + a native document with two document_version rows).
// Returns the ids the registration calls reference. Mirrors seedLegacyDoc in
// the migration runner integration test (same table shapes).
//
// Test isolation: the workspace row is registered for CASCADE deletion in
// t.Cleanup so the CAS tests' documents/assets do not leak into the migration
// reconcile scan (which scans published documents lacking a knowledge_asset
// row) when the integration suite runs cross-package against one DB.
func seedCASWorkspace(t *testing.T, pool *pgxpool.Pool) (wsID, userID, docID, docVer1, docVer2 uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	userID, wsID = uuid.New(), uuid.New()
	docID, docVer1, docVer2 = uuid.New(), uuid.New(), uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, name, status) VALUES ($1,$2,'CAS Test','active')`,
		userID, "cas_"+userID.String()[:8]+"@mora.local")
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1,'CAS WS',$2,$3)`,
		wsID, "cas-"+wsID.String()[:8], userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO documents (id, workspace_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, parse_status)
		VALUES ($1,$2,'Doc','["p",{"t":"b"}]'::jsonb,'b','blocks','published','pending',1,$3,$3,'parsed')`,
		docID, wsID, userID)
	require.NoError(t, err)
	for vno, v := range []uuid.UUID{docVer1, docVer2} {
		_, err = pool.Exec(ctx, `
			INSERT INTO document_versions (id, document_id, version_no, content, content_text, author_id)
			VALUES ($1,$2,$3,'["p",{"t":"b"}]'::jsonb,'b',$4)`,
			v, docID, vno+1, userID)
		require.NoError(t, err)
	}
	// Deleting the workspace CASCADES to documents, document_versions,
	// knowledge_assets, knowledge_asset_versions — full cleanup so no row
	// leaks into a later reconcile/backfill scan.
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})
	return
}

// currentVersionOf reads back the asset's current_version_id + latest barrier.
func currentVersionOf(t *testing.T, pool *pgxpool.Pool, assetID uuid.UUID) (current *uuid.UUID, latest int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT current_version_id, latest_requested_version_no FROM knowledge_assets WHERE id=$1`,
		assetID).Scan(&current, &latest)
	require.NoError(t, err)
	return
}

// versionStatusOf reads a version row's build_status + governance_status.
func versionStatusOf(t *testing.T, pool *pgxpool.Pool, versionID uuid.UUID) (build, gov string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT build_status, governance_status FROM knowledge_asset_versions WHERE id=$1`,
		versionID).Scan(&build, &gov)
	require.NoError(t, err)
	return
}

// --- §10.3 用例 21: a late-completing OLD version does NOT overwrite a newer
// current_version_id (CAS fail-no-overwrite) ---

// TestCAS_OldVersionCompletingLateDoesNotOverwrite asserts the §10.3 用例 21
// invariant: version 2 is registered first and CAS-activates current_version_id
// to v2. Version 1 then "completes" (is registered after, simulating a
// late-finishing job). The CAS WHERE latest_requested_version_no = $3 must
// NOT match for v1 (the barrier has already advanced to 2), so
// current_version_id STAYS on v2 — the old version does not rewind the
// pointer (§6.4 单调栅栏 / §7 CAS 失败不覆盖).
func TestCAS_OldVersionCompletingLateDoesNotOverwrite(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, docID, docVer1, docVer2 := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()

	// v2 registers first — becomes current.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	r2, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: docVer2, VersionNo: 2,
		Title: "v2", CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	current, latest := currentVersionOf(t, pool, r2.AssetID)
	require.NotNil(t, current, "v2 must CAS-activate current_version_id")
	assert.Equal(t, r2.AssetVersionID, *current, "current_version_id points at v2")
	assert.Equal(t, int64(2), latest, "barrier advanced to 2")

	// v1 completes LATE (registered after v2) — must NOT overwrite v2.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	r1, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: docVer1, VersionNo: 1,
		Title: "v1", CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	current2, latest2 := currentVersionOf(t, pool, r1.AssetID)
	// §10.3 用例 21 red line: current_version_id still points at v2, not v1.
	assert.Equal(t, r2.AssetVersionID, *current2,
		"a late-completing old version must NOT overwrite a newer current_version_id (§7 CAS 失败不覆盖)")
	assert.Equal(t, int64(2), latest2,
		"the monotonic barrier must not rewind (GREATEST guard)")
	assert.NotEqual(t, r1.AssetVersionID, *current2,
		"the old version's row must not become current")
}

// --- §10.3 用例 21 (supplement): re-registering the SAME version does not
// churn current_version_id (idempotent) ---

// TestCAS_ReregisterSameVersionIsIdempotent asserts re-registering an existing
// version (dedupe_key hit, versionCreated=false) is a no-op: it does not touch
// current_version_id and does not advance the barrier. This is the idempotent
// retry contract for the CAS path (a re-delivered event must not churn state).
func TestCAS_ReregisterSameVersionIsIdempotent(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, docID, docVer1, _ := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	r1, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: docVer1, VersionNo: 1,
		Title: "v1", CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	currentBefore, latestBefore := currentVersionOf(t, pool, r1.AssetID)

	// Re-register the SAME version — idempotent no-op (Created=false).
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	r1b, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: docVer1, VersionNo: 1,
		Title: "v1", CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	assert.False(t, r1b.Created, "re-registering an existing version is a no-op (dedupe_key hit)")

	currentAfter, latestAfter := currentVersionOf(t, pool, r1b.AssetID)
	assert.Equal(t, currentBefore, currentAfter, "re-register must not churn current_version_id")
	assert.Equal(t, latestBefore, latestAfter, "re-register must not advance the barrier")
}

// --- §10.3 用例 23 (invariant pin): current_version_id only ever points at a
// usable (ready+published) version; a candidate version is NOT surfaced ---

// TestCAS_CurrentVersionAlwaysUsuableCandidateNotReturned asserts the §10.3
// 用例 23 invariant from the read side: current_version_id, once set, points
// at a version with build_status='ready' AND governance_status='published'
// (the authz usable-version gate's contract). A version stamped candidate by
// a separate write (simulating the connector-sourced candidate flow the async
// Activate path would transition) is NOT the version current_version_id points
// at — it must not be returned as the current/usable version.
//
// This pins the invariant the §7 authz gate relies on; the candidate→published
// TRANSITION itself is the async Activate path that is ErrNotWired today
// (reported as a §10.3 SUT gap for 用例 20/22/24).
func TestCAS_CurrentVersionAlwaysUsuableCandidateNotReturned(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, docID, docVer1, _ := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	r1, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: docVer1, VersionNo: 1,
		Title: "v1", CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	current, _ := currentVersionOf(t, pool, r1.AssetID)
	require.NotNil(t, current)
	build, gov := versionStatusOf(t, pool, *current)
	assert.Equal(t, "ready", build, "current_version_id points at a ready version")
	assert.Equal(t, "published", gov, "current_version_id points at a published version")

	// Simulate a second candidate version (connector-sourced, not yet
	// activated). It is written directly with governance_status='candidate'.
	candVer, candVersionRow := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO knowledge_asset_versions
		   (id, asset_id, version_no, content_origin, dedupe_key,
		    build_status, governance_status, created_by_type, created_by_id)
		 VALUES ($1,$2,2,'human',$3,'pending','candidate','user',$4)`,
		candVersionRow, r1.AssetID, "document_version:"+candVer.String(), userID)
	require.NoError(t, err)

	// The candidate version is NOT the current one, and it is NOT usable per
	// the authz gate's contract (build_status != 'ready' AND gov != 'published').
	current2, _ := currentVersionOf(t, pool, r1.AssetID)
	assert.NotEqual(t, candVersionRow, *current2,
		"a candidate version must NOT be returned as current_version_id (§10.3 用例 23)")
	cBuild, cGov := versionStatusOf(t, pool, candVersionRow)
	assert.True(t, cBuild != "ready" || cGov != "published",
		"a candidate version must not satisfy the usable-version invariant")
}

// --- async-path helpers (用例 20/22/24 target the §7 Activate path) ---

// seedCandidateAsset creates an asset + a single candidate version whose
// build_status/governance_status/activation_policy_snapshot are caller-
// controlled, simulating the connector-sourced async path (not the native
// dual-write, which stamps ready+published). current_version_id is left NULL
// (the async CAS Activate is what would set it). Returns the asset + version
// ids the Activate/MarkProjectionReady calls reference.
//
// dedupe_key is derived from a fresh UUID so repeated runs don't collide.
func seedCandidateAsset(t *testing.T, pool *pgxpool.Pool, wsID, userID uuid.UUID, versionNo int64, buildStatus, govStatus, snapshotJSON string) (assetID, versionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	assetID, versionID = uuid.New(), uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO knowledge_assets
		   (id, workspace_id, asset_type, name, owner_type, owner_id, current_version_id, latest_requested_version_no)
		 VALUES ($1,$2,'document','Async CAS Test','user',$3,NULL,$4)`,
		assetID, wsID, userID, versionNo)
	require.NoError(t, err)
	var snap any
	if snapshotJSON != "" {
		snap = snapshotJSON
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO knowledge_asset_versions
		   (id, asset_id, version_no, content_origin, dedupe_key,
		    build_status, governance_status, activation_policy_snapshot,
		    created_by_type, created_by_id)
		 VALUES ($1,$2,$3,'generated',$4,$5,$6,$7::jsonb,'user',$8)`,
		versionID, assetID, versionNo, "asset_version:"+versionID.String(),
		buildStatus, govStatus, snap, userID)
	require.NoError(t, err)
	return assetID, versionID
}

// addProjectionRow inserts an asset_projections row in the given status.
// buildRevision disambiguates rows so MarkProjectionReady's ON CONFLICT
// (asset_version_id, projection_kind, build_revision) is exercised realistically.
func addProjectionRow(t *testing.T, pool *pgxpool.Pool, versionID uuid.UUID, kind, provider, buildRevision, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO asset_projections
		   (asset_version_id, projection_kind, provider, build_revision, status, built_at)
		 VALUES ($1,$2,$3,$4,$5,now())`,
		versionID, kind, provider, buildRevision, status)
	require.NoError(t, err)
}

// publishVersion flips governance_status to 'published' (simulating the
// governance review approval the async path waits on).
func publishVersion(t *testing.T, pool *pgxpool.Pool, versionID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE knowledge_asset_versions SET governance_status='published' WHERE id=$1`,
		versionID)
	require.NoError(t, err)
}

// --- §10.3 用例 20: a missing required projection must NOT flip
// build_status='ready', and Activate must refuse (P0 D4-1) ---

// TestAsyncCAS_MissingRequiredProjectionBlocksReady asserts the §7 red-line
// the §10 review flagged as P0 (D4-1): when a version's
// activation_policy_snapshot requires ["fts","vector"] but only "fts" has a
// ready row (the "vector" projection was NEVER built — no row at all),
// MarkProjectionReady("fts") must NOT flip build_status to 'ready', and
// Activate must return ErrProjectionsNotReady (not switch current_version_id).
//
// The earlier count-non-ready gate treated the missing "vector" row as "ready"
// (count of non-ready rows = 0 → flip), which let a version with no vector
// index activate. The fix counts DISTINCT ready required kinds and requires
// == len(required); a missing kind blocks ready.
func TestAsyncCAS_MissingRequiredProjectionBlocksReady(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, _, _, _ := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()
	// required = ["fts","vector"]; version starts pending/candidate.
	snapshot := `{"required_projections":["fts","vector"]}`
	assetID, verID := seedCandidateAsset(t, pool, wsID, userID, 1, "pending", "candidate", snapshot)

	// Only the fts projection is built + ready. vector has NO row at all.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.MarkProjectionReady(ctx, tx, verID, domain.ProjectionFts, "rag-worker", "rev-fts-1", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// §7 失败不覆盖: build_status must still be 'pending' — the missing
	// vector projection blocks the ready flip.
	build, _ := versionStatusOf(t, pool, verID)
	assert.Equal(t, domain.VersionBuildPending, build,
		"a missing required projection must NOT flip build_status to ready (P0 D4-1)")

	// Even if governance were to publish it, Activate must refuse — vector is
	// still absent. (Defense-in-depth: Activate re-asserts the gate, not just
	// trust build_status.)
	publishVersion(t, pool, verID)
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.Activate(ctx, tx, assetID, verID, 1, nil) // expected_current=nil (initial activation)
	assert.ErrorIs(t, err, domain.ErrProjectionsNotReady,
		"Activate must refuse when a required projection has no ready row (§7 部分就绪不得覆盖)")
	require.NoError(t, tx.Rollback(ctx))

	// current_version_id must be untouched (still NULL).
	current, _ := currentVersionOf(t, pool, assetID)
	assert.Nil(t, current, "current_version_id must not switch when projections are incomplete")
}

// --- §10.3 用例 22: a stale version completing late must mark ready but NOT
// switch current_version_id (CAS fail-no-overwrite) ---

// TestAsyncCAS_StaleVersionCompletingLateDoesNotSwitch asserts: version 2
// activates current_version_id to v2 (all projections ready + published);
// then version 1 (the older fence) completes late — MarkProjectionReady can
// flip v1's build_status to ready, but Activate(v1, fence=1) must fail the
// monotonic barrier (latest_requested_version_no has advanced to 2) and
// current_version_id must STAY on v2.
func TestAsyncCAS_StaleVersionCompletingLateDoesNotSwitch(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, _, _, _ := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()
	snapshot := `{"required_projections":["fts"]}` // single projection so we can fully build it

	// v2: fully ready + published → activates. Bump the asset's fence to 2.
	assetID, v2 := seedCandidateAsset(t, pool, wsID, userID, 2, "pending", "candidate", snapshot)
	addProjectionRow(t, pool, v2, "fts", "rag-worker", "rev-v2-fts", "ready")
	publishVersion(t, pool, v2)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.MarkProjectionReady(ctx, tx, v2, domain.ProjectionFts, "rag-worker", "rev-v2-fts", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	// Now activate v2 under fence=2, expected_current=nil (initial).
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, reg.Activate(ctx, tx, assetID, v2, 2, nil))
	require.NoError(t, tx.Commit(ctx))
	current, latest := currentVersionOf(t, pool, assetID)
	require.NotNil(t, current)
	assert.Equal(t, v2, *current, "v2 activated as current")
	assert.Equal(t, int64(2), latest, "barrier at 2")

	// v1 completes LATE: same asset, older version. Its build can flip ready,
	// but the CAS under fence=1 must fail (barrier already at 2).
	v1 := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO knowledge_asset_versions
		   (id, asset_id, version_no, content_origin, dedupe_key,
		    build_status, governance_status, activation_policy_snapshot,
		    created_by_type, created_by_id)
		 VALUES ($1,$2,1,'generated',$3,'pending','published',$4::jsonb,'user',$5)`,
		v1, assetID, "asset_version:"+v1.String(), snapshot, userID)
	require.NoError(t, err)
	addProjectionRow(t, pool, v1, "fts", "rag-worker", "rev-v1-fts", "ready")
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.MarkProjectionReady(ctx, tx, v1, domain.ProjectionFts, "rag-worker", "rev-v1-fts", nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	// v1 build is ready now, but Activate under fence=1 must be stale.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.Activate(ctx, tx, assetID, v1, 1, nil)
	assert.ErrorIs(t, err, domain.ErrCASVersionStale,
		"a late-completing old version must fail the monotonic fence (用例 22)")
	require.NoError(t, tx.Rollback(ctx))

	// §7 失败不覆盖: current_version_id still on v2, barrier still 2.
	current2, latest2 := currentVersionOf(t, pool, assetID)
	assert.Equal(t, v2, *current2, "stale version must not overwrite current_version_id")
	assert.Equal(t, int64(2), latest2, "barrier must not rewind")
}

// --- §10.3 用例 24: Activate without the expected_current the caller
// observed must be rejected (concurrent-activation detection) ---

// TestAsyncCAS_MissingExpectedCurrentRejected asserts: when current_version_id
// is already non-NULL (a version was activated), an Activate that passes
// expected_current=nil (acting as if it were the initial activation) must be
// rejected with ErrCASExpectedMismatch — the caller's view of the pointer is
// stale, a concurrent activation already moved it.
func TestAsyncCAS_MissingExpectedCurrentRejected(t *testing.T) {
	pool := casTestPool(t)
	wsID, userID, _, _, _ := seedCASWorkspace(t, pool)
	reg := NewAssetRegistry()
	ctx := context.Background()
	snapshot := `{"required_projections":["fts"]}`

	assetID, v1 := seedCandidateAsset(t, pool, wsID, userID, 1, "pending", "candidate", snapshot)
	addProjectionRow(t, pool, v1, "fts", "rag-worker", "rev-v1-fts", "ready")
	publishVersion(t, pool, v1)
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, reg.MarkProjectionReady(ctx, tx, v1, domain.ProjectionFts, "rag-worker", "rev-v1-fts", nil))
	require.NoError(t, tx.Commit(ctx))
	// Initial activation: expected_current=nil matches the NULL pointer.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, reg.Activate(ctx, tx, assetID, v1, 1, nil))
	require.NoError(t, tx.Commit(ctx))

	// A second version v2 is ready + published; the caller requesting it
	// observed current_version_id = v1 and would pass expected_current=&v1.
	// Here we simulate a STALE caller who passes expected_current=nil instead
	// — the CAS must reject because the pointer is no longer NULL.
	v2 := uuid.New()
	_, err = pool.Exec(ctx,
		`INSERT INTO knowledge_asset_versions
		   (id, asset_id, version_no, content_origin, dedupe_key,
		    build_status, governance_status, activation_policy_snapshot,
		    created_by_type, created_by_id)
		 VALUES ($1,$2,2,'generated',$3,'pending','published',$4::jsonb,'user',$5)`,
		v2, assetID, "asset_version:"+v2.String(), snapshot, userID)
	require.NoError(t, err)
	addProjectionRow(t, pool, v2, "fts", "rag-worker", "rev-v2-fts", "ready")
	// Bump the fence to 2 (the caller requesting v2 advanced the barrier).
	_, err = pool.Exec(ctx, `UPDATE knowledge_assets SET latest_requested_version_no=2 WHERE id=$1`, assetID)
	require.NoError(t, err)
	// Make v2 build-ready.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, reg.MarkProjectionReady(ctx, tx, v2, domain.ProjectionFts, "rag-worker", "rev-v2-fts", nil))
	require.NoError(t, tx.Commit(ctx))

	// Activate v2 with expected_current=nil → mismatch (current is v1, not NULL).
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	err = reg.Activate(ctx, tx, assetID, v2, 2, nil)
	assert.ErrorIs(t, err, domain.ErrCASExpectedMismatch,
		"a stale expected_current must be rejected (用例 24 concurrent-activation detection)")
	require.NoError(t, tx.Rollback(ctx))

	// And the correct expected_current=&v1 succeeds (proves the rejection was
	// the expected_current check, not the fence).
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, reg.Activate(ctx, tx, assetID, v2, 2, &v1))
	require.NoError(t, tx.Commit(ctx))
	current, _ := currentVersionOf(t, pool, assetID)
	assert.Equal(t, v2, *current, "with the correct expected_current the CAS switches to v2")
}
