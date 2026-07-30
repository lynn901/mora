package tool

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wiki/wiki-backend/internal/module/mcp/auth"
	"github.com/wiki/wiki-backend/internal/module/mcp/wikiclient"
	domainerr "github.com/wiki/wiki-backend/internal/pkg/errors"
	"github.com/wiki/wiki-backend/internal/platform/rbac"
)

const (
	twsEng  = "ws-t-eng-0001"
	tdocAPI = "doc-t-api-0001"
	tuser   = "user-t-1"
)

func newTestClient(t *testing.T) (*wikiclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := wikiclient.NewMock()
	m.AddWorkspace(wikiclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m.AddDocument(wikiclient.DocumentMeta{
		ID: tdocAPI, WorkspaceID: twsEng, Title: "API 规范", Status: "published",
		IndexStatus: "indexed", VersionNo: 1, CreatedBy: tuser,
	}, "# API 规范\n\n分页。\n", "markdown", nil)
	m.GrantWrite(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadWrite,
	}
	return m, ac
}

func withAuth(ctx context.Context, ac *auth.AuthContext) context.Context {
	return auth.WithAuthContext(ctx, ac)
}

// get_document with permission returns the body.
func TestGetDocumentSuccess(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewGetDocumentTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"document_id": tdocAPI})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, tdocAPI)
	assert.Contains(t, res.Content[0].Text, "分页")
}

// get_document without permission returns EMPTY success (existence leak prevention).
func TestGetDocumentNoPermissionEmpty(t *testing.T) {
	m, ac := newTestClient(t)
	// Remove the grant so user has no read access.
	m.RevokeRead(tuser, twsEng)
	tool := NewGetDocumentTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"document_id": tdocAPI})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError, "no-permission must NOT set isError")
	assert.Equal(t, "", res.Content[0].Text, "must return empty content")
}

// get_document for a non-existent doc also returns empty (no existence hint).
func TestGetDocumentMissingEmpty(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewGetDocumentTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"document_id": "does-not-exist"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// get_document missing required param → invalid params error.
func TestGetDocumentInvalidParams(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewGetDocumentTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// search returns hits for accessible docs.
func TestSearchSuccess(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"query": "分页"})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, tdocAPI)
}

// create_draft with readonly scope → scope denied.
func TestCreateDraftScopeDenied(t *testing.T) {
	m, ac := newTestClient(t)
	ac.Scope = rbac.ScopeReadOnly
	tool := NewCreateDraftTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "title": "T", "content": "C",
	})
	assert.ErrorIs(t, err, domainerr.ErrScopeDenied)
}

// create_draft with readwrite scope → draft created with a review URL.
func TestCreateDraftSuccess(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewCreateDraftTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "title": "新草稿", "content": "# 草稿",
	})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "draft_id")
	assert.Contains(t, res.Content[0].Text, "review_url")
}

// update_document produces a new version draft.
func TestUpdateDocumentSuccess(t *testing.T) {
	m, ac := newTestClient(t)
	tool := NewUpdateDocumentTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"document_id": tdocAPI, "content": "# 更新内容",
	})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "review_url")
}

// list_documents on an inaccessible workspace returns empty (not error).
func TestListDocumentsNoPermissionEmpty(t *testing.T) {
	m, ac := newTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewListDocumentsTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"workspace_id": twsEng})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, `"total":0`)
}

// get_tags on an inaccessible workspace returns empty (not error).
func TestGetTagsNoPermissionEmpty(t *testing.T) {
	m, ac := newTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewGetTagsTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{"workspace_id": twsEng})
	require.NoError(t, err)
	// Empty array.
	assert.Equal(t, "[]", res.Content[0].Text)
}

// IsWrite flags for tools.
func TestToolWriteFlags(t *testing.T) {
	m, _ := newTestClient(t)
	assert.False(t, NewSearchTool(m).IsWrite())
	assert.False(t, NewGetDocumentTool(m).IsWrite())
	assert.False(t, NewListDocumentsTool(m).IsWrite())
	assert.False(t, NewGetTagsTool(m).IsWrite())
	assert.True(t, NewCreateDraftTool(m).IsWrite())
	assert.True(t, NewUpdateDocumentTool(m).IsWrite())
}

// Tool definitions carry the expected names + input schemas.
func TestToolDefinitions(t *testing.T) {
	m, _ := newTestClient(t)
	expected := map[string]bool{
		"search_knowledge_base": false,
		"get_document":          false,
		"list_documents":        false,
		"get_tags":              false,
		"create_draft":          false,
		"update_document":       false,
	}
	// Each tool's Definition() returns a server.ToolDef whose .Name we check.
	check := func(name string, got string) {
		assert.Equal(t, name, got)
		if _, ok := expected[name]; ok {
			expected[name] = true
		}
	}
	check("search_knowledge_base", NewSearchTool(m).Definition().Name)
	check("get_document", NewGetDocumentTool(m).Definition().Name)
	check("list_documents", NewListDocumentsTool(m).Definition().Name)
	check("get_tags", NewGetTagsTool(m).Definition().Name)
	check("create_draft", NewCreateDraftTool(m).Definition().Name)
	check("update_document", NewUpdateDocumentTool(m).Definition().Name)
	for name, seen := range expected {
		assert.True(t, seen, "tool %s not registered", name)
	}
}
