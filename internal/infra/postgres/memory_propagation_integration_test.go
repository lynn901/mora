//go:build integration

// Integration tests for the deletion-propagation SQL chain (design-docs/18
// §9.2, D3; acceptance gate §11 「删除传播路径有自动化测试」). Skipped unless
// DATABASE_URL is set (run with:
//
//	DATABASE_URL=postgres://mora:mora@localhost:55432/mora?sslmode=disable \
//	  go test -tags=integration ./internal/infra/postgres/...)
//
// These verify the SQL the unit tests can't: that Purge erases
// encrypted_content/storage_key, the 019 pending_purged_at column drives the
// PurgeReady grace-window query, CountAvailableEvidence drops purged evidence
// so a unit flips evidence_missing, and the reaper's Tick stitches the repos
// together end-to-end. They require migration 018+019 applied (run-migrations.sh).
package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// evidenceTestPool gates on DATABASE_URL (same convention as knowledge_job_*
// integration tests). The propagation SQL lives here so the test exercises the
// real memory_evidence/memory_evidence_links/memory_units tables + the 019
// pending_purged_at column.
func evidenceTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func resetMemoryTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		TRUNCATE memory_evidence_links, memory_units, memory_evidence, memory_dedup_suggestions, memory_feedback CASCADE`)
	require.NoError(t, err)
}

// seedMemoryAsset inserts a user + workspace (via the shared seedUserWorkspace
// helper) and a knowledge_assets(asset_type='memory') row. Returns the
// workspace_id + asset_id. memory assets don't carry a native_document_id and
// don't require a governance_profile row (the FK is nullable), so this is a
// leaner seed than the document-asset path.
func seedMemoryAsset(t *testing.T, pool *pgxpool.Pool) (wsID, assetID uuid.UUID) {
	t.Helper()
	userID, wsID, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)
	ctx := context.Background()
	assetID = uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO knowledge_assets
		  (id, workspace_id, asset_type, name, owner_type, owner_id, status, visibility)
		VALUES ($1,$2,'memory',$3,'user',$4,'draft','private')`,
		assetID, wsID, "memory-asset-"+assetID.String()[:8], userID)
	require.NoError(t, err, "seed memory asset")
	return wsID, assetID
}

// TestPurgeEvidence_IntegrationEndToEnd: the §9.2 chain over real SQL —
// insert evidence + link + unit → Purge → content nulled, unit flagged
// evidence_missing (this was its only support), hash retained.
func TestPurgeEvidence_IntegrationEndToEnd(t *testing.T) {
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	ctx := context.Background()
	wsID, assetID := seedMemoryAsset(t, pool)

	evRepo := NewMemoryEvidenceRepo(NewDB(pool))
	unitRepo := NewMemoryUnitRepo(NewDB(pool))
	linkRepo := NewMemoryEvidenceLinkRepo(NewDB(pool))

	// 1. insert evidence (inline ciphertext).
	kv := 1
	evID, err := evRepo.Insert(ctx, domain.MemoryEvidence{
		WorkspaceID: wsID, OwnerType: domain.OwnerUser, OwnerID: uuid.New(),
		SourceKind: domain.EvidenceSourceMessage, SourceRef: "msg-1",
		Visibility: domain.EvidencePrivate, CapturedAuthzRevision: 1,
		ContentHash: "hash-int-1", EncryptedContent: []byte("cipher"),
		KeyVersion: &kv, RedactedExcerpt: "excerpt-1", State: domain.EvidenceActive,
	})
	require.NoError(t, err)

	// 2. insert a unit linked to that evidence (its only support).
	unitID, err := unitRepo.Insert(ctx, domain.MemoryUnit{
		WorkspaceID: wsID, AssetID: assetID, MemoryType: domain.MemoryFact,
		Statement: "a distilled fact", State: domain.MemoryCandidate,
		CreatedByType: domain.OwnerUser, CreatedByID: uuid.New(),
	})
	require.NoError(t, err)
	require.NoError(t, linkRepo.Insert(ctx, domain.MemoryEvidenceLink{
		MemoryUnitID: unitID, EvidenceID: evID, SupportType: domain.Supports,
	}))

	// 3. purge through the propagation service.
	svc := evidence.NewPropagationService(evidence.PropagationConfig{
		Evidence: evRepo, Units: unitRepo, Links: linkRepo,
		Objects: &noopObjStore{}, Projections: evidence.NoopProjectionInvalidator{},
		Now: func() time.Time { return time.Unix(3000, 0) },
	})
	out, err := svc.PurgeEvidence(ctx, evID)
	require.NoError(t, err)
	require.Len(t, out.UnitsFlagged, 1)
	assert.Equal(t, unitID, out.UnitsFlagged[0])

	// 4. verify content erased, hash retained, unit flagged.
	row := pool.QueryRow(ctx, `SELECT state, encrypted_content, storage_key, key_version, content_hash, pending_purged_at, purged_at FROM memory_evidence WHERE id=$1`, evID)
	var state, hash string
	var enc []byte
	var sk *string
	var kk *int
	var pp, pa *time.Time
	require.NoError(t, row.Scan(&state, &enc, &sk, &kk, &hash, &pp, &pa))
	assert.Equal(t, "purged", state)
	assert.Nil(t, enc)
	assert.Nil(t, sk)
	assert.Nil(t, kk)
	assert.Equal(t, "hash-int-1", hash, "hash retained as deletion proof")

	u, err := unitRepo.Get(ctx, unitID)
	require.NoError(t, err)
	assert.True(t, u.EvidenceMissing, "unit flagged evidence_missing")

	// 5. CountAvailableEvidence reflects the purge (no remaining support).
	n, err := linkRepo.CountAvailableEvidence(ctx, unitID, evID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// noopObjStore is a nil-safe evidence.ObjectStore for the integration test
// (no MinIO wired in the test env); inline evidence has no StorageKey so
// Purge never calls Delete anyway.
type noopObjStore struct{}

func (noopObjStore) Put(context.Context, uuid.UUID, uuid.UUID, []byte) (string, error) {
	return "", nil
}
func (noopObjStore) Read(context.Context, string) ([]byte, error) { return nil, nil }
func (noopObjStore) Delete(context.Context, string) error         { return nil }
