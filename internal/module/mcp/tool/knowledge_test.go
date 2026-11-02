package tool

// knowledge_test.go covers the knowledge_search / knowledge_context MCP tools
// (design-docs/19 §11.4, Phase 6-5/YS-210). The tests pin:
//
//   - The §8.2 no-leak contract: no-permission + no-results both return an
//     EMPTY success (indistinguishable), never an error to the Agent.
//   - The §11.5 three-state contract: a provider fault (ErrProviderDown)
//     surfaces as an EMPTY result + a diagnostic note, distinct from genuine
//     empty — never a faked candidate, never confused with "no knowledge".
//   - The §11.4 compatibility-alias deprecation: search_knowledge_base /
//     get_document / list_documents advertise deprecated:true in tools/list
//     while the new knowledge_* tools do NOT (and the type-specialized tools
//     remain non-deprecated — D12 no-flatten).
//   - Required-arg validation (query + workspace_id).
//
// The mock models the Broker's leak-safe contract (read-gated empty on
// no-permission; empty candidate list on no matches; ErrProviderDown on a
// seeded fault) — not the ten-step pipeline.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/audit"
	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/resource"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// newKnowledgeTestClient seeds a mock with one document in the eng workspace,
// grants the test user read, and returns the client + a read-only auth context.
// Callers needing write flip ac.Scope to ScopeReadWrite (the knowledge_* tools
// are read-only, so read scope suffices for the success path).
func newKnowledgeTestClient(t *testing.T) (*moraclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m.AddDocument(moraclient.DocumentMeta{
		ID: tdocAPI, WorkspaceID: twsEng, Title: "API 规范", Status: "published",
		IndexStatus: "indexed", VersionNo: 1, CreatedBy: tuser,
	}, "# API 规范\n\n分页采用 page/page_size。\n", "markdown", nil)
	m.GrantRead(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadOnly,
	}
	return m, ac
}

// knowledgeArgs builds the §11.3 tool args for a query against the eng workspace.
func knowledgeArgs(query string) map[string]any {
	return map[string]any{"query": query, "workspace_id": twsEng}
}

// --- knowledge_search ---

// knowledge_search with permission returns the Broker candidate carrying the
// seeded doc's title + a citation anchor.
func TestKnowledgeSearchSuccess(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "API 规范") // title
	assert.Contains(t, res.Content[0].Text, "candidates")
	assert.Contains(t, res.Content[0].Text, "citation")
	assert.Contains(t, res.Content[0].Text, `"intent"`) // Broker envelope
}

// knowledge_search with no matches returns an EMPTY candidate list (normal
// success — authorized-empty / no-results indistinguishable, §8.2).
func TestKnowledgeSearchNoMatchesEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("不存在的术语"))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, `"candidates":[]`)
}

// knowledge_search without permission returns an EMPTY candidate list (no
// existence leak — §8.2). Indistinguishable from no-results above.
func TestKnowledgeSearchNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewKnowledgeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err, "no-permission must NOT be an error (no leak)")
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, `"candidates":[]`)
	assert.NotContains(t, res.Content[0].Text, "API 规范")
}

// knowledge_search on a provider fault returns EMPTY + a diagnostic note,
// distinct from genuine empty (§11.5 three-state — a fault is never "no
// knowledge").
func TestKnowledgeSearchProviderDownDiagnostic(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.SetKnowledgeProviderDown(twsEng, "codegraph sidecar timeout")
	tl := NewKnowledgeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err, "provider-down must NOT be a hard error (§11.5 diagnostic)")
	assert.False(t, res.IsError)
	// The diagnostic note names the fault class — NOT an empty candidate list,
	// and NOT a faked result.
	assert.NotEqual(t, "", res.Content[0].Text)
	assert.NotContains(t, res.Content[0].Text, `"candidates"`)
	assert.Contains(t, res.Content[0].Text, "unavailable")
}

// --- knowledge_context ---

// knowledge_context with permission returns the full Broker envelope
// (intent + candidates + citation).
func TestKnowledgeContextSuccess(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeContextTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "API 规范")
	assert.Contains(t, res.Content[0].Text, `"intent"`)
	assert.Contains(t, res.Content[0].Text, `"policy_version"`)
	assert.Contains(t, res.Content[0].Text, "citation")
}

// knowledge_context max_items truncation surfaces truncation reason + continue
// tools (D6 — never silently truncate citations).
func TestKnowledgeContextTruncationSurfacesContinueTools(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	// Seed a second doc sharing a term with the first so one query matches both
	// and max_items=1 forces truncation (both titles contain "规范"? no — use a
	// body term both share).
	m.AddDocument(moraclient.DocumentMeta{
		ID: "doc-t-onboarding-0002", WorkspaceID: twsEng, Title: "新人入职指南",
		Status: "published", IndexStatus: "indexed", VersionNo: 1, CreatedBy: tuser,
	}, "# 入职\n\n分页说明。\n", "markdown", nil) // body shares "分页" with the API doc
	tl := NewKnowledgeContextTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng, "max_items": 1,
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "truncation")
	assert.Contains(t, res.Content[0].Text, "continue_tools")
}

// knowledge_context without permission → EMPTY (no leak).
func TestKnowledgeContextNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewKnowledgeContextTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, `"candidates":[]`)
}

// knowledge_context provider-down → diagnostic note (§11.5 three-state).
func TestKnowledgeContextProviderDownDiagnostic(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.SetKnowledgeProviderDown(twsEng, "qdrant unavailable")
	tl := NewKnowledgeContextTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), knowledgeArgs("分页"))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.NotEqual(t, "", res.Content[0].Text)
	assert.NotContains(t, res.Content[0].Text, `"candidates"`)
	assert.Contains(t, res.Content[0].Text, "unavailable")
}

// --- validation ---

// knowledge_search missing query → invalid params.
func TestKnowledgeSearchMissingQuery(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeSearchTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"workspace_id": twsEng})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_search missing workspace_id → invalid params.
func TestKnowledgeSearchMissingWorkspace(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeSearchTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"query": "x"})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_context missing query → invalid params.
func TestKnowledgeContextMissingQuery(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tl := NewKnowledgeContextTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"workspace_id": twsEng})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// --- deprecation markers (§11.4 compatibility aliases) ---

// TestDeprecatedAliasesMarked asserts the three compatibility aliases advertise
// deprecated:true in tools/list, while the new knowledge_* tools + a sample
// type-specialized tool (code_callers) do NOT (D12 — type tools are not
// flattened, and the new tools are the non-deprecated surface).
func TestDeprecatedAliasesMarked(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	cases := []struct {
		name     string
		tool     server.ToolHandler
		deprecated bool
	}{
		{"search_knowledge_base", NewSearchTool(m), true},
		{"get_document", NewGetDocumentTool(m), true},
		{"list_documents", NewListDocumentsTool(m), true},
		{"knowledge_search", NewKnowledgeSearchTool(m), false},
		{"knowledge_context", NewKnowledgeContextTool(m), false},
		{"code_callers", NewCodeCallersTool(m), false}, // type-specialized, not flattened (D12)
		{"skill_resources", NewSkillResourcesTool(m), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			def := c.tool.Definition()
			assert.Equal(t, c.name, def.Name)
			assert.Equal(t, c.deprecated, def.Deprecated,
				"%s deprecated flag mismatch (want %v)", c.name, c.deprecated)
		})
	}
}

// TestKnowledgeToolsRegisteredInServer confirms the new tools register and
// appear in tools/list under their canonical names, and that the deprecated
// flag serializes through the tools/list path (catches a wiring drift where a
// tool is defined but not registered in cmd/mcp-server, and a ToolDef
// serialization drift on the Deprecated field). It builds a real Server over
// the exported in-memory stores so the full Handle → handleToolsList path runs.
func TestKnowledgeToolsRegisteredInServer(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	srv := server.NewServer(
		resource.NewRegistry(m),
		server.NewMemorySessionStore(),
		audit.NewMemoryStore(),
		auth.NewMemoryRateLimiter(),
		"test-mcp", "0.0.1",
	)
	srv.RegisterTool(NewKnowledgeSearchTool(m))
	srv.RegisterTool(NewKnowledgeContextTool(m))
	srv.RegisterTool(NewSearchTool(m)) // deprecated alias

	resp, _ := srv.Handle(context.Background(), &server.Request{
		JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/list",
	})
	require.NotNil(t, resp, "tools/list with an id must return a response")
	require.Nil(t, resp.Error, "tools/list must not error")
	list, ok := resp.Result.(server.ToolsListResult)
	require.True(t, ok, "tools/list result is ToolsListResult, got %T", resp.Result)
	byName := make(map[string]server.ToolDef, len(list.Tools))
	for _, d := range list.Tools {
		byName[d.Name] = d
	}
	assert.Contains(t, byName, "knowledge_search")
	assert.Contains(t, byName, "knowledge_context")
	assert.True(t, byName["search_knowledge_base"].Deprecated, "alias deprecated in tools/list")
	assert.False(t, byName["knowledge_search"].Deprecated, "new tool not deprecated")
	assert.False(t, byName["knowledge_context"].Deprecated, "new tool not deprecated")
}
