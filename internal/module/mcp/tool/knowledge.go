package tool

// knowledge.go implements the two Phase 6 converged-retrieval MCP tools
// (design-docs/19 §11.4, Phase 6-5):
//   - knowledge_search  — converged retrieval (routes by intent, fans out to
//     the typed query ports, dedups keeping conflicts, returns ranked
//     candidates; no context assembly).
//   - knowledge_context — context assembly (the search path plus the budgeter
//     + citation builder; returns a budgeted catalog + a truncation report
//     naming the continue-tools for progressive reads).
//
// Both delegate to the Context Broker's internal API via MoraClient (§3.2:
// the mcp module only depends on moraclient, never imports knowledge/context).
// RBAC is the Broker's hard two-pass constraint (§0 D10); the tool layer only
// propagates the caller identity. Existence never leaks (§8.2): an
// unauthorized / empty-authorized / no-result caller gets an empty success
// result — the three states stay distinguishable on the response shape (§7.3),
// never an error to the Agent. Type-specialized tools (code_callers/impact,
// memory_evidence_read, skill_resources) are NOT flattened by these tools
// (D12): they remain independent tools the Agent calls for body reads.

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// isBrokerFault reports whether err is an upstream not-found / forbidden path
// that must collapse to an empty result (§8.2 no-leak). The Broker maps a
// no-permission / empty-authorized caller to a 200 with an empty candidate
// slice, so this only catches the rare transport-level 403/404 a misconfigured
// internal route could emit — it never surfaces to the Agent as an error.
func isBrokerFault(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound) ||
		domainerr.Is(err, domainerr.ErrForbidden)
}

// --- knowledge_search ---

// KnowledgeSearchTool implements knowledge_search (§11.4). Read-only: converged
// retrieval across document/codebase/memory/skill, routed by intent, deduped
// with conflicts kept side-by-side. No permission / no matches → empty result
// (the three empty states stay distinguishable on the response shape, §7.3).
type KnowledgeSearchTool struct{ base }

// NewKnowledgeSearchTool builds a knowledge_search tool.
func NewKnowledgeSearchTool(client moraclient.MoraClient) *KnowledgeSearchTool {
	return &KnowledgeSearchTool{base: base{client: client}}
}

// Definition returns the tool schema. asset_types empty = the Broker's
// IntentRouter picks (§4.2 rule 1); the Broker enforces RBAC two-pass (§0 D10).
func (t *KnowledgeSearchTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_search",
		Description: "跨文档/代码/记忆/Skill 的收敛检索（Context Broker 路由+去重+冲突保留，不组装上下文）。结果严格遵循调用方权限；无权限或无结果返回空，三状态可区分。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "自然语言查询"},
				"workspace_id": map[string]any{"type": "string", "description": "限定工作区（可选）"},
				"asset_types": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "enum": []string{"document", "codebase", "memory", "skill"}},
					"description": "显式类型集（可选，空=Broker 按意图路由）",
				},
				"directory_id": map[string]any{"type": "string", "description": "限定目录（可选）"},
				"tags":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_items":    map[string]any{"type": "integer", "default": 20},
				"timeout_ms":   map[string]any{"type": "integer", "default": 2000},
			},
		},
	}
}

// IsWrite is false — knowledge_search is read-only.
func (t *KnowledgeSearchTool) IsWrite() bool { return false }

// Execute runs the converged search. Missing query → invalid params. Empty
// candidates are a normal success (no permission is not distinguishable from
// no matches at the tool layer; the response shape carries the three-state
// signals from the Broker, §7.3).
func (t *KnowledgeSearchTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	req := moraclient.KnowledgeSearchRequest{
		Query:       query,
		WorkspaceID: optString(args, "workspace_id"),
		AssetTypes:  optStringSlice(args, "asset_types"),
		DirectoryID: optString(args, "directory_id"),
		Tags:        optStringSlice(args, "tags"),
		MaxItems:    optInt(args, "max_items", 0),
		TimeoutMs:   optInt(args, "timeout_ms", 0),
	}
	res, err := t.client.KnowledgeSearch(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if isBrokerFault(err) {
			return asTextResult(&moraclient.KnowledgeSearchResult{Candidates: []moraclient.KnowledgeCandidate{}})
		}
		return nil, err
	}
	if res == nil {
		res = &moraclient.KnowledgeSearchResult{Candidates: []moraclient.KnowledgeCandidate{}}
	}
	return asTextResult(res)
}

// --- knowledge_context ---

// KnowledgeContextTool implements knowledge_context (§11.3/§11.4). Read-only:
// assembles a budgeted context with citations — directory + summary + citation
// by default (the body is a progressive read via the type tool, §6.2). A
// truncation report names the continue-tools when candidates are dropped
// (never silently truncates a citation, §6.2).
type KnowledgeContextTool struct{ base }

// NewKnowledgeContextTool builds a knowledge_context tool.
func NewKnowledgeContextTool(client moraclient.MoraClient) *KnowledgeContextTool {
	return &KnowledgeContextTool{base: base{client: client}}
}

// Definition returns the tool schema. include_content defaults false (§6.2);
// max_tokens/max_items 0 = workspace default (§6.3).
func (t *KnowledgeContextTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_context",
		Description: "组装上下文（Context Broker 路由+预算+引用）：默认返回资产目录+摘要+引用，预算不足时返回截断原因与继续读取工具，不静默截断引用。结果严格遵循调用方权限。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "自然语言查询"},
				"workspace_id": map[string]any{"type": "string", "description": "限定工作区（可选）"},
				"asset_types": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string", "enum": []string{"document", "codebase", "memory", "skill"}},
					"description": "显式类型集（可选，空=Broker 按意图路由）",
				},
				"directory_id":    map[string]any{"type": "string", "description": "限定目录（可选）"},
				"tags":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"max_tokens":      map[string]any{"type": "integer", "description": "上下文 token 预算（0=工作区默认）"},
				"max_items":       map[string]any{"type": "integer", "description": "条目上限（0=工作区默认）"},
				"timeout_ms":      map[string]any{"type": "integer", "default": 2000},
				"include_content": map[string]any{"type": "boolean", "default": false, "description": "是否内联正文（默认 false：目录+摘要+引用）"},
			},
		},
	}
}

// IsWrite is false — knowledge_context is read-only.
func (t *KnowledgeContextTool) IsWrite() bool { return false }

// Execute assembles the context. Missing query → invalid params. The Broker
// returns a budgeted catalog + truncation report; an empty result is a normal
// success (§8.2 no-leak).
func (t *KnowledgeContextTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	req := moraclient.KnowledgeContextRequest{
		Query:          query,
		WorkspaceID:    optString(args, "workspace_id"),
		AssetTypes:     optStringSlice(args, "asset_types"),
		DirectoryID:    optString(args, "directory_id"),
		Tags:           optStringSlice(args, "tags"),
		MaxTokens:      optInt(args, "max_tokens", 0),
		MaxItems:       optInt(args, "max_items", 0),
		TimeoutMs:      optInt(args, "timeout_ms", 0),
		IncludeContent: optBool(args, "include_content", false),
	}
	res, err := t.client.KnowledgeContext(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if isBrokerFault(err) {
			return asTextResult(&moraclient.KnowledgeContextResult{Candidates: []moraclient.KnowledgeCandidate{}})
		}
		return nil, err
	}
	if res == nil {
		res = &moraclient.KnowledgeContextResult{Candidates: []moraclient.KnowledgeCandidate{}}
	}
	return asTextResult(res)
}
