package tool

// knowledge_test.go covers the knowledge_search + knowledge_context MCP tools
// (design-docs/19 §11.4, Phase 6-5). The tests pin:
//   - §8.2 no-leak: an authorized-empty / no-permission outcome is an EMPTY
//     success, never an error (the Agent cannot infer whether the workspace
//     has knowledge).
//   - §11.5 three-state: a provider fault surfaces as an EMPTY result + a
//     diagnostic note, never a faked result and never confused with
//     genuine empty.
//   - §11.4 aliasing: the legacy search_knowledge_base / get_document /
//     list_documents tool defs carry Deprecated=true; knowledge_search /
//     knowledge_context do not; the type-specific surfaces (get_tags,
//     code_*, skill_*) are NOT marked deprecated (D12 — not flattened).
//   - invalid-params: missing query / workspace_id → ErrInvalidParams.
//   - IsWrite: both knowledge_* tools are read-only (read bucket, readonly
//     tokens admitted).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// newKnowledgeTestClient seeds a mock with one knowledge result in the eng
// workspace, grants the test user read, and returns the client + a read/write
// auth context. The seeded response carries one candidate so the success path
// is exercisable.
func newKnowledgeTestClient(t *testing.T) (*moraclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	score := 0.9
	m.AddKnowledgeResult(moraclient.MockKnowledgeResult{
		WorkspaceID: twsEng,
		Response: moraclient.KnowledgeContextResponse{
			Intent:        "spec",
			PolicyVersion: 1,
			AuthzRevision: 7,
			Candidates: []moraclient.KnowledgeCandidateResponse{
				{
					AssetID: "asset-t-mem-0001", AssetType: "memory", Title: "mem",
					Snippet: "分页", Score: 0.8, Authority: 0.8,
					Citation: moraclient.KnowledgeCitationResponse{
						AssetID: "asset-t-mem-0001", AssetType: "memory", Authority: 0.8,
						Confidence: &score,
					},
				},
			},
		},
	})
	m.GrantWrite(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadWrite,
	}
	return m, ac
}

// --- knowledge_search ---

// knowledge_search with permission returns the Broker candidates envelope.
func TestKnowledgeSearchSuccess(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "asset-t-mem-0001")
	assert.Contains(t, res.Content[0].Text, "policy_version")
}

// knowledge_search without permission returns EMPTY success (no leak). The
// caller cannot infer whether the workspace has knowledge (§8.2).
func TestKnowledgeSearchNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError, "no-permission must NOT set isError")
	assert.Equal(t, "", res.Content[0].Text, "must return empty content")
}

// knowledge_search for an authorized workspace that yields no candidates
// returns empty (no existence hint) — a genuine authorized-empty is a 200
// empty list (§8.2 / §7.3). Distinct from no-permission: the caller IS a
// member, the Broker ran, but the memory ports surfaced nothing.
func TestKnowledgeSearchAuthorizedEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	// A second workspace the caller can read but with an empty candidate list
	// (the Broker ran the pipeline; no candidates survived).
	const wsEmpty = "ws-t-empty-0001"
	m.AddWorkspace(moraclient.Workspace{ID: wsEmpty, Name: "空", Slug: "empty"})
	m.GrantRead(tuser, wsEmpty)
	m.AddKnowledgeResult(moraclient.MockKnowledgeResult{
		WorkspaceID: wsEmpty,
		Response: moraclient.KnowledgeContextResponse{
			Intent:     "spec",
			Candidates: []moraclient.KnowledgeCandidateResponse{},
		},
	})
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": wsEmpty,
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// knowledge_search with a provider fault returns EMPTY + a diagnostic note
// (§11.5) — never an error, never a faked result, distinguishable from a
// genuine empty.
func TestKnowledgeSearchProviderFaultDiagnostic(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	// Replace the seeded result with a provider-fault marker.
	m.AddKnowledgeResult(moraclient.MockKnowledgeResult{
		WorkspaceID:   twsEng,
		ProviderFault: true,
	})
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err, "a provider fault must NOT be a tool error")
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "unavailable",
		"a fault must carry a diagnostic note distinguishing it from genuine empty")
}

// knowledge_search missing query → invalid params.
func TestKnowledgeSearchMissingQuery(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng,
	})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_search missing workspace_id → invalid params (AC-4: the workspace
// scopes the convergence retrieval; it is required).
func TestKnowledgeSearchMissingWorkspace(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeSearchTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页",
	})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_search is read-only — a readonly token is admitted.
func TestKnowledgeSearchReadOnlyAdmitted(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	ac.Scope = rbac.ScopeReadOnly
	tool := NewKnowledgeSearchTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err)
	assert.Contains(t, res.Content[0].Text, "asset-t-mem-0001")
}

// --- knowledge_context ---

// knowledge_context with permission returns the full §11.3 envelope (intent +
// policy_version + authz_revision + candidates + truncation when present).
func TestKnowledgeContextSuccess(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.AddKnowledgeResult(moraclient.MockKnowledgeResult{
		WorkspaceID: twsEng,
		Response: moraclient.KnowledgeContextResponse{
			Intent: "spec", PolicyVersion: 2, AuthzRevision: 9,
			Candidates: []moraclient.KnowledgeCandidateResponse{
				{AssetID: "asset-t-doc-0002", AssetType: "document", Title: "API",
					Snippet: "分页", Score: 0.7, Authority: 0.7},
			},
			Truncation: moraclient.KnowledgeTruncation{
				Reason:        "budget",
				ContinueTools: []string{"get_document"},
			},
		},
	})
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
		"asset_types": []string{"document"}, "max_tokens": 4000,
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "asset-t-doc-0002")
	assert.Contains(t, res.Content[0].Text, "policy_version")
	assert.Contains(t, res.Content[0].Text, "budget")
	assert.Contains(t, res.Content[0].Text, "get_document")
}

// knowledge_context without permission returns EMPTY success (§8.2 no-leak).
func TestKnowledgeContextNoPermissionEmpty(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// knowledge_context provider fault → empty + diagnostic (§11.5).
func TestKnowledgeContextProviderFaultDiagnostic(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	m.AddKnowledgeResult(moraclient.MockKnowledgeResult{
		WorkspaceID:   twsEng,
		ProviderFault: true,
	})
	tool := NewKnowledgeContextTool(m)
	res, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{
		"query": "分页", "workspace_id": twsEng,
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "unavailable")
}

// knowledge_context missing required params → invalid params.
func TestKnowledgeContextInvalidParams(t *testing.T) {
	m, ac := newKnowledgeTestClient(t)
	tool := NewKnowledgeContextTool(m)
	_, err := tool.Execute(withAuth(context.Background(), ac), map[string]any{})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}

// knowledge_context is read-only.
func TestKnowledgeContextIsWriteFalse(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	assert.False(t, NewKnowledgeContextTool(m).IsWrite())
}

// knowledge_search is read-only.
func TestKnowledgeSearchIsWriteFalse(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	assert.False(t, NewKnowledgeSearchTool(m).IsWrite())
}

// --- §11.4 aliasing / deprecation ---

// The legacy alias tools are marked deprecated in their ToolDef; the new
// knowledge_* tools + the non-aliased surfaces are NOT deprecated (D12 — type-
// specific tools are not flattened, so get_tags / code_* / skill_* stay clean).
func TestKnowledgeToolDeprecationMarking(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	// §11.4 compatibility aliases — deprecated.
	assert.True(t, NewSearchTool(m).Definition().Deprecated, "search_knowledge_base is a §11.4 deprecated alias")
	assert.True(t, NewGetDocumentTool(m).Definition().Deprecated, "get_document is a §11.4 deprecated alias")
	assert.True(t, NewListDocumentsTool(m).Definition().Deprecated, "list_documents is a §11.4 deprecated alias")
	// New knowledge convergence tools — not deprecated.
	assert.False(t, NewKnowledgeSearchTool(m).Definition().Deprecated, "knowledge_search is the replacement, not deprecated")
	assert.False(t, NewKnowledgeContextTool(m).Definition().Deprecated, "knowledge_context is the replacement, not deprecated")
	// D12: type-specific surfaces are NOT flattened / not deprecated.
	assert.False(t, NewGetTagsTool(m).Definition().Deprecated, "get_tags is not a §11.4 alias")
	assert.False(t, NewCodeStatusTool(m).Definition().Deprecated, "code_* tools are not flattened (D12)")
	assert.False(t, NewSkillListTool(m).Definition().Deprecated, "skill_* tools are not flattened (D12)")
}

// The two knowledge_* tools carry the expected names + a workspace_id-required
// schema (AC-4 isolation: the workspace scopes every convergence retrieval).
func TestKnowledgeToolDefinitions(t *testing.T) {
	m, _ := newKnowledgeTestClient(t)
	ks := NewKnowledgeSearchTool(m).Definition()
	assert.Equal(t, "knowledge_search", ks.Name)
	required, _ := ks.InputSchema["required"].([]string)
	assert.Contains(t, required, "query")
	assert.Contains(t, required, "workspace_id")

	kc := NewKnowledgeContextTool(m).Definition()
	assert.Equal(t, "knowledge_context", kc.Name)
	required2, _ := kc.InputSchema["required"].([]string)
	assert.Contains(t, required2, "workspace_id")
}
