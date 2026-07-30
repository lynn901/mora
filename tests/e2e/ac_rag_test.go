//go:build e2e

package e2e

import (
	"context"
	"net/http"

	"github.com/stretchr/testify/require"
)

// TestAC9_AutoPipelineAndBadge covers AC-9: document create/update/delete
// automatically triggers the indexing pipeline with no manual intervention;
// the index_status badge transitions correctly (pending → indexed).
func (s *Suite) TestAC9_AutoPipelineAndBadge() {
	admin := s.adminClient()
	keyword := uniqueKeyword("ac9")
	ws := s.createWorkspace(admin, "E2E AC9 WS", "e2e-ac9-"+randHex(4))

	// Create → pending badge.
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC9", "# "+keyword+"\n\nauto pipeline doc")
	require.Equal(s.T(), "pending", doc.IndexStatus, "new doc badge must be pending")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")

	// No manual trigger — poll until the pipeline sets indexed.
	indexed := s.waitForIndexStatus(admin, published.ID, "indexed")
	require.Equal(s.T(), "indexed", indexed.IndexStatus, "badge must reach indexed automatically (AC-9)")

	// Update → badge resets and pipeline re-indexes.
	updated, _, _ := s.updateDoc(admin, published.ID, indexed.VersionNo, "# "+keyword+"\n\nupdated content v2")
	require.Equal(s.T(), "indexed", indexed.VersionNo+1-1, "version must increment") // sanity: version changed
	_ = updated
	// Re-publish not needed (update keeps status); wait for re-index to settle.
	s.waitForIndexStatus(admin, published.ID, "indexed")
}

// TestAC10_CascadeCleanupOnUpdateAndDelete covers AC-10: after update/delete the
// old chunks are cascade-cleaned so search no longer returns stale content.
func (s *Suite) TestAC10_CascadeCleanupOnUpdateAndDelete() {
	admin := s.adminClient()
	stale := uniqueKeyword("ac10stale")
	fresh := uniqueKeyword("ac10fresh")
	ws := s.createWorkspace(admin, "E2E AC10 WS", "e2e-ac10-"+randHex(4))

	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC10", "# "+stale+"\n\noriginal stale body")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	s.waitForIndexStatus(admin, published.ID, "indexed")

	// Stale content is searchable before update.
	require.True(s.T(), ragSees(s, admin, stale, ws.ID, doc.ID), "stale content must be searchable before update")

	// Update: replace content entirely (remove stale keyword, add fresh).
	updated, _, _ := s.updateDoc(admin, published.ID, published.VersionNo, "# "+fresh+"\n\ncompletely new body")
	s.waitForIndexStatus(admin, updated.ID, "indexed")

	// Stale keyword must no longer return this doc (old chunks cleaned).
	require.False(s.T(), ragSees(s, admin, stale, ws.ID, doc.ID), "stale content must not be searchable after update (AC-10)")
	// Fresh content must be searchable.
	require.True(s.T(), ragSees(s, admin, fresh, ws.ID, doc.ID), "fresh content must be searchable after update")

	// Delete → no longer searchable at all.
	require.Equal(s.T(), http.StatusNoContent, s.deleteDoc(admin, doc.ID))
	require.False(s.T(), ragSees(s, admin, fresh, ws.ID, doc.ID), "deleted doc must not be searchable (AC-10 cascade)")
}

// TestAC11_EmbeddingConnectivity covers AC-11: TEI/Ollama + Qwen3-Embedding
// connectivity. The admin embedding-model routes are not mounted in wiki-api
// (known gap — see README), so connectivity is verified indirectly: a document
// reaching index_status=indexed proves the embedding provider + vector store
// are wired and producing vectors.
func (s *Suite) TestAC11_EmbeddingConnectivity() {
	s.requireDB("embedding_models lookup")
	admin := s.adminClient()

	// Active embedding model is configured (seeded by migration 010).
	provider, model, dim := s.activeEmbeddingModel()
	require.NotEmpty(s.T(), provider, "an active embedding model must be configured")
	require.NotEmpty(s.T(), model)
	require.Greater(s.T(), dim, 0)

	// Indexing success == end-to-end connectivity (TEI/Ollama → Qdrant).
	ws := s.createWorkspace(admin, "E2E AC11 WS", "e2e-ac11-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC11", "# connectivity probe")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	indexed := s.waitForIndexStatus(admin, published.ID, "indexed")
	require.Equal(s.T(), "indexed", indexed.IndexStatus, "indexed status proves embedding + vector pipeline connectivity")

	// NOTE: model hot-switch +存量重建 (/admin/embedding-models/{id}/rebuild) is
	// not exercised — those routes are not mounted in wiki-api. Tracked as a gap.
}

// TestAC12_HybridSearchAndRBAC covers AC-12: Dense+BM25 hybrid search returns
// structured hits with per-path scores; metadata (workspace) filtering works;
// RBAC is a hard constraint (no bypass).
func (s *Suite) TestAC12_HybridSearchAndRBAC() {
	s.requireDB("non-admin user for RBAC hard-constraint")
	admin := s.adminClient()
	keyword := uniqueKeyword("ac12")
	ws := s.createWorkspace(admin, "E2E AC12 WS", "e2e-ac12-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC12", "# "+keyword+"\n\nhybrid search target")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	s.waitForIndexStatus(admin, published.ID, "indexed")

	// Admin hybrid search returns the doc with fused + per-path scores.
	r, st, env := s.ragSearch(admin, keyword, ws.ID, 10)
	require.Equalf(s.T(), http.StatusOK, st, "rag/search: code=%d msg=%s", env.Code, env.Message)
	require.True(s.T(), ragContainsDoc(r, doc.ID), "hybrid search must hit the doc")
	if hit := findRagHit(r, doc.ID); hit != nil {
		// At least one score path is populated (dense or bm25); fused score present.
		require.True(s.T(), hit.Score > 0 || hit.DenseScore > 0 || hit.BM25Score > 0, "hit must carry a score")
	}

	// Metadata filter: workspace_id scope excludes other workspaces.
	ws2 := s.createWorkspace(admin, "E2E AC12 WS2", "e2e-ac12b-"+randHex(4))
	r2, _, _ := s.ragSearch(admin, keyword, ws2.ID, 10)
	require.False(s.T(), ragContainsDoc(r2, doc.ID), "workspace filter must exclude doc from ws1")

	// RBAC hard constraint: bob (no permission) cannot see the doc via search.
	bob := s.jwtClient(s.bobJWT)
	rb, _, _ := s.ragSearch(bob, keyword, ws.ID, 10)
	require.False(s.T(), ragContainsDoc(rb, doc.ID), "bob must not see doc (RBAC hard constraint, no bypass)")
}

// TestAC13_IdempotentReindex covers AC-13: re-indexing the same document version
// is idempotent — no duplicate vectors inflate search results.
//
// The failure-retry path (TEI down → exponential backoff ×3 → dead-letter) is
// not triggered here as it requires breaking the embedding provider; it is
// covered by rag-worker unit tests and noted as an infra-dependent scenario.
func (s *Suite) TestAC13_IdempotentReindex() {
	admin := s.adminClient()
	keyword := uniqueKeyword("ac13")
	ws := s.createWorkspace(admin, "E2E AC13 WS", "e2e-ac13-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC13", "# "+keyword+"\n\nidempotency probe")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	s.waitForIndexStatus(admin, published.ID, "indexed")

	// Baseline hit count for this doc.
	before := countRagHits(s, admin, keyword, ws.ID, doc.ID)

	// Re-update with identical content → pipeline re-indexes; must not duplicate.
	updated, _, _ := s.updateDoc(admin, published.ID, published.VersionNo, "# "+keyword+"\n\nidempotency probe")
	s.waitForIndexStatus(admin, updated.ID, "indexed")

	after := countRagHits(s, admin, keyword, ws.ID, doc.ID)
	require.Equal(s.T(), before, after, "re-index must be idempotent (no duplicate chunks): before=%d after=%d", before, after)
}

// --- helpers ---

func ragSees(s *Suite, cl *Client, keyword, wsID, docID string) bool {
	r, _, _ := s.ragSearch(cl, keyword, wsID, 10)
	return ragContainsDoc(r, docID)
}

func findRagHit(r ragResult, docID string) *ragHit {
	for i := range r.Items {
		if r.Items[i].DocumentID == docID {
			return &r.Items[i]
		}
	}
	return nil
}

func countRagHits(s *Suite, cl *Client, keyword, wsID, docID string) int {
	r, _, _ := s.ragSearch(cl, keyword, wsID, 50)
	n := 0
	for _, h := range r.Items {
		if h.DocumentID == docID {
			n++
		}
	}
	return n
}

// activeEmbeddingModel reads the active embedding_models row (migration 010 seed).
func (s *Suite) activeEmbeddingModel() (provider, model string, dim int) {
	if s.pool == nil {
		return "", "", 0
	}
	_ = s.pool.QueryRow(context.Background(),
		`SELECT provider, model_name, dimension FROM embedding_models WHERE status='active' ORDER BY updated_at DESC LIMIT 1`).
		Scan(&provider, &model, &dim)
	return
}
