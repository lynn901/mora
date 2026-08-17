//go:build integration

// Phase 4 D3 evidence-purge CHECK integration test (design-docs/18 §8.4 / §2.1
// D4). DATABASE_URL-gated; skipped when unset. Run with:
//
//	DATABASE_URL=... go test -tags=integration ./test/integration/...
//
// This pins the defect that the unit tests (propagation_test.go /
// reaper_test.go) cannot catch: the 018 storage-split CHECK on
// memory_evidence is a DB-layer invariant the fake repos do not enforce.
// Purge (infra/postgres/memory_evidence.go:147) nulls encrypted_content +
// storage_key + key_version simultaneously and sets state='purged'. Under the
// 018 CHECK (unconditional XOR: exactly one of inline-ciphertext / large-object
// key must be set), a purged row satisfies NEITHER branch → CHECK violation on
// a real DB, so Purge would fail in production. Migration 020 narrows the
// CHECK to non-purged states (active/pending_purge) so a purged row with all
// three content columns NULL is legitimate (§8.4 "purged 后只保留
// id/hash/审计元数据").
//
// These tests prove, against a real PostgreSQL, that:
//   - an inline (encrypted_content) row can be Purge'd without a CHECK error;
//   - a large-object (storage_key) row can be Purge'd without a CHECK error;
//   - the post-purge row has state='purged', content columns NULL, and retains
//     content_hash + redacted_excerpt (the deletion-proof residue, §2.1 不变量);
//   - a pending_purge → purged transition (the reaper's second half) also passes
//     the CHECK.
// They are the end-to-end §9.2 SQL gate for the storage-split ↔ purge
// interaction; the unit tests cover the orchestration around it.
package integration

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// EvidencePurgeCheckSuite pins the 020 CHECK fix against the real DB.
type EvidencePurgeCheckSuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB
	repo evidence.EvidenceRepo
}

func TestEvidencePurgeCheckSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(EvidencePurgeCheckSuite))
}

func (s *EvidencePurgeCheckSuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.repo = postgres.NewMemoryEvidenceRepo(s.db)
}

func (s *EvidencePurgeCheckSuite) TearDownSuite() { s.pool.Close() }

func (s *EvidencePurgeCheckSuite) SetupTest() {
	ctx := context.Background()
	// clean in dependency order (children before parents); memory_* tables
	// only reference workspaces + knowledge_assets, both of which we seed
	// fresh per-test, but we wipe to keep suites independent.
	for _, t := range []string{
		"memory_dedup_suggestions", "memory_feedback",
		"memory_evidence_links", "memory_units", "memory_evidence",
	} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	// memory_retention_policies keeps its 018 system-default rows (per-workspace
	// seed); only delete non-system rows a test may insert.
	_, _ = s.pool.Exec(ctx, "DELETE FROM memory_retention_policies WHERE is_system = false")
	for _, t := range []string{"workspaces", "users"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
}

// seedWS inserts a workspace + owner user and returns their IDs (FK base for
// memory_evidence.workspace_id; owner_id is a free UUID column with no FK,
// but we use a real user id to stay consistent with production seeds).
func (s *EvidencePurgeCheckSuite) seedWS(slug, email string) (wsID, userID uuid.UUID) {
	ctx := context.Background()
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, email).Scan(&userID))
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, userID).Scan(&wsID))
	return wsID, userID
}

// insertRow seeds an evidence row through the real repo (the production write
// path) and returns its id. content is either inline ciphertext or a storage
// key depending on branch.
func (s *EvidencePurgeCheckSuite) insertRow(wsID, userID uuid.UUID, hash string, inline bool) uuid.UUID {
	ctx := context.Background()
	kv := 1
	e := domain.MemoryEvidence{
		WorkspaceID:           wsID,
		OwnerType:             domain.OwnerUser,
		OwnerID:               userID,
		SourceKind:            domain.EvidenceSourceSession,
		SourceRef:             "sess-" + hash,
		Visibility:            domain.EvidencePrivate,
		CapturedAuthzRevision: 1,
		ContentHash:           hash,
		RedactedExcerpt:       "redacted-" + hash,
		State:                 domain.EvidenceActive,
	}
	if inline {
		e.EncryptedContent = []byte("ciphertext-" + hash)
		e.KeyVersion = &kv
	} else {
		e.StorageKey = "mora-evidence/" + wsID.String() + "/" + hash
	}
	id, err := s.repo.Insert(ctx, e)
	require.NoError(s.T(), err, "Insert through real repo must succeed (CHECK holds for active)")
	return id
}

// assertPurgedRow loads the row via the real repo Get and asserts the post-
// purge invariant: state=purged, content columns nil/empty, hash + excerpt
// retained (the deletion-proof residue, §2.1 不变量 / §8.4).
func (s *EvidencePurgeCheckSuite) assertPurgedRow(id uuid.UUID, hash string) {
	ctx := context.Background()
	got, err := s.repo.Get(ctx, id)
	require.NoError(s.T(), err, "purged row must still be readable (not soft-deleted)")
	assert.Equal(s.T(), domain.EvidencePurged, got.State)
	assert.Nil(s.T(), got.EncryptedContent, "encrypted_content erased on purge")
	assert.Empty(s.T(), got.StorageKey, "storage_key erased on purge")
	assert.Nil(s.T(), got.KeyVersion, "key_version erased on purge")
	assert.Equal(s.T(), hash, got.ContentHash, "content_hash retained (deletion proof)")
	assert.Equal(s.T(), "redacted-"+hash, got.RedactedExcerpt, "redacted_excerpt retained")
	assert.NotNil(s.T(), got.PurgedAt, "purged_at stamped")
}

// TestPurge_InlineRowPassesCHECK: an inline (encrypted_content) active row
// Purge'd through the real repo must not trip the 018 CHECK. This is the
// defect case — pre-020 it failed with SQLSTATE 23514.
func (s *EvidencePurgeCheckSuite) TestPurge_InlineRowPassesCHECK() {
	wsID, userID := s.seedWS("inline", "inline@example.com")
	id := s.insertRow(wsID, userID, "h-inline", true)

	require.NoError(s.T(), s.repo.Purge(context.Background(), id),
		"Purge must not trigger the storage-split CHECK (020 narrows it to non-purged)")
	s.assertPurgedRow(id, "h-inline")
}

// TestPurge_LargeObjectRowPassesCHECK: a storage_key active row Purge'd
// through the real repo must not trip the CHECK either (both branches null out).
func (s *EvidencePurgeCheckSuite) TestPurge_LargeObjectRowPassesCHECK() {
	wsID, userID := s.seedWS("largeobj", "largeobj@example.com")
	id := s.insertRow(wsID, userID, "h-lo", false)

	require.NoError(s.T(), s.repo.Purge(context.Background(), id),
		"Purge on a large-object row must not trigger the storage-split CHECK")
	s.assertPurgedRow(id, "h-lo")
}

// TestPurge_PendingPurgeTransition: the reaper's second half (active →
// pending_purge → purged) must pass the CHECK at the final erase step.
func (s *EvidencePurgeCheckSuite) TestPurge_PendingPurgeTransition() {
	wsID, userID := s.seedWS("pending", "pending@example.com")
	id := s.insertRow(wsID, userID, "h-pend", true)

	// first half: active → pending_purge (content still present, CHECK holds)
	require.NoError(s.T(), s.repo.MarkPendingPurge(context.Background(), id))
	got, err := s.repo.Get(context.Background(), id)
	require.NoError(s.T(), err)
	require.Equal(s.T(), domain.EvidencePendingPurge, got.State)
	require.NotNil(s.T(), got.PendingPurgedAt, "pending_purged_at stamped")

	// second half: pending_purge → purged (content nulled) — the defect step
	require.NoError(s.T(), s.repo.Purge(context.Background(), id),
		"pending_purge → purged erase must not trigger the storage-split CHECK")
	s.assertPurgedRow(id, "h-pend")
}

// TestInsert_RejectsBothBranchesSet: the 020 CHECK still enforces D4's
// either/or for non-purged states — a row with BOTH encrypted_content and
// storage_key set must be rejected (SQLSTATE 23514). This guards against
// accidentally widening the constraint too far when narrowing it for purged.
func (s *EvidencePurgeCheckSuite) TestInsert_RejectsBothBranchesSet() {
	wsID, userID := s.seedWS("both", "both@example.com")
	ctx := context.Background()
	kv := 1
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memory_evidence
		  (workspace_id, owner_type, owner_id, source_kind, source_ref,
		   visibility, captured_authz_revision, content_hash,
		   encrypted_content, storage_key, key_version, redacted_excerpt, state)
		VALUES ($1,'user',$2,'session','sess-both','private',1,'h-both',
		        $3,$4,$5,'redacted-h-both','active')`,
		wsID, userID, []byte("cipher"), "mora-evidence/both", &kv)
	require.Error(s.T(), err, "both branches set must violate the storage-split CHECK")
	assert.Contains(s.T(), err.Error(), "storage_split_check",
		"violation must come from the 020 named constraint")
}

// TestInsert_RejectsNeitherBranchSet: an active row with NEITHER
// encrypted_content NOR storage_key set must still be rejected — the
// narrowing only exempts the purged state, not active.
func (s *EvidencePurgeCheckSuite) TestInsert_RejectsNeitherBranchSet() {
	wsID, userID := s.seedWS("neither", "neither@example.com")
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO memory_evidence
		  (workspace_id, owner_type, owner_id, source_kind, source_ref,
		   visibility, captured_authz_revision, content_hash,
		   encrypted_content, storage_key, key_version, redacted_excerpt, state)
		VALUES ($1,'user',$2,'session','sess-neither','private',1,'h-neither',
		        NULL,NULL,NULL,'redacted-h-neither','active')`,
		wsID, userID)
	require.Error(s.T(), err, "active row with no content branch must violate the CHECK")
	assert.Contains(s.T(), err.Error(), "storage_split_check")
}

// TestPurge_PurgedRowExemptFromCHECK: a directly-inserted purged row (all
// content NULL, state=purged — the post-erase residue shape) must be INSERT-
// legal. This is the shape Purge leaves behind; the 020 narrowing makes it
// valid where the 018 unconditional CHECK would reject it.
func (s *EvidencePurgeCheckSuite) TestPurge_PurgedRowExemptFromCHECK() {
	wsID, userID := s.seedWS("residue", "residue@example.com")
	ctx := context.Background()
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO memory_evidence
		  (workspace_id, owner_type, owner_id, source_kind, source_ref,
		   visibility, captured_authz_revision, content_hash,
		   encrypted_content, storage_key, key_version, redacted_excerpt, state)
		VALUES ($1,'user',$2,'session','sess-residue','private',1,'h-residue',
		        NULL,NULL,NULL,'redacted-h-residue','purged') RETURNING id`,
		wsID, userID).Scan(&id)
	require.NoError(s.T(), err, "a purged row with all content NULL must be CHECK-legal (020 exempts purged)")
}
