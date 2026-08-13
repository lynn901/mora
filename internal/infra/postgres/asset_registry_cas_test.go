//go:build integration

// asset_registry_cas_test.go covers the §10.3 version atomic-switch invariants
// that ARE testable against the current SUT (design-docs/14 §10.3 用例 21,
// and the build_status/governance_status axis for 用例 23's invariant).
//
// These are integration tests gated on DATABASE_URL (same convention as the
// migration runner tests). They exercise AssetRegistry.RegisterDocumentAsset
// against a real Postgres with 013+014 schema applied — the CAS is a SQL
// UPDATE … WHERE clause, so the invariants only hold against the real engine.
//
// SUT SCOPE (what IS implemented and therefore testable here):
//  - RegisterDocumentAsset is the NATIVE-DOCUMENT dual-write path (§3.1).
//    It bumps latest_requested_version_no monotonically (GREATEST barrier) and
//    CAS-activates current_version_id under `WHERE latest_requested_version_no
//    = $3` — so an old version that completes LATE cannot overwrite a newer
//    current_version_id (§10.3 用例 21 "失败不覆盖 / 旧版本不切换").
//  - Native documents are stamped build_status='ready', governance_status=
//    'published' at write time; current_version_id therefore always points at
//    a usable version. The §10.3 用例 23 "candidate not returned" invariant
//    is enforced downstream by the authz usable-version gate
//    (build_status='ready' AND governance_status='published'); this test
//    pins that a candidate-stamped version is NOT surfaced as current.
//
// SUT GAP (what is NOT implemented — reported as a blocker, not faked):
//  - The async Connector-sourced CAS activation path is ErrNotWired: the
//    knowledge-worker's ProjectionBuildHandler / AssetActivateHandler are
//    stubs (return ErrNotWired), and AssetRegistry has no
//    MarkProjectionReady / Activate / ReconcileScan method. The §10.3 用例
//    20 (required projection missing → blocks ready), 用例 22 (stale version
//    CAS fails — mark ready only, no switch), and 用例 24 (rollback without
//    expected_current rejected) target exactly this async path and therefore
//    have no SUT to automate against. These three are listed as blocked in the
//    YS-111 progress comment pending the §6 CAS / §7 activate deliverable
//    landing in the worker (tracked against YS-110).
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
