package tool

// code_test.go covers the code_* MCP tools (design-docs/17 §6.2): the §8.2
// no-leak + §15 fault contracts that every codegraph tool must honor. The
// tools are read-only and delegate to the MoraClient; the tests pin the
// mapping table itself (not-found/forbidden → empty success, never an error)
// plus the happy path so a regression that surfaces a 403-style leak is caught
// here.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

const (
	tcbCode = "codebase-" + twsEng // AddCodebase derives this id from the workspace
)

// newCodeTestClient seeds a mock with one codebase in the eng workspace, grants
// the test user read, and returns the client + auth context. Mirrors
// newTestClient for the document tools.
func newCodeTestClient(t *testing.T) (*moraclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m.AddCodebase(moraclient.MockCodebase{
		WorkspaceID: twsEng,
		Commit:      "abc123",
		Files: []moraclient.CodeFileNode{
			{Path: "main.go", Lines: 42, Commit: "abc123"},
		},
		Symbols: []moraclient.CodeNodeDef{
			{Loc: moraclient.CodeLoc{Commit: "abc123", Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"}, Signature: "func Serve()", Docstring: "serves http"},
		},
		Edges: []moraclient.CodeEdge{
			{From: moraclient.CodeLoc{Commit: "abc123", Path: "main.go", StartLine: 20, Symbol: "main"}, To: moraclient.CodeLoc{Commit: "abc123", Path: "main.go", StartLine: 10, Symbol: "Serve"}, Kind: "calls"},
		},
	})
	m.GrantRead(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadOnly,
	}
	return m, ac
}

// code_status with permission returns metadata carrying the commit (§3.2).
func TestCodeStatusSuccess(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeStatusTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "abc123")
}

// code_status without permission returns EMPTY success (no existence leak).
func TestCodeStatusNoPermissionEmpty(t *testing.T) {
	m, ac := newCodeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewCodeStatusTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// code_status for a missing codebase returns empty (no existence hint).
func TestCodeStatusMissingEmpty(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeStatusTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": "no-such-codebase"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// code_files returns the seeded file tree.
func TestCodeFilesSuccess(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeFilesTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "main.go")
}

// code_search with a matching query returns hits carrying the commit.
func TestCodeSearchSuccess(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "query": "Serve"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "Serve")
	assert.Contains(t, res.Content[0].Text, "abc123")
}

// code_search with a non-matching query returns empty (authorized-empty, not
// an error — §15: must not be confused with a fault).
func TestCodeSearchNoMatchEmpty(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeSearchTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "query": "nope-no-such-symbol"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// items slice is empty but the commit is still present.
	assert.Contains(t, res.Content[0].Text, "abc123")
}

// code_search missing required query → invalid params.
func TestCodeSearchInvalidParams(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeSearchTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// code_node resolves the seeded symbol; missing symbol → empty (nil node).
func TestCodeNodeSuccessAndMissing(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeNodeTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "symbol": "Serve"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "func Serve()")

	// Missing symbol → empty result, not an error.
	res2, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "symbol": "Ghost"})
	require.NoError(t, err)
	assert.Equal(t, "", res2.Content[0].Text)
}

// code_callers / code_callees return the seeded edge both ways.
func TestCodeCallersCallees(t *testing.T) {
	m, ac := newCodeTestClient(t)
	ctx := withAuth(context.Background(), ac)
	callers := NewCodeCallersTool(m)
	res, err := callers.Execute(ctx, map[string]any{"codebase_id": tcbCode, "symbol": "Serve"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "main") // main calls Serve

	callees := NewCodeCalleesTool(m)
	res2, err := callees.Execute(ctx, map[string]any{"codebase_id": tcbCode, "symbol": "main"})
	require.NoError(t, err)
	assert.Contains(t, res2.Content[0].Text, "Serve") // main → Serve
}

// code_impact returns the caller set for the seeded edge.
func TestCodeImpactSuccess(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeImpactTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "symbol": "Serve"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "main")
}

// code_explore returns hits + nodes for the query.
func TestCodeExploreSuccess(t *testing.T) {
	m, ac := newCodeTestClient(t)
	tl := NewCodeExploreTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"codebase_id": tcbCode, "query": "Serve"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "Serve")
	assert.Contains(t, res.Content[0].Text, "abc123")
}

// All eight tools: no-permission → empty success (no leak), never an error.
func TestCodeToolsNoPermissionEmpty(t *testing.T) {
	m, ac := newCodeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	ctx := withAuth(context.Background(), ac)
	args := func(extra map[string]any) map[string]any {
		out := map[string]any{"codebase_id": tcbCode}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	tools := []struct {
		name string
		exec func(context.Context, map[string]any) (*server.ToolCallResult, error)
		extra map[string]any
	}{
		{"status", NewCodeStatusTool(m).Execute, nil},
		{"files", NewCodeFilesTool(m).Execute, nil},
		{"search", NewCodeSearchTool(m).Execute, map[string]any{"query": "x"}},
		{"explore", NewCodeExploreTool(m).Execute, map[string]any{"query": "x"}},
		{"node", NewCodeNodeTool(m).Execute, map[string]any{"symbol": "x"}},
		{"callers", NewCodeCallersTool(m).Execute, map[string]any{"symbol": "x"}},
		{"callees", NewCodeCalleesTool(m).Execute, map[string]any{"symbol": "x"}},
		{"impact", NewCodeImpactTool(m).Execute, map[string]any{"symbol": "x"}},
	}
	for _, c := range tools {
		t.Run(c.name, func(t *testing.T) {
			res, err := c.exec(ctx, args(c.extra))
			require.NoError(t, err, "%s: no-permission must NOT be an error", c.name)
			assert.False(t, res.IsError, "%s: must not set isError", c.name)
			assert.Equal(t, "", res.Content[0].Text, "%s: must return empty content", c.name)
		})
	}
}
