package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/audit"
	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/resource"
	"github.com/lynn901/mora/internal/module/mcp/server"
	"github.com/lynn901/mora/internal/module/mcp/tool"
	"github.com/lynn901/mora/internal/platform/rbac"
)

const (
	wsEng      = "ws-eng-0001"
	dirRoot    = "dir-eng-root-0001"
	docAPI     = "doc-api-0001"
	docOnb     = "doc-onboarding-0002"
	identity   = "user-1"
	sessionHdr = "Mcp-Session-Id"
)

// testEnv wires a Server with an in-memory mock MoraClient + memory stores and
// a seeded ACL.
type testEnv struct {
	engine   *gin.Engine
	srv      *server.Server
	mock     *moraclient.Mock
	tokens   *auth.MemoryTokenStore
	tokenRW  string
	tokenRO  string
	tokenBad string
	aud      *audit.MemoryStore
	limiter  *auth.MemoryRateLimiter
}

func newTestEnv(t *testing.T, rateRead, rateWrite int) *testEnv {
	t.Helper()
	mock := moraclient.NewMock()
	mock.AddWorkspace(moraclient.Workspace{ID: wsEng, Name: "工程团队", Slug: "eng", OwnerID: identity})
	mock.AddWorkspace(moraclient.Workspace{ID: "ws-sales-0001", Name: "销售团队", Slug: "sales", OwnerID: "user-2"})
	mock.AddDirectory(moraclient.DirectoryNode{ID: dirRoot, Name: "工程文档", Path: "", SortOrder: 1}, wsEng)
	mock.AddDocument(moraclient.DocumentMeta{
		ID: docAPI, WorkspaceID: wsEng, DirectoryID: dirRoot, Title: "API 设计规范",
		Status: "published", IndexStatus: "indexed", VersionNo: 5, Tags: []string{"api"},
		CreatedBy: identity, UpdatedAt: "2026-07-29T08:00:00Z",
	}, "# API 设计规范\n\n分页采用 page/page_size 参数。\n", "markdown",
		[]moraclient.VersionSummary{{VersionNo: 5, DiffSummary: "补充分页", AuthorID: identity, CreatedAt: "2026-07-29T08:00:00Z"}})
	mock.AddDocument(moraclient.DocumentMeta{
		ID: docOnb, WorkspaceID: wsEng, DirectoryID: "", Title: "新人入职指南",
		Status: "published", IndexStatus: "indexed", VersionNo: 2, Tags: []string{"guide"},
		CreatedBy: identity, UpdatedAt: "2026-07-25T08:00:00Z",
	}, "# 新人入职指南\n\n欢迎加入。\n", "markdown",
		[]moraclient.VersionSummary{{VersionNo: 2, AuthorID: identity, CreatedAt: "2026-07-25T08:00:00Z"}})
	mock.AddTags(wsEng, []moraclient.Tag{{ID: "tag-api", Name: "api"}})
	mock.GrantWrite(identity, wsEng)

	tokenStore := auth.NewMemoryTokenStore()
	env := &testEnv{
		mock:     mock,
		tokens:   tokenStore,
		aud:      audit.NewMemoryStore(),
		limiter:  auth.NewMemoryRateLimiter(),
		tokenRW:  "wki_test_rw_0001",
		tokenRO:  "wki_test_ro_0001",
		tokenBad: "wki_test_revoked_0001",
	}
	tokenStore.Add(auth.HashToken(env.tokenRW), &auth.TokenRecord{
		ID: "tok-rw", Name: "rw", Prefix: "wki_test_rw", IdentityType: rbac.IdentityUser,
		IdentityID: identity, IdentityName: "Dev", Scope: rbac.ScopeReadWrite,
	})
	tokenStore.Add(auth.HashToken(env.tokenRO), &auth.TokenRecord{
		ID: "tok-ro", Name: "ro", Prefix: "wki_test_ro", IdentityType: rbac.IdentityUser,
		IdentityID: identity, IdentityName: "Dev", Scope: rbac.ScopeReadOnly,
	})
	revokedAt := time.Now().UTC().Add(-time.Hour)
	tokenStore.Add(auth.HashToken(env.tokenBad), &auth.TokenRecord{
		ID: "tok-bad", Name: "bad", Prefix: "wki_test_re", IdentityType: rbac.IdentityUser,
		IdentityID: identity, IdentityName: "Dev", Scope: rbac.ScopeReadWrite, RevokedAt: &revokedAt,
	})

	resReg := resource.NewRegistry(mock)
	srv := server.NewServer(resReg, server.NewMemorySessionStore(), env.aud, env.limiter, "mora-mcp", "1.0.0",
		server.WithRateLimits(rateRead, rateWrite),
		server.WithProtocolVersion("2025-06-18"))
	srv.RegisterTool(tool.NewSearchTool(mock))
	srv.RegisterTool(tool.NewGetDocumentTool(mock))
	srv.RegisterTool(tool.NewListDocumentsTool(mock))
	srv.RegisterTool(tool.NewGetTagsTool(mock))
	srv.RegisterTool(tool.NewCreateDraftTool(mock))
	srv.RegisterTool(tool.NewUpdateDocumentTool(mock))
	env.srv = srv

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.Recovery())
	srv.PublicRoutes(r)
	authed := r.Group("/")
	authed.Use(auth.AuthMiddleware(tokenStore))
	srv.HTTPTransport(authed, env.aud)
	env.engine = r
	return env
}

func (e *testEnv) rpc(t *testing.T, token, method string, params any) *server.Response {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	if w.Code == http.StatusAccepted || w.Code == http.StatusUnauthorized {
		return nil
	}
	var resp server.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body=%s", w.Body.String())
	return &resp
}

func (e *testEnv) rpcRaw(t *testing.T, token string, body any) (*server.Response, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, req)
	if w.Code == http.StatusAccepted {
		return nil, w.Code
	}
	if w.Code == http.StatusUnauthorized {
		return nil, w.Code
	}
	var resp server.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return &resp, w.Code
}

// AC-14: MCP Server passes the standard initialize/capabilities handshake over HTTP.
func TestInitializeHandshake(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	raw := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"clientInfo":      map[string]any{"name": "test-agent", "version": "1.0"},
		}}
	resp, code := env.rpcRaw(t, env.tokenRW, raw)
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, resp)
	require.Nil(t, resp.Error, "err: %v", resp.Error)

	var initResult server.InitializeResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &initResult))
	assert.Equal(t, "2025-06-18", initResult.ProtocolVersion)
	assert.NotNil(t, initResult.Capabilities.Tools)
	assert.NotNil(t, initResult.Capabilities.Resources)
	assert.Equal(t, "mora-mcp", initResult.ServerInfo.Name)

	// The handshake must advertise the Mcp-Session-Id header.
	body, _ := json.Marshal(raw)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.NotEmpty(t, w.Header().Get(sessionHdr), "Mcp-Session-Id header must be set on initialize")
}

// AC-14: tools/list returns the registered tools.
func TestToolsList(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "tools/list", nil)
	require.Nil(t, resp.Error)
	var res server.ToolsListResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	names := toolNames(res.Tools)
	assert.Contains(t, names, "search_knowledge_base")
	assert.Contains(t, names, "get_document")
	assert.Contains(t, names, "create_draft")
	assert.Contains(t, names, "update_document")
}

// AC-16: search_knowledge_base returns structured results.
func TestSearchTool(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name":      "search_knowledge_base",
		"arguments": map[string]any{"query": "分页", "top_n": 5},
	})
	require.Nil(t, resp.Error, "err: %v", resp.Error)
	var res server.ToolCallResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	assert.False(t, res.IsError)
	assert.Len(t, res.Content, 1)
	assert.Contains(t, res.Content[0].Text, docAPI)
}

// Existence-leak: search with no visible workspace returns empty, not error.
func TestSearchNoPermissionEmpty(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name":      "search_knowledge_base",
		"arguments": map[string]any{"query": "anything", "workspace_id": "ws-sales-0001"},
	})
	require.Nil(t, resp.Error)
	var res server.ToolCallResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	var sr map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].Text), &sr))
	items, _ := sr["items"].([]any)
	assert.Empty(t, items)
}

// Existence-leak: get_document with no permission returns empty, NOT 403/404.
func TestGetDocumentNoPermissionIsEmpty(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	env.mock.RevokeRead(identity, wsEng)
	resp := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name":      "get_document",
		"arguments": map[string]any{"document_id": docAPI},
	})
	require.Nil(t, resp.Error, "must not surface a JSON-RPC error (existence leak)")
	var res server.ToolCallResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text, "empty content, no existence hint")
}

// AC-17: create_draft with a readonly token is scope-denied.
func TestCreateDraftScopeDenied(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRO, "tools/call", map[string]any{
		"name":      "create_draft",
		"arguments": map[string]any{"workspace_id": wsEng, "title": "T", "content": "C"},
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, server.ErrCodeScopeDenied, resp.Error.Code)
}

// AC-17: create_draft with readwrite token produces a draft (review URL, not published).
func TestCreateDraftSuccess(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name":      "create_draft",
		"arguments": map[string]any{"workspace_id": wsEng, "title": "新草稿", "content": "# 草稿"},
	})
	require.Nil(t, resp.Error, "err: %v", resp.Error)
	var res server.ToolCallResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "draft_id")
	assert.Contains(t, res.Content[0].Text, "review_url")
}

// AC-18: missing token → 401; revoked token → 401.
func TestAuthRejected(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	resp := env.rpc(t, env.tokenBad, "ping", nil)
	assert.Nil(t, resp, "revoked token must be rejected")
}

// AC-19: a token revoked at runtime is rejected on the next request.
func TestTokenRevocationAtRuntime(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "ping", nil)
	require.Nil(t, resp.Error)
	now := time.Now().UTC()
	env.tokens.Add(auth.HashToken(env.tokenRW), &auth.TokenRecord{
		ID: "tok-rw", Name: "rw", Prefix: "wki_test_rw", IdentityType: rbac.IdentityUser,
		IdentityID: identity, IdentityName: "Dev", Scope: rbac.ScopeReadWrite, RevokedAt: &now,
	})
	resp = env.rpc(t, env.tokenRW, "ping", nil)
	assert.Nil(t, resp, "revoked token must be rejected")
}

// AC-19 / rate limiting: exceeding the read bucket returns a rate-limit error.
func TestRateLimitRead(t *testing.T) {
	env := newTestEnv(t, 2, 20)
	rateLimitedSeen := false
	for i := 0; i < 5; i++ {
		r := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
			"name": "search_knowledge_base", "arguments": map[string]any{"query": "分页"},
		})
		if r != nil && r.Error != nil && r.Error.Code == server.ErrCodeRateLimited {
			rateLimitedSeen = true
		}
	}
	assert.True(t, rateLimitedSeen, "expected a rate-limit error after exceeding 2 req/min")
}

// AC-15: resources/list returns visible workspace resources.
func TestResourcesList(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "resources/list", nil)
	require.Nil(t, resp.Error)
	var res server.ResourcesListResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	uris := resourceURIs(res.Resources)
	assert.Contains(t, uris, "wiki://workspaces")
	assert.Contains(t, uris, "wiki://workspaces/"+wsEng+"/tree")
}

// AC-15: resources/read returns document metadata; no-permission returns empty.
func TestResourcesReadMeta(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "resources/read", map[string]any{"uri": "wiki://documents/" + docAPI + "/meta"})
	require.Nil(t, resp.Error)
	var res server.ResourceReadResult
	b, _ := json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	require.Len(t, res.Contents, 1)
	assert.Contains(t, res.Contents[0].Text, docAPI)

	env.mock.RevokeRead(identity, wsEng)
	resp = env.rpc(t, env.tokenRW, "resources/read", map[string]any{"uri": "wiki://documents/" + docAPI + "/meta"})
	require.Nil(t, resp.Error)
	b, _ = json.Marshal(resp.Result)
	require.NoError(t, json.Unmarshal(b, &res))
	assert.Empty(t, res.Contents)
}

// Unknown method → -32601.
func TestUnknownMethod(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "nonexistent/method", nil)
	require.NotNil(t, resp.Error)
	assert.Equal(t, server.ErrCodeMethodNotFound, resp.Error.Code)
}

// Audit: every tool call is recorded.
func TestAuditRecorded(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	_ = env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name": "search_knowledge_base", "arguments": map[string]any{"query": "分页"},
	})
	records, err := env.aud.List(context.Background(), audit.Filter{})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(records), 1)
	assert.Equal(t, "search_knowledge_base", records[0].ToolName)
	assert.Equal(t, audit.StatusSuccess, records[0].ResultStatus)
}

// Health endpoint is public (no token).
func TestHealthPublic(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	req := httptest.NewRequest(http.MethodGet, "/mcp/health", nil)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "ok")
}

// tools/call with invalid params → -32602.
func TestInvalidParams(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name": "search_knowledge_base", "arguments": map[string]any{},
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, server.ErrCodeInvalidParams, resp.Error.Code)
}

// Audit redacts sensitive params and truncates long content.
func TestAuditParamRedaction(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	_ = env.rpc(t, env.tokenRW, "tools/call", map[string]any{
		"name": "create_draft",
		"arguments": map[string]any{
			"workspace_id": wsEng, "title": "T", "content": strings.Repeat("x", 500),
			"token": "supersecret",
		},
	})
	records, _ := env.aud.List(context.Background(), audit.Filter{})
	require.GreaterOrEqual(t, len(records), 1)
	rec := records[0]
	assert.Equal(t, "[redacted]", rec.ParamsSummary["token"])
	ct, _ := rec.ParamsSummary["content"].(string)
	assert.Less(t, len(ct), 300, "long content must be truncated in audit summary")
}

// notifications/initialized produces no response (accepted, empty).
func TestNotificationNoResponse(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// ping returns an empty result.
func TestPing(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	resp := env.rpc(t, env.tokenRW, "ping", nil)
	require.Nil(t, resp.Error)
}

// DELETE /mcp ends the session without error.
func TestDeleteSession(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set(sessionHdr, "sess-to-delete")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// JSON-RPC batch: multiple requests return an array of responses.
func TestBatchRequest(t *testing.T) {
	env := newTestEnv(t, 100, 20)
	batch := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "ping"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
	}
	raw, _ := json.Marshal(batch)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var resps []server.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resps))
	assert.Len(t, resps, 2)
}

func toolNames(defs []server.ToolDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

func resourceURIs(defs []server.ResourceDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.URI
	}
	return out
}
