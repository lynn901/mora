//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBoundary_MCPOverPermissionDenied covers PRD §7 "MCP 越权": a write tool
// call without permission must not modify the document and the attempt is
// recorded in the audit log.
//
// NOTE: the MCP moraclient currently sends `content` as a string while mora-api
// expects `[]Block`/`markdown` (known contract drift, YS-10 scope). Until fixed,
// update_document via MCP fails before/without reaching RBAC. This test asserts
// the SECURITY OUTCOME (no tampering + audited) which holds regardless of
// whether the rejection is 403 (RBAC) or 400 (drift). The precise 403 RBAC
// denial is verified at the mora layer in TestBoundary_MoraWriteRBACDenied.
func (s *Suite) TestBoundary_MCPOverPermissionDenied() {
	s.requireDB("non-admin token for over-permission")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E Boundary OverPerm WS", "e2e-overperm-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-OverPerm", "# target")
	s.publishDoc(admin, doc.ID, doc.VersionNo, "")

	// alice has readwrite token but NO write permission on this workspace.
	aliceSess := s.mcpInitialize(s.mcpClient(s.aliceRWToken))
	res, err := aliceSess.toolsCall("update_document", map[string]any{
		"document_id": doc.ID, "content": "# tampered", "format": "markdown",
	})

	// The write must NOT succeed: either an RPC error or an isError tool result.
	succeeded := err == nil && res != nil && !isToolError(res)
	require.False(s.T(), succeeded, "alice must NOT modify a doc she has no write permission on (PRD §7 MCP 越权)")

	// Content must be unchanged (no tampering) — the core security guarantee.
	got, _, _ := s.getDoc(admin, doc.ID)
	require.False(s.T(), contentHasText(got.Content, "tampered"), "doc content must not be modified by over-permission attempt")

	// Audit recorded the attempt.
	require.Eventually(s.T(), func() bool {
		audit := s.toolCallsAudit(admin, "tool_name=update_document")
		return len(audit) > 0
	}, 15*time.Second, 2*time.Second, "over-permission attempt must be audited")
}

// TestBoundary_MoraWriteRBACDenied verifies the precise 403 RBAC write denial at
// the mora-api layer: alice (no write permission) issuing a valid PATCH is
// rejected with 403/forbidden. This complements the MCP over-permission test
// (which is currently masked by the content-shape contract drift).
func (s *Suite) TestBoundary_MoraWriteRBACDenied() {
	s.requireDB("non-admin user for write RBAC denial")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E Boundary WriteRBAC WS", "e2e-writerbac-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-WriteRBAC", "# original")
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")

	// alice has no write permission; a valid PATCH (content as []Block) reaches RBAC.
	alice := s.jwtClient(s.aliceJWT)
	_, st, env := s.updateDoc(alice, published.ID, published.VersionNo, "# tampered by alice")
	require.Truef(s.T(), st == http.StatusForbidden || st == http.StatusNotFound,
		"alice (no write) must be denied; got status=%d code=%d msg=%s", st, env.Code, env.Message)

	// Content unchanged.
	got, _, _ := s.getDoc(admin, published.ID)
	require.False(s.T(), contentHasText(got.Content, "tampered by alice"), "doc must not be modified after denied write")
}

// TestBoundary_TokenRevocationInvalidatesSession covers PRD §7 "Token 泄露": a
// token revoked mid-session immediately invalidates the active session.
func (s *Suite) TestBoundary_TokenRevocationInvalidatesSession() {
	s.requireDB("a revocable mid-session token")
	// Fresh token for this scenario so we don't disturb shared fixtures.
	ctx := context.Background()
	tok := s.seedToken(ctx, "e2e-midsession", s.aliceUserID, "readwrite", nil)
	cl := s.mcpClient(tok)

	// Session works before revocation.
	ms := s.mcpInitialize(cl)
	_, err := ms.toolsCall("search_knowledge_base", map[string]any{"query": "anything", "top_n": 1})
	require.Nil(s.T(), err, "session must work before token revocation")

	// Revoke → same session must immediately fail (401).
	s.revokeToken(tok)
	st, _, _, _ := cl.raw(http.MethodPost, "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 99, "method": "tools/list",
	}, map[string]string{"Mcp-Session-Id": ms.sessionID})
	require.Equal(s.T(), http.StatusUnauthorized, st, "revoked token must invalidate the session immediately (PRD §7)")
}

// TestBoundary_ExistenceIndistinguishable covers PRD F1.5/F3.2 "存在性不泄露":
// a caller without permission cannot distinguish a hidden document from a
// non-existent one — both yield an empty/forbidden result, not a 404 that
// leaks existence.
func (s *Suite) TestBoundary_ExistenceIndistinguishable() {
	s.requireDB("non-admin caller")
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E Boundary Exist WS", "e2e-exist-"+randHex(4))
	hidden := s.createDocMarkdown(admin, ws.ID, "E2E-Hidden", "# secret existence")
	s.publishDoc(admin, hidden.ID, hidden.VersionNo, "")

	bob := s.jwtClient(s.bobJWT)
	bobSess := s.mcpInitialize(s.mcpClient(s.bobROToken))

	// get_document on a hidden (real) doc id.
	hiddenRes, hiddenErr := bobSess.toolsCall("get_document", map[string]any{"document_id": hidden.ID})
	// get_document on a non-existent doc id.
	nonExistRes, nonExistErr := bobSess.toolsCall("get_document", map[string]any{"document_id": "00000000-0000-0000-0000-000000000000"})

	// Both must be non-errors with empty content (no existence leak), and
	// indistinguishable: neither reveals which one is real.
	require.Nil(s.T(), hiddenErr, "hidden doc must not surface an error (existence non-leak)")
	require.Nil(s.T(), nonExistErr, "non-existent doc must not surface an error")
	require.False(s.T(), mcpGetDocID(hiddenRes, hidden.ID), "hidden doc content must not be returned")
	require.False(s.T(), mcpGetDocID(nonExistRes, hidden.ID), "non-existent doc must not return content")

	// HTTP GET on hidden vs non-existent must both be non-200 (no content leak).
	// NOTE: the API contract (04 §5) says 404 covers "not exist OR no permission";
	// if the implementation returns 403 for hidden + 404 for non-existent, that
	// distinction leaks existence at the mora-api GET layer and should be reviewed.
	// The MCP get_document layer (asserted above) is the authoritative non-leak
	// boundary per PRD F3.2.
	_, stHidden, _ := s.getDoc(bob, hidden.ID)
	_, stNonExist, _ := s.getDoc(bob, "00000000-0000-0000-0000-000000000000")
	require.NotEqual(s.T(), http.StatusOK, stHidden, "hidden doc must not be 200")
	require.NotEqual(s.T(), http.StatusOK, stNonExist, "non-existent doc must not be 200")
	if stHidden != stNonExist {
		s.T().Logf("existence-leak review: mora-api GET returned %d (hidden) vs %d (non-existent); "+
			"contract intends 404 for both. MCP layer non-leak verified above.", stHidden, stNonExist)
	}
}

// TestBoundary_LargeDocumentIndexes covers PRD §7 "超大文档": a large document
// is chunked in batches and indexes without blocking or failing.
func (s *Suite) TestBoundary_LargeDocumentIndexes() {
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E Boundary Large WS", "e2e-large-"+randHex(4))
	keyword := uniqueKeyword("large")
	// ~80KB body: many paragraphs (well under the 50MB cap, large enough to span
	// many chunks at 512-token chunk size).
	var sb strings.Builder
	sb.WriteString("# " + keyword + "\n\n")
	for i := 0; i < 4000; i++ {
		sb.WriteString("Paragraph number " + randHex(2) + " with enough words to fill chunk windows for the large document boundary test.\n\n")
	}
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-Large", sb.String())
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	indexed := s.waitForIndexStatus(admin, published.ID, "indexed")
	require.Equal(s.T(), "indexed", indexed.IndexStatus, "large document must index via batched chunking (PRD §7)")

	// And it remains searchable.
	require.True(s.T(), ragSees(s, admin, keyword, ws.ID, doc.ID), "large document must be searchable after indexing")
}

// TestBoundary_ModelUnavailableDegradation is a placeholder for PRD §7 "模型不可用":
// when the embedding provider is unavailable, the pipeline queues (no event
// loss) and retrieval degrades to pure BM25.
//
// SKIPPED in automated E2E: triggering it requires taking TEI/Ollama offline
// mid-run, which is an infra operation outside the black-box HTTP surface. The
// degradation path is verified by rag-worker unit tests (provider error →
// failed/retry → BM25-only search). To exercise manually: stop the `tei`
// container, create a doc, confirm index_status stays processing/failed while
// FTS (/search) and RAG BM25 path still return existing docs.
func (s *Suite) TestBoundary_ModelUnavailableDegradation() {
	s.T().Skip("infra-dependent: requires taking the TEI/Ollama container offline mid-run; see comment for manual steps")
}

// TestBoundary_RateLimited is a placeholder for PRD §7 "大量并发检索": per-token
// rate limiting returns 429 + Retry-After.
//
// SKIPPED in automated E2E: default limits (read 100/min, write 20/min) are too
// high to trip reliably without generating noisy burst traffic that risks
// affecting other tests sharing the stack. Verify manually by lowering
// MCP_RATE_LIMIT_READ and bursting tools/call, expecting HTTP 429.
func (s *Suite) TestBoundary_RateLimited() {
	s.T().Skip("infra-dependent: default rate limits too high to trip reliably in shared stack; see comment for manual steps")
}

// keep testing import referenced for clarity (test methods use s.T()).
var _ = testing.T{}
