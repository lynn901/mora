//go:build e2e

package e2e

import (
	"net/http"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRBACCrossLayerConsistency verifies PRD §7 / §4.3.3: a permission change
// in the Mora layer propagates so that retrieval (FTS + RAG) and MCP results
// converge to the same visibility for the affected principal.
//
// Flow:
//  1. create doc in a directory, publish, index
//  2. grant alice read on the directory (subtree) → alice sees doc in FTS, RAG, MCP
//  3. revoke the grant → alice no longer sees doc in FTS (immediate, live RBAC),
//     and RAG + MCP converge within a window (visible_to recompute / defensive re-check)
//  4. re-grant → alice sees doc again (convergence both directions)
//
// Covers AC-7, AC-8, AC-12, AC-18 (cross-layer RBAC consistency).
func (s *Suite) TestRBACCrossLayerConsistency() {
	s.requireDB("non-admin user + MCP token for cross-layer RBAC")

	admin := s.adminClient()
	keyword := uniqueKeyword("rbacxlayer")

	ws := s.createWorkspace(admin, "E2E RBAC XLayer WS", "e2e-rbacxl-"+randHex(4))
	dir := s.createDirectory(admin, ws.ID, "", "Engineering")

	doc := s.createDocInDir(admin, ws.ID, dir.ID, "E2E-RBAC-XLayer",
		"# "+keyword+"\n\nCross-layer RBAC convergence test document.")
	require.Equal(s.T(), "pending", doc.IndexStatus)
	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	s.waitForIndexStatus(admin, published.ID, "indexed")

	alice := s.jwtClient(s.aliceJWT)
	bob := s.jwtClient(s.bobJWT)

	// Baseline: neither alice nor bob can see the doc (no grants).
	require.False(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "alice must NOT see doc before grant")
	require.False(s.T(), ftsSees(s, bob, ws.ID, keyword, doc.ID), "bob must NOT see doc")

	// --- Grant alice read on the directory (subtree inherits to doc) ---
	grant := s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "directory", dir.ID, "allow")

	// FTS converges immediately (live RBAC at SQL layer).
	require.True(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "alice must see doc after directory grant (FTS)")

	// MCP (Qdrant visible_to) converges within a window.
	aliceSess := s.mcpInitialize(s.mcpClient(s.aliceRWToken))
	require.Eventually(s.T(), func() bool {
		return mcpSearchSees(s, aliceSess, keyword, ws.ID, doc.ID)
	}, 60*time.Second, 3*time.Second, "alice MCP search must converge to visible after grant")

	// bob still must not see.
	require.False(s.T(), mcpSearchSees(s, s.mcpInitialize(s.mcpClient(s.bobROToken)), keyword, ws.ID, doc.ID),
		"bob must NOT see doc in MCP after alice-only grant")

	// --- Revoke the grant → alice must drop out of all layers ---
	s.revokePermission(admin, grant.ID)

	// FTS drops immediately.
	require.False(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "alice must NOT see doc after revoke (FTS)")

	// New MCP session after revoke: search must converge to invisible.
	aliceSess2 := s.mcpInitialize(s.mcpClient(s.aliceRWToken))
	require.Eventually(s.T(), func() bool {
		return !mcpSearchSees(s, aliceSess2, keyword, ws.ID, doc.ID)
	}, 60*time.Second, 3*time.Second, "alice MCP search must converge to invisible after revoke")

	// --- Re-grant → alice becomes visible again (bidirectional convergence) ---
	grant2 := s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "directory", dir.ID, "allow")
	require.NotEmpty(s.T(), grant2.ID)
	require.True(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "alice must see doc again after re-grant (FTS)")
	aliceSess3 := s.mcpInitialize(s.mcpClient(s.aliceRWToken))
	require.Eventually(s.T(), func() bool {
		return mcpSearchSees(s, aliceSess3, keyword, ws.ID, doc.ID)
	}, 60*time.Second, 3*time.Second, "alice MCP search must converge to visible after re-grant")
}

// TestRBACExistenceNonLeak verifies PRD F1.5/F3.2: a principal without read
// permission cannot infer a document's existence — counts are not leaked and
// no error is returned for read operations.
func (s *Suite) TestRBACExistenceNonLeak() {
	s.requireDB("non-admin user for existence-non-leak")
	admin := s.adminClient()
	keyword := uniqueKeyword("noleak")

	ws := s.createWorkspace(admin, "E2E NoLeak WS", "e2e-noleak-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-NoLeak", "# "+keyword+"\n\nsecret content")
	s.publishDoc(admin, doc.ID, doc.VersionNo, "")
	s.waitForIndexStatus(admin, doc.ID, "indexed")

	bob := s.jwtClient(s.bobJWT)

	// FTS: 0 hits, not an error.
	fts := s.searchFTS(bob, ws.ID, keyword, nil)
	require.Equal(s.T(), 0, fts.Total, "bob FTS total must be 0 (no count leak)")

	// GET doc directly: 403 or 404 (not 200) — no content leaked.
	_, st, _ := s.getDoc(bob, doc.ID)
	require.NotEqual(s.T(), http.StatusOK, st, "bob must not read doc content")

	// MCP get_document: empty result, NOT an error (existence non-leak).
	bobSess := s.mcpInitialize(s.mcpClient(s.bobROToken))
	res, err := bobSess.toolsCall("get_document", map[string]any{"document_id": doc.ID})
	require.Nilf(s.T(), err, "bob get_document must not be RPC error: %+v", err)
	require.False(s.T(), mcpGetDocID(res, doc.ID), "bob get_document must return empty content")
}

// --- helpers ---

// createDocInDir creates a markdown document placed in directory dirID.
func (s *Suite) createDocInDir(cl *Client, wsID, dirID, title, markdown string) document {
	var doc document
	st, env := cl.post("/api/v1/workspaces/"+wsID+"/documents",
		map[string]any{"title": title, "directory_id": dirID, "markdown": markdown}, &doc)
	require.Equalf(s.T(), http.StatusCreated, st, "create doc in dir: code=%d msg=%s", env.Code, env.Message)
	return doc
}

func ftsSees(s *Suite, cl *Client, wsID, keyword, docID string) bool {
	p := s.searchFTS(cl, wsID, keyword, nil)
	return containsDoc(p.Items, docID)
}

func mcpSearchSees(s *Suite, ms *mcpSession, keyword, wsID, docID string) bool {
	res, err := ms.toolsCall("search_knowledge_base", map[string]any{"query": keyword, "workspace_id": wsID, "top_n": 5})
	if err != nil || res == nil {
		return false
	}
	return mcpResultContainsDoc(res, docID)
}
