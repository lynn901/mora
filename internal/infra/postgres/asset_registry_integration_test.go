//go:build integration

// Integration tests for the Phase 1 asset registry, dual-write, backfill, and
// reconciliation (design-docs/14 §3.1–§3.4). Skipped unless DATABASE_URL is set
// (run with: DATABASE_URL=... go test -tags=integration ./internal/infra/postgres/...).
//
// These verify the SQL contract unit tests can't: that the 014 migration's
// tables + constraints hold, that RegisterDocumentAsset is idempotent across
// the dual-write and backfill paths, that the DocWriteSink dual-write lands
// doc + version + asset + outbox atomically (and stamps asset_id/version_id
// into the payload), and that backfill→reconcile converges current_version_id
// to documents.version_no without ever copying content.
package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// registryTestPool reuses the sink test pool (same DATABASE_URL gate).
func registryTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return sinkTestPool(t)
}

// TestAssetRegistry_RegisterDocumentAsset_Idempotent: registering the same
// document version twice returns the same asset/version ids with Created=false
// on the second call — the dedupe_key UNIQUE + (asset_id,
// native_document_version_id) guards (§3.1 不变量).
func TestAssetRegistry_RegisterDocumentAsset_Idempotent(t *testing.T) {
	pool := registryTestPool(t)
	reg := NewAssetRegistry()
	ctx := context.Background()

	userID, wsID, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	// Seed a document + a document_version (as the native doc the asset refers
	// to). DocWriteSink does this in its own tx; here we do it directly.
	docID, versionID := seedNativeDoc(t, pool, wsID, userID, 1, "idempotent doc")

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck — guard so a failed require never leaks the conn
	r1, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: versionID,
		VersionNo: 1, Title: "idempotent doc",
		CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	assert.True(t, r1.Created, "first registration must create")

	// Second registration of the SAME version — idempotent.
	tx2, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx2.Rollback(ctx) //nolint:errcheck
	r2, err := reg.RegisterDocumentAsset(ctx, tx2, asset.Registration{
		DocumentID: docID, WorkspaceID: wsID, VersionID: versionID,
		VersionNo: 1, Title: "idempotent doc",
		CreatedByType: domain.SubjectUser, CreatedByID: userID,
	})
	require.NoError(t, err, "second registration (idempotent re-register)")
	require.NoError(t, tx2.Commit(ctx))
	assert.False(t, r2.Created, "re-registration must not create")
	assert.Equal(t, r1.AssetID, r2.AssetID)
	assert.Equal(t, r1.AssetVersionID, r2.AssetVersionID)

	// Exactly one asset row and one asset version row for this document.
	var assetN, versionN int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_assets WHERE native_document_id=$1 AND asset_type='document'`, docID).Scan(&assetN))
	assert.Equal(t, 1, assetN, "exactly one asset per document")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_asset_versions WHERE native_document_version_id=$1`, versionID).Scan(&versionN))
	assert.Equal(t, 1, versionN, "exactly one asset version per document version")
}

// TestAssetRegistry_RegisterMultipleVersions: registering versions 1..N of the
// same document creates one asset and N versions, and the asset's
// current_version_id advances to the latest (§6.4 CAS activation).
func TestAssetRegistry_RegisterMultipleVersions(t *testing.T) {
	pool := registryTestPool(t)
	reg := NewAssetRegistry()
	ctx := context.Background()

	userID, wsID, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	docID := uuid.New()
	seedDocRow(t, pool, docID, wsID, userID)

	var lastAssetID, lastVersionID uuid.UUID
	for vno := 1; vno <= 3; vno++ {
		vid := uuid.New()
		seedVersionRow(t, pool, vid, docID, vno, userID)
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx) //nolint:errcheck — guard so a failed require never leaks the conn
		res, err := reg.RegisterDocumentAsset(ctx, tx, asset.Registration{
			DocumentID: docID, WorkspaceID: wsID, VersionID: vid,
			VersionNo: int64(vno), Title: "multi-version doc",
			CreatedByType: domain.SubjectUser, CreatedByID: userID,
		})
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		assert.True(t, res.Created)
		lastAssetID = res.AssetID
		lastVersionID = res.AssetVersionID
	}

	// One asset, three versions, current_version_id = version 3.
	var assetN, versionN int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_assets WHERE native_document_id=$1`, docID).Scan(&assetN))
	assert.Equal(t, 1, assetN)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_asset_versions WHERE asset_id=$1`, lastAssetID).Scan(&versionN))
	assert.Equal(t, 3, versionN)

	var curVID *uuid.UUID
	var latestNo int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT current_version_id, latest_requested_version_no FROM knowledge_assets WHERE id=$1`, lastAssetID).Scan(&curVID, &latestNo))
	require.NotNil(t, curVID)
	assert.Equal(t, lastVersionID, *curVID, "current_version_id must point at the latest version")
	assert.Equal(t, int64(3), latestNo)
}

// TestDocWriteSink_DualWrite_Registry: with an asset.Registry wired, WriteDoc
// (create) lands document + version + knowledge_asset + knowledge_asset_version
// + outbox in ONE tx, and the outbox payload carries asset_id + version_id
// (§3.1). Content is NOT copied into the asset version (§3.3).
func TestDocWriteSink_DualWrite_Registry(t *testing.T) {
	pool := registryTestPool(t)
	sink := NewDocWriteSink(pool, outbox.NewStore()).WithRegistry(NewAssetRegistry())
	ctx := context.Background()

	author, ws, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	doc := &domain.Document{
		ID: uuid.New(), WorkspaceID: ws, Title: "dual-write",
		Content: []domain.Block{{Type: "p", Text: "secret-content"}}, Format: "markdown",
		Status: domain.StatusDraft, IndexStatus: domain.IndexPending,
		ParseStatus: domain.ParseParsed, CreatedBy: author,
	}
	doc.UpdatedBy = &doc.CreatedBy
	ver := &domain.DocumentVersion{
		DocumentID: doc.ID, VersionNo: 1, Content: doc.Content,
		ContentText: "secret-content", AuthorID: author, DiffSummary: "initial",
	}
	ev := domain.KnowledgeEvent{
		EventType: domain.KEAssetCreated, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &ws,
	}

	out, err := sink.WriteDoc(ctx, doc, ver, 0, true, ev)
	require.NoError(t, err)
	assert.Equal(t, 1, out.VersionNo)

	// Asset + version registered.
	var assetID, versionRowID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM knowledge_assets WHERE native_document_id=$1 AND asset_type='document'`, doc.ID).Scan(&assetID))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM knowledge_asset_versions WHERE native_document_version_id=$1`, ver.ID).Scan(&versionRowID))

	// Content NOT copied: the asset version's only content reference is
	// native_document_version_id; generation_ref/provider_ref are NULL.
	var genRef, provRef []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT generation_ref, provider_ref FROM knowledge_asset_versions WHERE id=$1`, versionRowID).Scan(&genRef, &provRef))
	assert.Nil(t, genRef, "generation_ref must be NULL for native documents (§3.3)")
	assert.Nil(t, provRef, "provider_ref must be NULL for native documents (§3.3)")

	// Outbox payload carries asset_id + asset_version_id (§3.1). The outbox
	// event's aggregate_id is the document id (the §13.6 envelope AggregateID),
	// while the asset ids are stamped into the payload by the dual-write sink.
	var payload []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT payload FROM outbox_events WHERE aggregate_id=$1`, doc.ID).Scan(&payload))
	assertContains(t, string(payload), assetID.String())
	assertContains(t, string(payload), versionRowID.String())
	// And still no content leaked into the event (§5.1).
	assertNotInPayload(t, payload, "secret-content")
}

// TestDocWriteSink_DualWrite_RollbackOnConflict: a CAS conflict must roll back
// the asset registration too — no orphan asset/version row (§3.1 不变量: 不出现
// 有文档无资产/有资产无文档的中间态). Extends the Phase 0 rollback test.
func TestDocWriteSink_DualWrite_RollbackOnConflict(t *testing.T) {
	pool := registryTestPool(t)
	sink := NewDocWriteSink(pool, outbox.NewStore()).WithRegistry(NewAssetRegistry())
	ctx := context.Background()

	author, ws, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	// Seed a doc at v1 via the dual-write sink.
	doc := &domain.Document{
		ID: uuid.New(), WorkspaceID: ws, Title: "dual-rb",
		Content: []domain.Block{{Type: "p", Text: "v1"}}, Format: "markdown",
		Status: domain.StatusDraft, IndexStatus: domain.IndexPending,
		ParseStatus: domain.ParseParsed, CreatedBy: author,
	}
	doc.UpdatedBy = &doc.CreatedBy
	_, err := sink.WriteDoc(ctx, doc, &domain.DocumentVersion{
		DocumentID: doc.ID, VersionNo: 1, Content: doc.Content, AuthorID: author,
	}, 0, true, domain.KnowledgeEvent{
		EventType: domain.KEAssetCreated, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &ws,
	})
	require.NoError(t, err)

	// Attempt v2 with a WRONG prevVersion (CAS miss) → whole tx rolls back,
	// including the would-be asset/version rows for v2.
	upd := *doc
	upd.Title = "dual-rb-v2"
	upd.Content = []domain.Block{{Type: "p", Text: "v2"}}
	_, err = sink.WriteDoc(ctx, &upd, &domain.DocumentVersion{
		DocumentID: doc.ID, VersionNo: 2, Content: upd.Content, AuthorID: author,
	}, 99, false, domain.KnowledgeEvent{
		EventType: domain.KEAssetVersionRequested, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &ws,
	})
	require.ErrorIs(t, err, service.ErrNotFound)

	// Still exactly ONE asset version (the v1 one); no orphan v2 asset version.
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_asset_versions kav
		 JOIN knowledge_assets a ON a.id = kav.asset_id
		 WHERE a.native_document_id=$1`, doc.ID).Scan(&n))
	assert.Equal(t, 1, n, "CAS conflict must roll back the asset version insert")
}

// --- helpers ---

// seedNativeDoc inserts a documents row + its current document_versions row and
// returns (docID, versionID). Used by tests that need a native doc to register.
func seedNativeDoc(t *testing.T, pool *pgxpool.Pool, wsID, userID uuid.UUID, vno int, title string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	docID := uuid.New()
	seedDocRow(t, pool, docID, wsID, userID)
	vid := uuid.New()
	seedVersionRow(t, pool, vid, docID, vno, userID)
	// point documents.version_no at the version
	_, err := pool.Exec(ctx, `UPDATE documents SET title=$2, version_no=$3 WHERE id=$1`, docID, title, vno)
	require.NoError(t, err)
	return docID, vid
}

func seedDocRow(t *testing.T, pool *pgxpool.Pool, docID, wsID, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO documents (id, workspace_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, parse_status)
		VALUES ($1,$2,'seed','[]'::jsonb,'','blocks','draft','pending',1,$3,$3,'parsed')`, docID, wsID, userID)
	require.NoError(t, err)
}

func seedVersionRow(t *testing.T, pool *pgxpool.Pool, vid, docID uuid.UUID, vno int, authorID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO document_versions (id, document_id, version_no, content, content_text, author_id)
		VALUES ($1,$2,$3,'[]'::jsonb,'',$4)`, vid, docID, vno, authorID)
	require.NoError(t, err)
}

// assertContains fails if needle is not in haystack.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !stringContains(haystack, needle) {
		t.Errorf("expected %q in %s", needle, haystack)
	}
}
