//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCoreClosedLoop drives the full PRD §5.1 core loop end to end:
//  1. create/edit doc → doc_event emitted
//  2. RAG worker consumes → chunk → embedding → Qdrant upsert → index_status receipt
//  3. FTS + semantic search hit, RBAC filter effective
//  4. Agent via MCP initialize → search_knowledge_base → get_document; over-permission empty + audit
//  5. delete doc → cascade clear chunks → search no longer returns
//
// Covers AC-9, AC-10, AC-12, AC-15, AC-16, AC-18 (existence non-leak).
func (s *Suite) TestCoreClosedLoop() {
	s.requireDB("non-admin user + MCP token for RBAC step")

	admin := s.adminClient()
	keyword := uniqueKeyword("coreloop")

	// --- Step 1: create + publish → doc_event (index_status pending) ---
	ws := s.createWorkspace(admin, "E2E CoreLoop WS", "e2e-coreloop-"+randHex(4))
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-CoreLoop-K8s",
		"# Kubernetes 部署指南\n\n"+keyword+" deployment guide for production clusters.")
	require.Equal(s.T(), "pending", doc.IndexStatus, "newly created doc must be pending indexing")
	require.Equal(s.T(), 1, doc.VersionNo)

	published := s.publishDoc(admin, doc.ID, doc.VersionNo, "")

	// --- Step 2: wait for RAG pipeline → index_status=indexed (receipt) ---
	indexed := s.waitForIndexStatus(admin, published.ID, "indexed")
	require.Equal(s.T(), "indexed", indexed.IndexStatus, "doc must reach indexed after pipeline")

	// Grant alice read on this doc so she can see it (for the allow-path of RBAC).
	s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "document", doc.ID, "allow")
	alice := s.jwtClient(s.aliceJWT)

	// --- Step 3: FTS + semantic search hit; RBAC filter effective ---
	ftsAdmin := s.searchFTS(admin, ws.ID, keyword, nil)
	require.True(s.T(), ftsAdmin.Total >= 1, "admin FTS must hit the doc; got total=%d", ftsAdmin.Total)
	require.True(s.T(), containsDoc(ftsAdmin.Items, doc.ID), "admin FTS result must include doc")

	ragAdmin, st, env := s.ragSearch(admin, keyword, ws.ID, 10)
	require.Equalf(s.T(), http.StatusOK, st, "admin rag/search: code=%d msg=%s", env.Code, env.Message)
	require.Truef(s.T(), ragAdmin.Total >= 1, "admin RAG must hit the doc; got %+v", ragAdmin)

	// bob has no permission at all → must not hit (existence non-leak).
	bob := s.jwtClient(s.bobJWT)
	ftsBob := s.searchFTS(bob, ws.ID, keyword, nil)
	require.Equal(s.T(), 0, ftsBob.Total, "bob (no perm) FTS must return 0 hits")
	ragBob, _, _ := s.ragSearch(bob, keyword, ws.ID, 10)
	require.Equal(s.T(), 0, ragBob.Total, "bob (no perm) RAG must return 0 hits")

	// alice has read → must hit (RBAC allow path converges with admin).
	ftsAlice := s.searchFTS(alice, ws.ID, keyword, nil)
	require.True(s.T(), ftsAlice.Total >= 1, "alice (granted read) FTS must hit")
	ragAlice, _, _ := s.ragSearch(alice, keyword, ws.ID, 10)
	require.True(s.T(), ragAlice.Total >= 1, "alice (granted read) RAG must hit")

	// --- Step 4: Agent via MCP initialize → search_knowledge_base → get_document ---
	// Admin-bound dev token: full visibility.
	adminSess := s.mcpInitialize(s.mcpClient(s.cfg.DevToken))
	tools := adminSess.toolsList()
	require.True(s.T(), hasTool(tools, "search_knowledge_base"), "tools/list must include search_knowledge_base")

	skbRes, skbErr := adminSess.toolsCall("search_knowledge_base", map[string]any{"query": keyword, "workspace_id": ws.ID, "top_n": 5})
	require.Nilf(s.T(), skbErr, "search_knowledge_base rpc error: %+v", skbErr)
	require.Falsef(s.T(), skbRes["isError"].(bool), "search_knowledge_base isError=true: %+v", skbRes)
	require.True(s.T(), mcpResultContainsDoc(skbRes, doc.ID), "MCP search must hit doc; got %+v", skbRes)

	getRes, getErr := adminSess.toolsCall("get_document", map[string]any{"document_id": doc.ID})
	require.Nilf(s.T(), getErr, "get_document rpc error: %+v", getErr)
	require.Falsef(s.T(), getRes["isError"].(bool), "get_document isError=true: %+v", getRes)
	require.True(s.T(), mcpGetDocID(getRes, doc.ID), "get_document must return the doc content")

	// Alice via her own token: granted read → search hits; get_document returns content.
	aliceSess := s.mcpInitialize(s.mcpClient(s.aliceRWToken))
	aRes, aErr := aliceSess.toolsCall("search_knowledge_base", map[string]any{"query": keyword, "workspace_id": ws.ID, "top_n": 5})
	require.Nilf(s.T(), aErr, "alice search rpc error: %+v", aErr)
	require.True(s.T(), mcpResultContainsDoc(aRes, doc.ID), "alice (granted) MCP search must hit")

	// Bob via his token (no permission): empty result, NOT an error (existence non-leak).
	bobSess := s.mcpInitialize(s.mcpClient(s.bobROToken))
	bRes, bErr := bobSess.toolsCall("search_knowledge_base", map[string]any{"query": keyword, "workspace_id": ws.ID, "top_n": 5})
	require.Nilf(s.T(), bErr, "bob search rpc error: %+v", bErr)
	require.False(s.T(), bRes["isError"].(bool), "bob search must not be an RPC error (existence non-leak)")
	require.False(s.T(), mcpResultContainsDoc(bRes, doc.ID), "bob (no perm) must not see doc in MCP search")

	bGetRes, bGetErr := bobSess.toolsCall("get_document", map[string]any{"document_id": doc.ID})
	require.Nilf(s.T(), bGetErr, "bob get_document rpc error: %+v", bGetErr)
	require.False(s.T(), mcpGetDocID(bGetRes, doc.ID), "bob (no perm) get_document must return empty (no existence leak)")

	// Audit: tool calls were recorded.
	audit := s.toolCallsAudit(admin, "tool_name=search_knowledge_base")
	require.True(s.T(), len(audit) >= 3, "audit /mcp/tool-calls must record search calls; got %d", len(audit))

	// --- Step 5: delete → cascade clear chunks → search no longer returns ---
	require.Equal(s.T(), http.StatusNoContent, s.deleteDoc(admin, doc.ID))

	// FTS must no longer return the doc.
	ftsAfter := s.searchFTS(admin, ws.ID, keyword, nil)
	require.False(s.T(), containsDoc(ftsAfter.Items, doc.ID), "deleted doc must not appear in FTS")

	// RAG must no longer return the doc (chunks cascade-cleared).
	require.Eventually(s.T(), func() bool {
		r, _, _ := s.ragSearch(admin, keyword, ws.ID, 10)
		return !ragContainsDoc(r, doc.ID)
	}, 30*time.Second, 2*time.Second, "deleted doc must disappear from RAG after cascade cleanup")
}

// --- small assertion helpers ---

func uniqueKeyword(tag string) string {
	return "e2e" + tag + randHex(4)
}

func containsDoc(items []json.RawMessage, docID string) bool {
	for _, raw := range items {
		var hit searchHit
		if json.Unmarshal(raw, &hit) == nil && hit.DocumentID == docID {
			return true
		}
	}
	return false
}

func ragContainsDoc(r ragResult, docID string) bool {
	for _, h := range r.Items {
		if h.DocumentID == docID {
			return true
		}
	}
	return false
}

func hasTool(tools []map[string]any, name string) bool {
	for _, t := range tools {
		if n, ok := t["name"].(string); ok && n == name {
			return true
		}
	}
	return false
}

func mcpResultContainsDoc(res map[string]any, docID string) bool {
	if data, ok := res["data"].([]any); ok {
		for _, it := range data {
			if m, ok := it.(map[string]any); ok {
				if id, _ := m["document_id"].(string); id == docID {
					return true
				}
			}
		}
	}
	// fallback: text payload
	if txt, ok := res["text"].(string); ok {
		return strings.Contains(txt, docID)
	}
	return false
}

func mcpGetDocID(res map[string]any, docID string) bool {
	// get_document returns the document; data may be a map with id, or content text.
	if data, ok := res["data"].(map[string]any); ok {
		if id, _ := data["id"].(string); id == docID {
			return true
		}
	}
	if txt, ok := res["text"].(string); ok {
		return strings.Contains(txt, docID)
	}
	return false
}
