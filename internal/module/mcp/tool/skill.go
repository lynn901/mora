package tool

// skill.go implements the four skill_* MCP tools (design-docs/19 §6.3,
// Phase 5-4). Each delegates to the upstream skill delivery + proposal
// service via MoraClient, enforcing token-scope gating locally while the
// agent-level binding gate is applied server-side by the DeliveryService.
//
// Existence never leaks (§8.2 / §6.4): an upstream ErrNotFound / ErrForbidden
// (a missing / unbound / summary-mode-refused skill) → an EMPTY success
// result, never an error to the Agent. skill_propose is a write: a read-only
// token scope is rejected with ErrScopeDenied before the upstream call, and a
// write-denied caller surfaces as empty (the caller cannot tell write-denied
// from missing — §8.2). No script execution occurs on any path (§4.4).

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// isSkillFault reports whether err is an upstream not-found / forbidden path
// that must collapse to an empty result (§8.2 no-leak). Skill delivery + the
// propose write-denial both surface as not-found/forbidden upstream.
func isSkillFault(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound) ||
		domainerr.Is(err, domainerr.ErrForbidden)
}

// --- skill_list ---

// SkillListTool implements skill_list (§6.3). Read-only: enumerates the
// skills the agent is bound to in the delegated workspace. An unbound agent /
// a workspace with no skills yields an empty list (no leak — §8.2).
type SkillListTool struct{ base }

// NewSkillListTool builds a skill_list tool.
func NewSkillListTool(client moraclient.MoraClient) *SkillListTool {
	return &SkillListTool{base: base{client: client}}
}

// Definition returns the tool schema. skill_list takes no arguments: the
// agent's delegated context (AgentID + WorkspaceID) scopes the query
// upstream (§11.2).
func (t *SkillListTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "skill_list",
		Description: "列出当前 Agent 在工作区中绑定的 Skill（只读）。未绑定或无权限时返回空列表，不泄露 Skill 是否存在。",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

// IsWrite is false — skill_list is read-only.
func (t *SkillListTool) IsWrite() bool { return false }

func (t *SkillListTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	res, err := t.client.SkillList(ctx, toMoraAuth(auth.FromContext(ctx)))
	if err != nil {
		if isSkillFault(err) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	if res == nil {
		return emptyTextResult(), nil
	}
	return asTextResult(res)
}

// --- skill_read ---

// SkillReadTool implements skill_read (§6.3). Read-only: returns the SKILL.md
// header + manifest, trimmed by the agent's binding delivery_mode (summary
// mode gets the capability summary, no raw file list). An unbound / missing
// skill → empty result (no leak — §8.2).
type SkillReadTool struct{ base }

func NewSkillReadTool(client moraclient.MoraClient) *SkillReadTool {
	return &SkillReadTool{base: base{client: client}}
}

func (t *SkillReadTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "skill_read",
		Description: "读取指定 Skill 的 SKILL.md 头部与资源清单（manifest），按 Agent 绑定的 delivery_mode 裁剪。无权限或不存在时返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"skill_id"},
			"properties": map[string]any{
				"skill_id": map[string]any{"type": "string", "description": "Skill 资产 id"},
				"version":  map[string]any{"type": "string", "description": "版本 id 或 latest（默认 latest）"},
			},
		},
	}
}

func (t *SkillReadTool) IsWrite() bool { return false }

func (t *SkillReadTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	skillID, err := requireString(args, "skill_id")
	if err != nil {
		return nil, err
	}
	versionSpec := optString(args, "version")
	res, err := t.client.SkillRead(ctx, toMoraAuth(auth.FromContext(ctx)), skillID, versionSpec)
	if err != nil {
		if isSkillFault(err) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	if res == nil {
		return emptyTextResult(), nil
	}
	return asTextResult(res)
}

// --- skill_resources ---

// SkillResourcesTool implements skill_resources (§6.3). Read-only: reads one
// declared resource file from the skill archive progressively. The binding's
// delivery_mode gates raw reads (inline/tool allow; summary refuses → empty).
// A non-manifest path / missing skill → empty result (no leak — §8.2).
type SkillResourcesTool struct{ base }

func NewSkillResourcesTool(client moraclient.MoraClient) *SkillResourcesTool {
	return &SkillResourcesTool{base: base{client: client}}
}

func (t *SkillResourcesTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "skill_resources",
		Description: "按清单路径渐进读取 Skill 的单个资源文件（只读）。summary 模式或无权限时返回空结果，不泄露文件存在性。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"skill_id", "path"},
			"properties": map[string]any{
				"skill_id": map[string]any{"type": "string", "description": "Skill 资产 id"},
				"path":     map[string]any{"type": "string", "description": "manifest 中的资源路径"},
				"version":  map[string]any{"type": "string", "description": "版本 id 或 latest（默认 latest）"},
			},
		},
	}
}

func (t *SkillResourcesTool) IsWrite() bool { return false }

// Execute streams the resource bytes. The upstream SkillResources call returns
// raw bytes (not an envelope), so a successful read is returned as a text
// content item with the bytes; the integrity hash + kind are surfaced as
// annotations when present. Every not-found / summary-refusal path collapses
// to an empty result (§8.2 no-leak).
func (t *SkillResourcesTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	skillID, err := requireString(args, "skill_id")
	if err != nil {
		return nil, err
	}
	resourcePath, err := requireString(args, "path")
	if err != nil {
		return nil, err
	}
	versionSpec := optString(args, "version")
	rc, err := t.client.SkillResources(ctx, toMoraAuth(auth.FromContext(ctx)), skillID, versionSpec, resourcePath)
	if err != nil {
		if isSkillFault(err) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	if rc == nil || len(rc.Content) == 0 {
		return emptyTextResult(), nil
	}
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: string(rc.Content)}},
	}, nil
}

// --- skill_propose ---

// SkillProposeTool implements skill_propose (§6.3). Write: the agent drafts a
// SKILL.md body and the tool lands a candidate proposal — it never publishes
// directly. A read-only token scope is rejected with ErrScopeDenied before the
// upstream call; a write-denied / no-context caller surfaces as empty (no
// leak — §8.2). The response carries the candidate + review references.
type SkillProposeTool struct{ base }

func NewSkillProposeTool(client moraclient.MoraClient) *SkillProposeTool {
	return &SkillProposeTool{base: base{client: client}}
}

func (t *SkillProposeTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "skill_propose",
		Description: "提交一个候选 Skill（草稿 SKILL.md），进入人工审核流程但不直接发布。需要写权限；普通查询不应触发此工具。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"workspace_id", "name", "draft_body"},
			"properties": map[string]any{
				"workspace_id": map[string]any{"type": "string", "description": "目标工作区 id"},
				"name":         map[string]any{"type": "string", "description": "Skill 名称"},
				"draft_body":    map[string]any{"type": "string", "description": "草稿 SKILL.md 内容"},
				"description":  map[string]any{"type": "string", "description": "Skill 描述（可选）"},
				"version":      map[string]any{"type": "string", "description": "版本号（可选）"},
				"source_ref":   map[string]any{"type": "object", "description": "来源引用载荷（可选）"},
			},
		},
	}
}

// IsWrite is true — skill_propose lands a candidate (write).
func (t *SkillProposeTool) IsWrite() bool { return true }

func (t *SkillProposeTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	if err := auth.CheckWriteScope(auth.FromContext(ctx)); err != nil {
		return nil, err
	}
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return nil, err
	}
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	draftBody, err := requireString(args, "draft_body")
	if err != nil {
		return nil, err
	}
	res, err := t.client.SkillPropose(ctx, toMoraAuth(auth.FromContext(ctx)), moraclient.SkillProposeRequest{
		WorkspaceID: wsID,
		Name:        name,
		DraftBody:   draftBody,
		Description: optString(args, "description"),
		Version:     optString(args, "version"),
		SourceRef:   optMap(args, "source_ref"),
	})
	if err != nil {
		// Write-denied surfaces upstream as not-found (the caller cannot tell
		// write-denied from missing — §8.2). A structural fault is a real error.
		if isSkillFault(err) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	return asTextResult(res)
}
