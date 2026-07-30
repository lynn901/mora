//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stretchr/testify/require"
)

// TestAC1_ContentReversibility covers AC-1: Markdown ↔ Block content is stored
// reversibly; code blocks / headings are preserved through the create→get round
// trip. (Full WYSIWYG rendering is a frontend concern; this verifies the content
// substrate the editor relies on.)
func (s *Suite) TestAC1_ContentReversibility() {
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E AC1 WS", "e2e-ac1-"+randHex(4))

	// Markdown with a heading + code block → converted to blocks on create.
	doc := s.createDocMarkdown(admin, ws.ID, "E2E-AC1-MD",
		"# AC1 Title\n\nIntro paragraph.\n\n```go\nfmt.Println(\"hi\")\n```\n")
	require.Equal(s.T(), "markdown", doc.Format, "markdown input must be stored as format=markdown")
	require.NotEmpty(s.T(), doc.Content, "content blocks must be materialized")

	got, st, _ := s.getDoc(admin, doc.ID)
	require.Equal(s.T(), http.StatusOK, st)
	require.True(s.T(), hasBlockType(got.Content, "heading"), "heading block must survive round trip")
	require.True(s.T(), hasBlockType(got.Content, "codeBlock"), "codeBlock must survive round trip")

	// Block input path → format=blocks.
	blocks := []map[string]any{
		headingBlock(1, "Block Title"),
		codeBlock("bash", "echo hello"),
	}
	doc2 := s.createDocBlocks(admin, ws.ID, "E2E-AC1-Blocks", blocks)
	require.Equal(s.T(), "blocks", doc2.Format)
	got2, _, _ := s.getDoc(admin, doc2.ID)
	require.True(s.T(), hasBlockType(got2.Content, "codeBlock"), "block-path codeBlock must persist")
}

// TestAC4_WorkspaceIsolationAndTree covers AC-4: multi-workspace data isolation
// + infinite-depth directory tree.
func (s *Suite) TestAC4_WorkspaceIsolationAndTree() {
	admin := s.adminClient()
	wsA := s.createWorkspace(admin, "E2E AC4 A", "e2e-ac4a-"+randHex(4))
	wsB := s.createWorkspace(admin, "E2E AC4 B", "e2e-ac4b-"+randHex(4))

	// Infinite-depth nesting in wsA: root → child → grandchild → great.
	root := s.createDirectory(admin, wsA.ID, "", "root")
	child := s.createDirectory(admin, wsA.ID, root.ID, "child")
	grand := s.createDirectory(admin, wsA.ID, child.ID, "grand")
	great := s.createDirectory(admin, wsA.ID, grand.ID, "great")
	_ = great

	// A directory in wsB only.
	s.createDirectory(admin, wsB.ID, "", "wsB-root")

	treeA := s.directoryTree(admin, wsA.ID)
	treeB := s.directoryTree(admin, wsB.ID)

	// wsA tree nests at least 4 levels deep.
	depth := maxDepth(treeA)
	require.GreaterOrEqual(s.T(), depth, 4, "wsA tree must support >=4 levels of nesting; got depth %d", depth)

	// wsB must NOT contain any wsA directory (isolation).
	require.False(s.T(), treeContainsName(treeB, "child"), "wsB must not leak wsA directories")
	require.False(s.T(), treeContainsName(treeA, "wsB-root"), "wsA must not leak wsB directories")
}

// TestAC6_VersionDiffAndRollback covers AC-6: any two versions can be diffed and
// rolled back; rollback produces a NEW version (history is append-only).
//
// NOTE: GET /documents/:id/versions currently returns a stub (known gap — see
// README). This test exercises the mounted diff + rollback + version_no path.
func (s *Suite) TestAC6_VersionDiffAndRollback() {
	admin := s.adminClient()
	ws := s.createWorkspace(admin, "E2E AC6 WS", "e2e-ac6-"+randHex(4))

	v1 := s.createDocMarkdown(admin, ws.ID, "E2E-AC6", "# v1 content\n\nfirst version body")
	require.Equal(s.T(), 1, v1.VersionNo)

	v2, _, _ := s.updateDoc(admin, v1.ID, v1.VersionNo, "# v2 content\n\nsecond version body")
	require.Equal(s.T(), 2, v2.VersionNo, "update must produce a new version")

	// Diff v1..v2 must be non-empty.
	st, env := admin.get("/api/v1/documents/"+v1.ID+"/versions/diff?from=1&to=2", nil)
	require.Equalf(s.T(), http.StatusOK, st, "diff: code=%d msg=%s", env.Code, env.Message)
	var diff struct {
		FromVersion int `json:"from_version"`
		ToVersion   int `json:"to_version"`
		Diff        []any `json:"diff"`
	}
	require.NoError(s.T(), json.Unmarshal(env.Data, &diff))
	require.Equal(s.T(), 1, diff.FromVersion)
	require.Equal(s.T(), 2, diff.ToVersion)
	require.NotEmpty(s.T(), diff.Diff, "diff between v1 and v2 must be non-empty")

	// Rollback to v1 → produces NEW version (3), history intact.
	rolled := s.rollbackDoc(admin, v1.ID, 1)
	require.Equal(s.T(), 3, rolled.VersionNo, "rollback must produce a new version, not overwrite")

	// Content after rollback must match v1.
	got, _, _ := s.getDoc(admin, v1.ID)
	require.True(s.T(), contentHasText(got.Content, "first version body"), "rollback content must match v1")
}

// TestAC7_RBACInheritanceAndOverride covers AC-7: RBAC workspace/directory/page
// config; inheritance + explicit override; no-permission docs absent from tree
// and search.
func (s *Suite) TestAC7_RBACInheritanceAndOverride() {
	s.requireDB("non-admin subject for RBAC")
	admin := s.adminClient()
	keyword := uniqueKeyword("ac7")
	ws := s.createWorkspace(admin, "E2E AC7 WS", "e2e-ac7-"+randHex(4))
	dir := s.createDirectory(admin, ws.ID, "", "Eng")
	doc := s.createDocInDir(admin, ws.ID, dir.ID, "E2E-AC7", "# "+keyword+"\n\ninheritance test")
	s.publishDoc(admin, doc.ID, doc.VersionNo, "")

	alice := s.jwtClient(s.aliceJWT)

	// Default deny: alice cannot read or search the doc.
	_, st, _ := s.getDoc(alice, doc.ID)
	require.NotEqual(s.T(), http.StatusOK, st, "alice default deny: must not read")
	require.Equal(s.T(), 0, s.searchFTS(alice, ws.ID, keyword, nil).Total, "alice default deny: 0 search hits")

	// Grant read on directory (subtree inherits to doc) → alice can read + search.
	grant := s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "directory", dir.ID, "allow")
	_, st, _ = s.getDoc(alice, doc.ID)
	require.Equal(s.T(), http.StatusOK, st, "alice must read via inherited directory allow")
	require.True(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "alice must search via inherited allow")

	// Explicit deny on the doc overrides inherited allow.
	deny := s.grantPermission(admin, "user", s.aliceUserID, s.viewerRoleID, "document", doc.ID, "deny")
	_, st, _ = s.getDoc(alice, doc.ID)
	require.NotEqual(s.T(), http.StatusOK, st, "explicit deny on doc must override inherited allow")
	require.False(s.T(), ftsSees(s, alice, ws.ID, keyword, doc.ID), "deny must remove doc from search")

	// Revoke the deny → inherited allow re-applies.
	s.revokePermission(admin, deny.ID)
	_, st, _ = s.getDoc(alice, doc.ID)
	require.Equal(s.T(), http.StatusOK, st, "after revoking deny, inherited allow must re-apply")

	// Cleanup the allow grant too.
	s.revokePermission(admin, grant.ID)
}

// TestAC8_FTSFiltersAndRBAC covers AC-8: full-text search with multi-dimensional
// filters (directory, creator) + result snippets; RBAC filtering.
func (s *Suite) TestAC8_FTSFiltersAndRBAC() {
	s.requireDB("non-admin user for search RBAC")
	admin := s.adminClient()
	keyword := uniqueKeyword("ac8")
	ws := s.createWorkspace(admin, "E2E AC8 WS", "e2e-ac8-"+randHex(4))
	dirA := s.createDirectory(admin, ws.ID, "", "DirA")
	dirB := s.createDirectory(admin, ws.ID, "", "DirB")

	docA := s.createDocInDir(admin, ws.ID, dirA.ID, "E2E-AC8-A", "# "+keyword+" in dirA")
	docB := s.createDocInDir(admin, ws.ID, dirB.ID, "E2E-AC8-B", "# "+keyword+" in dirB")
	s.publishDoc(admin, docA.ID, docA.VersionNo, "")
	s.publishDoc(admin, docB.ID, docB.VersionNo, "")

	// Unfiltered: both docs match the keyword.
	all := s.searchFTS(admin, ws.ID, keyword, nil)
	require.GreaterOrEqual(s.T(), all.Total, 2, "unfiltered search must find both docs")

	// directory_id filter narrows to dirA only.
	onlyA := s.searchFTS(admin, ws.ID, keyword, map[string]string{"directory_id": dirA.ID})
	require.True(s.T(), containsDoc(onlyA.Items, docA.ID), "directory filter must include docA")
	require.False(s.T(), containsDoc(onlyA.Items, docB.ID), "directory filter must exclude docB")

	// created_by filter narrows to admin's docs.
	byCreator := s.searchFTS(admin, ws.ID, keyword, map[string]string{"created_by": s.adminUserID()})
	require.GreaterOrEqual(s.T(), byCreator.Total, 2, "created_by filter must find admin's docs")

	// Snippet/highlight present on at least one hit.
	require.True(s.T(), anySnippet(all.Items), "at least one hit must carry a snippet")

	// RBAC: alice (no grants) gets 0 hits — existence not leaked.
	alice := s.jwtClient(s.aliceJWT)
	require.Equal(s.T(), 0, s.searchFTS(alice, ws.ID, keyword, nil).Total, "alice must get 0 hits without permission")
}

// --- helpers ---

func hasBlockType(content []map[string]any, want string) bool {
	for _, b := range content {
		if t, ok := b["type"].(string); ok && t == want {
			return true
		}
	}
	return false
}

func contentHasText(content []map[string]any, needle string) bool {
	for _, b := range content {
		if raw, ok := b["content"].([]any); ok {
			for _, c := range raw {
				if m, ok := c.(map[string]any); ok {
					if t, ok := m["text"].(string); ok && strContains(t, needle) {
						return true
					}
				}
			}
		}
	}
	return false
}

func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func maxDepth(trees []directory) int {
	max := 0
	for _, t := range trees {
		d := 1 + maxDepth(t.Children)
		if d > max {
			max = d
		}
	}
	return max
}

func treeContainsName(trees []directory, name string) bool {
	for _, t := range trees {
		if t.Name == name || treeContainsName(t.Children, name) {
			return true
		}
	}
	return false
}

func anySnippet(items []json.RawMessage) bool {
	for _, raw := range items {
		var hit searchHit
		if json.Unmarshal(raw, &hit) == nil && hit.Snippet != "" {
			return true
		}
	}
	return false
}

// rollbackDoc rolls back to version_no and returns the new-version document.
func (s *Suite) rollbackDoc(cl *Client, docID string, versionNo int) document {
	var doc document
	st, env := cl.post("/api/v1/documents/"+docID+"/versions/"+strconv.Itoa(versionNo)+"/rollback", nil, &doc)
	require.Equalf(s.T(), http.StatusOK, st, "rollback: code=%d msg=%s", env.Code, env.Message)
	return doc
}

func (s *Suite) adminUserID() string {
	if s.pool == nil {
		return ""
	}
	var id string
	_ = s.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email=$1`, s.cfg.AdminEmail).Scan(&id)
	return id
}
