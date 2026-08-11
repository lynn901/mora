//go:build integration

// Integration tests for the legacy-document online migration runner
// (design-docs/14 §3.2 backfill, §3.3 reconciliation). Skipped unless
// DATABASE_URL is set (run with:
// DATABASE_URL=... go test -tags=integration ./internal/module/knowledge/migration/...).
//
// These verify the protocol's hard guarantees (§13 验收门禁):
//   - 存量文档 100% 登记为 Document Asset；不复制正文；
//   - dedupe_key 幂等无重复（backfill 重跑 no-op）；
//   - current_version_id 与 documents.version_no 一致。
package migration

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// migrationTestPool gates on DATABASE_URL (same convention as the sink/job tests).
func migrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// seedMigrationWorkspace inserts a user + workspace + service account, returns
// (userID, wsID, saID, cleanup). The service account is the §3.4 migration
// approver.
func seedMigrationWorkspace(t *testing.T, pool *pgxpool.Pool) (userID, wsID, saID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	userID = uuid.New()
	wsID = uuid.New()
	saID = uuid.New()
	email := "mig_" + userID.String()[:8] + "@mora.local"
	_, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, name, status) VALUES ($1,$2,'Mig Test','active')`, userID, email)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1,'Mig WS',$2,$3)`,
		wsID, "mig-"+wsID.String()[:8], userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO service_accounts (id, name, description) VALUES ($1,'mora-legacy-migration-test','test')`, saID)
	require.NoError(t, err)
	return userID, wsID, saID, func() {
		// workspaces cascade-deletes documents/document_versions/knowledge_*;
		// users/service_accounts cleaned up explicitly.
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, wsID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM service_accounts WHERE id=$1`, saID)
	}
}

// seedLegacyDoc inserts a documents row at version_no=N plus N document_versions
// rows — the "existing document written before Phase 1" that backfill must
// register. Returns the document id and the version ids (v1..vN).
func seedLegacyDoc(t *testing.T, pool *pgxpool.Pool, wsID, userID uuid.UUID, versions int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	docID := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO documents (id, workspace_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, parse_status)
		VALUES ($1,$2,'legacy','["p",{"t":"secret"}]'::jsonb,'secret','blocks','published','pending',$3,$4,$4,'parsed')`,
		docID, wsID, versions, userID)
	require.NoError(t, err)
	vids := make([]uuid.UUID, 0, versions)
	for vno := 1; vno <= versions; vno++ {
		vid := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO document_versions (id, document_id, version_no, content, content_text, author_id)
			VALUES ($1,$2,$3,'["p",{"t":"secret"}]'::jsonb,'secret',$4)`, vid, docID, vno, userID)
		require.NoError(t, err)
		vids = append(vids, vid)
	}
	return docID, vids
}

// TestBackfill_RegistersAllDocuments_NoContent: backfill registers every
// existing document as a Document asset, dedupe_key is unique, and content is
// NOT copied (§3.2, §3.3 不复制正文).
func TestBackfill_RegistersAllDocuments_NoContent(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	userID, wsID, saID, cleanup := seedMigrationWorkspace(t, pool)
	t.Cleanup(cleanup)

	// Three legacy docs, 1-3 versions each. §3.2 backfill registers each
	// document's CURRENT version (v.version_no = d.version_no) — 3 versions.
	// The historical versions (v1 of the 2-version doc; v1,v2 of the 3-version
	// doc) are registered by the §3.3 reconcile scan, not by backfill.
	seedLegacyDoc(t, pool, wsID, userID, 1)
	seedLegacyDoc(t, pool, wsID, userID, 3)
	seedLegacyDoc(t, pool, wsID, userID, 2)

	runner := newTestRunner(t, pool, saID)
	n, err := runner.BackfillWorkspace(ctx, wsID)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "backfill registers one (current) version per document (§3.2)")

	// Reconcile fills in the 3 historical versions (§3.3 row 2).
	rep, err := runner.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, rep.MissingVersions, "reconcile registers the missing historical versions")

	// Every document has exactly one asset; every document version has exactly
	// one asset version; no dedupe_key duplicates.
	var docCount, missingAssets, missingVersions, dupDedupe int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM documents WHERE workspace_id=$1 AND status!='deleted'`, wsID).Scan(&docCount))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM documents d
		LEFT JOIN knowledge_assets a ON a.native_document_id=d.id AND a.asset_type='document'
		WHERE d.workspace_id=$1 AND a.id IS NULL`, wsID).Scan(&missingAssets))
	assert.Equal(t, 0, missingAssets, "every document must have an asset (§13)")
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM document_versions v
		JOIN documents d ON d.id=v.document_id
		LEFT JOIN knowledge_asset_versions kav ON kav.native_document_version_id=v.id
		WHERE d.workspace_id=$1 AND kav.id IS NULL`, wsID).Scan(&missingVersions))
	assert.Equal(t, 0, missingVersions, "every document version must have an asset version")
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT dedupe_key FROM knowledge_asset_versions kav
		  JOIN knowledge_assets a ON a.id=kav.asset_id
		  WHERE a.workspace_id=$1
		  GROUP BY dedupe_key HAVING count(*)>1
		) sub`, wsID).Scan(&dupDedupe))
	assert.Equal(t, 0, dupDedupe, "dedupe_key must be unique (§13)")

	// Content NOT copied: every asset version's generation_ref/provider_ref NULL.
	var withGenRef int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_asset_versions kav
		JOIN knowledge_assets a ON a.id=kav.asset_id
		WHERE a.workspace_id=$1 AND (generation_ref IS NOT NULL OR provider_ref IS NOT NULL)`, wsID).Scan(&withGenRef))
	assert.Equal(t, 0, withGenRef, "no content copied into asset versions (§3.3)")

	// legacy_migration review recorded for each version (§3.4).
	var reviewN int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM review_requests rr
		JOIN knowledge_assets a ON a.id=rr.asset_id
		WHERE a.workspace_id=$1 AND rr.status='approved' AND rr.rationale='legacy_migration backfill'`, wsID).Scan(&reviewN))
	assert.Equal(t, 6, reviewN, "every backfilled version has a §3.4 system approval")
}

// TestBackfill_Idempotent_Rerun: re-running backfill over an already-migrated
// workspace registers nothing new (dedupe_key idempotency).
func TestBackfill_Idempotent_Rerun(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	userID, wsID, saID, cleanup := seedMigrationWorkspace(t, pool)
	t.Cleanup(cleanup)

	seedLegacyDoc(t, pool, wsID, userID, 2)
	runner := newTestRunner(t, pool, saID)

	// §3.2 backfill registers the document's CURRENT version (v2) — 1 row.
	n1, err := runner.BackfillWorkspace(ctx, wsID)
	require.NoError(t, err)
	require.Equal(t, 1, n1)

	// Re-run — must be a no-op (the current version is already registered).
	n2, err := runner.BackfillWorkspace(ctx, wsID)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "re-run must register nothing (dedupe_key idempotent)")
}

// TestReconcile_ConvergesCurrentVersion: after backfill, a §3.3 row-3 drift
// (document's version_no advanced but the asset's current_version_id did not
// follow — e.g. a crash between the doc write and CAS activation, or a v2
// version registered by a path that did not activate) is repaired by reconcile
// so current_version_id points at the document's current version.
func TestReconcile_ConvergesCurrentVersion(t *testing.T) {
	pool := migrationTestPool(t)
	ctx := context.Background()
	userID, wsID, saID, cleanup := seedMigrationWorkspace(t, pool)
	t.Cleanup(cleanup)

	docID, _ := seedLegacyDoc(t, pool, wsID, userID, 1)
	runner := newTestRunner(t, pool, saID)
	_, err := runner.BackfillWorkspace(ctx, wsID)
	require.NoError(t, err)

	// Read the v1 asset so we can build the drift.
	var assetID, v1AssetVID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id, current_version_id FROM knowledge_assets WHERE native_document_id=$1`, docID).Scan(&assetID, &v1AssetVID))

	// Simulate drift: the document advances to version 2 with a new
	// document_version, AND the v2 asset version is registered (as if a
	// producer wrote it) but current_version_id is NOT advanced — left pointing
	// at v1. Reconcile's §3.3 row-3 scan must repair the pointer.
	vid2 := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO document_versions (id, document_id, version_no, content, content_text, author_id)
		VALUES ($1,$2,2,'[]'::jsonb,'v2',$3)`, vid2, docID, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE documents SET version_no=2 WHERE id=$1`, docID)
	require.NoError(t, err)

	// Register v2's asset version directly, bypassing the registry's CAS so the
	// pointer stays at v1 (the drift state). latest_requested_version_no is
	// advanced to 2 (a real registration would), but current_version_id stays
	// at v1AssetVID.
	_, err = pool.Exec(ctx, `
		INSERT INTO knowledge_asset_versions
		  (asset_id, version_no, native_document_version_id, content_origin,
		   dedupe_key, build_status, governance_status, activation_policy_snapshot,
		   created_by_type, created_by_id)
		VALUES ($1,2,$2,'human',$3,'ready','published','{}'::jsonb,'service_account',$4)`,
		assetID, vid2, "document_version:"+vid2.String(), saID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`UPDATE knowledge_assets SET latest_requested_version_no=2 WHERE id=$1`, assetID)
	require.NoError(t, err)
	// Confirm the drift: pointer at v1 while document is at v2.
	var curVID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT current_version_id FROM knowledge_assets WHERE id=$1`, assetID).Scan(&curVID))
	require.Equal(t, v1AssetVID, curVID, "precondition: pointer stale at v1")

	// Reconcile repairs the stale pointer (§3.3 row 3).
	rep, err := runner.Reconcile(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, rep.VersionMismatches, 1, "current_version_id mismatch repaired")

	// After reconcile: current_version_id points at the v2 asset version.
	var curPointsAtV2 bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM knowledge_assets a
		  JOIN knowledge_asset_versions kav ON kav.id=a.current_version_id
		  WHERE a.native_document_id=$1 AND kav.native_document_version_id=$2
		)`, docID, vid2).Scan(&curPointsAtV2))
	assert.True(t, curPointsAtV2, "current_version_id must resolve to the document's current version_no")

	// And globally: every published document asset's current_version_id matches
	// the document's version_no (§13 验收门禁).
	var mismatches int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM knowledge_assets a
		JOIN documents d ON d.id=a.native_document_id AND a.asset_type='document'
		LEFT JOIN knowledge_asset_versions kav ON kav.id=a.current_version_id
		LEFT JOIN document_versions vt ON vt.id=kav.native_document_version_id
		WHERE a.workspace_id=$1 AND a.status='published'
		  AND vt.version_no IS DISTINCT FROM d.version_no`, wsID).Scan(&mismatches))
	assert.Equal(t, 0, mismatches, "no current_version_id/version_no mismatches after reconcile")
}

// newTestRunner builds a Runner over the test pool with a fresh registry + outbox.
func newTestRunner(t *testing.T, pool *pgxpool.Pool, saID uuid.UUID) *Runner {
	t.Helper()
	return NewRunner(pool, postgres.NewAssetRegistry(), outbox.NewStore(), Options{
		MigrationServiceAccountID: saID,
	}, nil)
}
