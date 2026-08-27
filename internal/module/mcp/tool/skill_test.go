package tool

// skill_test.go covers the four skill_* MCP tools (design-docs/19 §6.3,
// Phase 5-4). The tests pin the §8.2 no-leak contract (every not-found /
// denied / summary-refusal path → EMPTY success, never an error to the
// Agent), the delivery_mode trim (summary hides the raw manifest), the
// progressive-read gate, and the skill_propose write-scope + write-permission
// gates. They mirror code_test.go's structure so a regression that surfaces
// a 403-style leak is caught here.

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

const (
	tskill = "skill-t-echo-0001" // a seeded skill id in the eng workspace
)

// newSkillTestClient seeds a mock with one tool-delivery skill in the eng
// workspace, grants the test user read, and returns the client + a read-only
// auth context. Callers needing write flip ac.Scope to ScopeReadWrite.
func newSkillTestClient(t *testing.T) (*moraclient.Mock, *auth.AuthContext) {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m.AddSkill(moraclient.MockSkill{
		ID:           tskill,
		WorkspaceID:  twsEng,
		DeliveryMode: "tool",
		Header:       map[string]any{"name": "echo-skill", "version": "1.0", "description": "echoes"},
		Manifest: []moraclient.SkillFileEntry{
			{Path: "SKILL.md", Size: 12, Hash: "h1", Kind: "skill_md"},
			{Path: "assets/guide.md", Size: 7, Hash: "h2", Kind: "asset"},
		},
		Resources: map[string][]byte{
			"SKILL.md":         []byte("# echo\n"),
			"assets/guide.md":  []byte("guide\n"),
		},
		VersionNo: 1,
	})
	m.GrantRead(tuser, twsEng)
	ac := &auth.AuthContext{
		TokenID: "tok-t", IdentityType: rbac.IdentityUser, IdentityID: tuser,
		IdentityName: "T", Scope: rbac.ScopeReadOnly,
	}
	return m, ac
}

// --- skill_list ---

// skill_list with read permission returns the seeded skill carrying its name.
func TestSkillListSuccess(t *testing.T) {
	m, ac := newSkillTestClient(t)
	tl := NewSkillListTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "echo-skill")
	assert.Contains(t, res.Content[0].Text, "tool") // delivery_mode
}

// skill_list without permission returns an EMPTY list (no existence leak).
// An unbound agent and a workspace with no skills are indistinguishable: both
// yield {"items":[],"total":0} — an empty list, never an error.
func TestSkillListNoPermissionEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewSkillListTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	// Empty list JSON — the leak-safe shape (no skill names surface).
	assert.Contains(t, res.Content[0].Text, "[]")
	assert.NotContains(t, res.Content[0].Text, "echo-skill")
}

// skill_list for an unbound agent (no skills) returns empty (no leak).
func TestSkillListEmpty(t *testing.T) {
	_, ac := newSkillTestClient(t)
	// A fresh workspace with no skills: add a second workspace the agent can
	// read but holds no skills. Grant read on it, keep the seeded skill in the
	// other workspace. We instead verify the empty-list shape by revoking the
	// only grant — already covered above. This test asserts the empty JSON
	// shape for a zero-skill result path (an unbound agent).
	m2 := moraclient.NewMock()
	m2.AddWorkspace(moraclient.Workspace{ID: twsEng, Name: "工程", Slug: "eng"})
	m2.GrantRead(tuser, twsEng)
	tl := NewSkillListTool(m2)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	// An empty list surfaces as a non-empty JSON ({"items":[],"total":0}),
	// distinct from the no-permission empty string. Both are leak-safe.
	assert.Contains(t, res.Content[0].Text, "[]")
}

// --- skill_read ---

// skill_read with permission returns the header + manifest (tool mode).
func TestSkillReadSuccess(t *testing.T) {
	m, ac := newSkillTestClient(t)
	tl := NewSkillReadTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"skill_id": tskill})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "echo-skill")
	assert.Contains(t, res.Content[0].Text, "assets/guide.md") // manifest present
	assert.Contains(t, res.Content[0].Text, "tool")          // delivery_mode
}

// skill_read without permission returns EMPTY success (no existence leak).
func TestSkillReadNoPermissionEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewSkillReadTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"skill_id": tskill})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// skill_read for a missing skill returns empty (no existence hint).
func TestSkillReadMissingEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	tl := NewSkillReadTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"skill_id": "no-such-skill"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// skill_read in summary mode hides the raw manifest (delivery_mode trim).
func TestSkillReadSummaryHidesManifest(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.AddSkill(moraclient.MockSkill{
		ID: "skill-t-summary-0001", WorkspaceID: twsEng, DeliveryMode: "summary",
		Header: map[string]any{"name": "summary-skill"},
		Manifest: []moraclient.SkillFileEntry{
			{Path: "SKILL.md", Size: 3, Hash: "hs", Kind: "skill_md"},
		},
		Resources: map[string][]byte{"SKILL.md": []byte("x\n")},
	})
	tl := NewSkillReadTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{"skill_id": "skill-t-summary-0001"})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "summary")
	assert.NotContains(t, res.Content[0].Text, "SKILL.md") // no raw file list
}

// --- skill_resources ---

// skill_resources with permission returns the resource bytes.
func TestSkillResourcesSuccess(t *testing.T) {
	m, ac := newSkillTestClient(t)
	tl := NewSkillResourcesTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"skill_id": tskill, "path": "assets/guide.md",
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "guide\n", res.Content[0].Text)
}

// skill_resources without permission returns EMPTY success (no leak).
func TestSkillResourcesNoPermissionEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.RevokeRead(tuser, twsEng)
	tl := NewSkillResourcesTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"skill_id": tskill, "path": "assets/guide.md",
	})
	require.NoError(t, err, "no-permission must NOT be an error")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// skill_resources for a non-manifest path returns empty (no leak).
func TestSkillResourcesNonManifestPathEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	tl := NewSkillResourcesTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"skill_id": tskill, "path": "../etc/passwd", // traversal attempt
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// skill_resources in summary mode refuses raw reads → empty (no leak).
func TestSkillResourcesSummaryRefusesEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.AddSkill(moraclient.MockSkill{
		ID: "skill-t-summary-0001", WorkspaceID: twsEng, DeliveryMode: "summary",
		Header:   map[string]any{"name": "summary-skill"},
		Manifest: []moraclient.SkillFileEntry{{Path: "SKILL.md", Size: 3, Hash: "hs", Kind: "skill_md"}},
		Resources: map[string][]byte{"SKILL.md": []byte("x\n")},
	})
	tl := NewSkillResourcesTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"skill_id": "skill-t-summary-0001", "path": "SKILL.md",
	})
	require.NoError(t, err, "summary-mode refusal must NOT be an error")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// --- skill_propose ---

// skill_propose with write permission lands a candidate (never publishes).
func TestSkillProposeSuccess(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.GrantWrite(tuser, twsEng) // promote to write
	ac.Scope = rbac.ScopeReadWrite
	tl := NewSkillProposeTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "name": "draft-skill", "draft_body": "# draft\n",
	})
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Contains(t, res.Content[0].Text, "candidate")
	assert.Contains(t, res.Content[0].Text, "review_request_id")
}

// skill_propose with readonly scope → scope denied (write tool).
func TestSkillProposeScopeDenied(t *testing.T) {
	m, ac := newSkillTestClient(t)
	ac.Scope = rbac.ScopeReadOnly
	tl := NewSkillProposeTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "name": "draft-skill", "draft_body": "# draft\n",
	})
	assert.ErrorIs(t, err, domainerr.ErrScopeDenied)
}

// skill_propose without write permission (read-only grant, write scope) →
// EMPTY success (no leak — the caller cannot tell write-denied from missing).
func TestSkillProposeNoPermissionEmpty(t *testing.T) {
	m, ac := newSkillTestClient(t)
	// Grant read only (not write); scope is readwrite so the scope gate passes
	// and the write-permission gate surfaces the no-leak empty result.
	m.RevokeRead(tuser, twsEng)
	m.GrantRead(tuser, twsEng)
	ac.Scope = rbac.ScopeReadWrite
	tl := NewSkillProposeTool(m)
	res, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "name": "draft-skill", "draft_body": "# draft\n",
	})
	require.NoError(t, err, "write-denied must NOT be an error (no leak)")
	assert.False(t, res.IsError)
	assert.Equal(t, "", res.Content[0].Text)
}

// skill_propose missing required params → invalid params error.
func TestSkillProposeInvalidParams(t *testing.T) {
	m, ac := newSkillTestClient(t)
	m.GrantWrite(tuser, twsEng)
	ac.Scope = rbac.ScopeReadWrite
	tl := NewSkillProposeTool(m)
	_, err := tl.Execute(withAuth(context.Background(), ac), map[string]any{
		"workspace_id": twsEng, "name": "draft-skill", // missing draft_body
	})
	assert.ErrorIs(t, err, domainerr.ErrInvalidParams)
}
