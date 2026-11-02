package tool

// knowledge_test.go covers the two Phase 6 converged-retrieval MCP tools
// (design-docs/19 §11.4, Phase 6-5): knowledge_search / knowledge_context.
// The tests pin the §8.2 no-leak contract (no read grant / no match → EMPTY
// success, never an error to the Agent), the converged-candidate shape
// (cross-type fold: document + codebase + skill candidates), the
// knowledge_context budget/truncation report (names continue-tools, §6.2 never
// silently truncates), and the include_content=false catalog-only trim. They
// also assert the legacy aliases are marked deprecated while the new tools are
// not, and that type-specialized tools are NOT flattened (D12).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// newKnowledgeTestClient seeds a mock with one document + one codebase + one
// skill in the eng workspace, grants the test user read, and returns the
// client + a read-only auth context. The three asset types are seeded so the
// converged fold exercises document + codebase + skill candidates.
func newKnowledgeTestClient(t *testing.T) (*moraclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m.AddDocument(moraclient.DocumentMeta{
		ID: tdocAPI, WorkspaceID: twsEng, Title: "API 设计规范", Status: "published",
		IndexStatus: "indexed", VersionNo: 5, CreatedBy: tuser,
	}, "# API 设计规范\n\n分页采用 page/page_size 参数。\n", "markdown", nil)
	m.AddCodebase(moraclient.MockCodebase{
		WorkspaceID: twsEng,
		Commit:      "commit-abc",
		Files:       []moraclient.CodeFileNode{{Path: "main.go", Lines: 42, Commit: "commit-abc"}},
		Symbols: []moraclient.CodeNodeDef{
			{
				Loc:       moraclient.CodeLoc{Commit: "commit-abc", Path: "main.go", StartLine: 10, Symbol: "Page", Kind: "func"},
				Signature: "func Page()",
				Docstring: "API 分页",
			},
		},
	})
	m.AddSkill(moraclient.MockSkill{
		ID: tskill, WorkspaceID: twsEng, DeliveryMode: "tool",
		Header: map[string]any{"name": "echo-skill", "version": "1.0", "description": "echoes 分页"},
	})
	m.GrantRead(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadOnly,
	}
	return m, ac
}

// decodeSearchResult unmarshals a tool's text content into a
// KnowledgeSearchResult. Fails the test on a bad decode.
func decodeSearchResult(t *testing.T, res *server.ToolCallResult) *moraclient.KnowledgeSearchResult {
	t.Helper()
	require.Len(t, res.Content, 1)
	var out moraclient.KnowledgeSearchResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	return &out
}

// decodeContextResult unmarshals a tool's text content into a
// KnowledgeContextResult.
func decodeContextResult(t *testing.T, res *server.ToolCallResult) *moraclient.KnowledgeContextResult {
	t.Helper()
	require.Len(t, res.Content, 1)
	var out moraclient.KnowledgeContextResult
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &out))
	return &out
}

// --- knowledge_search ---

// knowledge_search with permission folds document + codebase + skill candidates
// matching the query into one ranked list (design-docs/19 §11.4).
func TestKnowledgeSearchFoldsTypes(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "分页"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	sr := decodeSearchResult(t, res)
	assert.NotEmpty(t, sr.Candidates, "converged search must return matching candidates")
	types := map[string]bool{}
	for _, c := range sr.Candidates {
		types[c.AssetType] = true
		assert.NotEmpty(t, c.AssetID)
		assert.NotEmpty(t, c.Citation.VersionOrRevision, "every candidate carries a version anchor (§8.1)")
	}
	// The document matches "分页" in body; the codebase symbol matches via its
	// docstring; the skill matches via its description. All three types fold in.
	assert.True(t, types["document"], "document candidate folded in")
	assert.True(t, types["codebase"], "codebase candidate folded in")
	assert.True(t, types["skill"], "skill candidate folded in")
	assert.NotEmpty(t, sr.Intent, "intent label is set on the response")
}

// knowledge_search without read grant returns EMPTY success (§8.2 no-leak) —
// never an error to the Agent. The three empty states stay distinguishable on
// the response shape, but the tool layer never surfaces a 403.
func TestKnowledgeSearchNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "分页"})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError, "no-permission must NOT set isError")
	sr := decodeSearchResult(t, res)
	assert.Empty(t, sr.Candidates, "empty-authorized: no candidates")
}

// knowledge_search with no matches returns empty (empty-no-results, §7.3) — a
// normal success, not an error.
func TestKnowledgeSearchNoMatchEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "不存在的查询词zzz"})
	require.NoError(t, err)
	sr := decodeSearchResult(t, res)
	assert.Empty(t, sr.Candidates)
}

// knowledge_search missing required query → invalid params.
func TestKnowledgeSearchInvalidParams(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_search honors the asset_types filter (§4.2 rule 1 — explicit type
// set narrows the fan-out). The args mirror the JSON-decoded shape a real
// tools/call delivers ([]any, not []string — optStringSlice asserts []any).
func TestKnowledgeSearchAssetTypeFilter(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query":       "分页",
		"asset_types": []any{"document"},
	})
	require.NoError(t, err)
	sr := decodeSearchResult(t, res)
	assert.NotEmpty(t, sr.Candidates, "document matches the query")
	for _, c := range sr.Candidates {
		assert.Equal(t, "document", c.AssetType, "only document candidates when asset_types=[document]")
	}
}

// --- knowledge_context ---

// knowledge_context with include_content=false (default) returns the catalog
// (directory + citation) with snippets dropped — the body is a progressive
// read via the type tool (§6.2).
func TestKnowledgeContextCatalogOnly(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "分页"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	cr := decodeContextResult(t, res)
	assert.NotEmpty(t, cr.Candidates)
	for _, c := range cr.Candidates {
		assert.Empty(t, c.Snippet, "include_content=false → snippet dropped (catalog only, §6.2)")
		assert.NotEmpty(t, c.Citation.VersionOrRevision)
	}
}

// knowledge_context with include_content=true keeps the snippet body.
func TestKnowledgeContextIncludeContent(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query":           "分页",
		"include_content": true,
	})
	require.NoError(t, err)
	cr := decodeContextResult(t, res)
	assert.NotEmpty(t, cr.Candidates)
	// At least one candidate (the document) carries a non-empty snippet.
	hasSnippet := false
	for _, c := range cr.Candidates {
		if c.Snippet != "" {
			hasSnippet = true
		}
	}
	assert.True(t, hasSnippet, "include_content=true keeps the snippet body")
}

// knowledge_context with max_items below the candidate count emits a
// truncation report naming continue-tools (§6.2 — never silently truncates).
func TestKnowledgeContextTruncationReport(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query":     "分页",
		"max_items": 1,
	})
	require.NoError(t, err)
	cr := decodeContextResult(t, res)
	require.Len(t, cr.Candidates, 1, "max_items caps the candidate count")
	assert.NotEmpty(t, cr.Truncation.Reason, "truncation reason is set")
	assert.NotEmpty(t, cr.Truncation.TruncatedAssetIDs, "dropped asset ids are named")
	assert.NotEmpty(t, cr.Truncation.ContinueTools, "continue-tools point at progressive reads")
}

// knowledge_context without read grant returns EMPTY success (§8.2 no-leak).
func TestKnowledgeContextNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "分页"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	cr := decodeContextResult(t, res)
	assert.Empty(t, cr.Candidates)
}

// knowledge_context missing required query → invalid params.
func TestKnowledgeContextInvalidParams(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeContextTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// --- deprecated aliases + type-specialized tools not flattened (D12) ---

// The legacy aliases are marked deprecated; the new converged tools are not
// (design-docs/19 §11.4 compatibility window).
func TestDeprecatedAliasFlags(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	assert.True(t, NewSearchTool(m).Definition().Deprecated, "search_knowledge_base deprecated")
	assert.True(t, NewGetDocumentTool(m).Definition().Deprecated, "get_document deprecated")
	assert.True(t, NewListDocumentsTool(m).Definition().Deprecated, "list_documents deprecated")
	assert.False(t, NewKnowledgeSearchTool(m).Definition().Deprecated, "knowledge_search NOT deprecated")
	assert.False(t, NewKnowledgeContextTool(m).Definition().Deprecated, "knowledge_context NOT deprecated")
}

// Type-specialized tools are NOT flattened by the converged tools (D12): the
// code_* and skill_* tools remain independently registered surfaces. This is
// a structural assertion — the converged tools do not subsume code_callers /
// code_impact / skill_resources. RBAC + rate-limit constraints on the legacy
// aliases are unchanged (the deprecated flag is a migration hint only).
func TestTypeSpecializedToolsNotFlattened(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	// The specialized tools still build + advertise independently.
	for _, th := range []server.ToolHandler{
		NewCodeCallersTool(m), NewCodeImpactTool(m),
		NewSkillResourcesTool(m), NewSkillReadTool(m),
	} {
		def := th.Definition()
		assert.False(t, def.Deprecated, "%s is a type-specialized tool, not deprecated", def.Name)
		assert.False(t, th.IsWrite(), "%s stays read-only", def.Name)
	}
	// The converged tools do not shadow the specialized names.
	ks := NewKnowledgeSearchTool(m).Definition().Name
	kc := NewKnowledgeContextTool(m).Definition().Name
	assert.NotEqual(t, "code_callers", ks)
	assert.NotEqual(t, "code_impact", kc)
	assert.NotEqual(t, "skill_resources", ks)
}
