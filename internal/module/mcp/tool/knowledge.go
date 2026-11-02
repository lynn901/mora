package tool

// knowledge.go implements the knowledge_search / knowledge_context MCP tools
// (design-docs/19 §11.4, Phase 6-5). Each delegates to the Context Broker's
// internal API via moraclient (D12 — the MCP layer depends ONLY on the
// moraclient internal API client, never on the knowledge/context Broker
// implementation package). The Broker enforces the §3.2 two-pass authz
// server-side; the tool layer propagates the caller's delegated identity
// (AgentID + WorkspaceID, §11.2) and stays leak-safe (§8.2).
//
// Three-state contract (§11.5 / D7): the Broker distinguishes provider-down /
// authorized-empty / no-results. authorized-empty and no-results are
// indistinguishable to the Agent — both return a 200 with an empty candidate
// list (existence never leaks, §8.2). A provider fault surfaces as
// moraclient.ErrProviderDown, which the tool layer maps to an EMPTY result +
// a §15 diagnostic note so the Agent can tell "knowledge unavailable" from
// "no knowledge" — never a faked result, never confused with genuine empty.
//
// Both tools are read-only (IsWrite=false): readonly tokens are admitted; the
// Broker's RBAC is the authority. Required args: query + workspace_id. The
// remaining §11.3 knobs (asset_types / filters / max_tokens / max_items /
// timeout_ms / include_content) are optional and passed through.

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// knowledgeInputSchema is the shared §11.3 input schema for both tools. It
// mirrors the Broker's contextRequest: query + workspace_id are required; the
// rest are optional knobs the Broker honors (intent routing picks asset_types
// when empty — §4.2 rule 6).
func knowledgeInputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"query", "workspace_id"},
		"properties": map[string]any{
			"query":           map[string]any{"type": "string", "description": "自然语言检索查询"},
			"workspace_id":    map[string]any{"type": "string", "description": "目标工作区 id（权限作用域）"},
			"agent_id":        map[string]any{"type": "string", "description": "委托 Agent id（可选，内部请求身份）"},
			"asset_types":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "限定资产类型（document/codebase/memory/skill）；空=由意图路由决定"},
			"filters":         map[string]any{"type": "object", "description": "附加过滤（如 directory_id）"},
			"max_tokens":      map[string]any{"type": "integer", "description": "上下文 token 预算上限（可选）"},
			"max_items":       map[string]any{"type": "integer", "description": "候选条目数上限（可选）"},
			"timeout_ms":      map[string]any{"type": "integer", "description": "整体超时毫秒（可选）"},
			"include_content": map[string]any{"type": "boolean", "default": false, "description": "是否包含正文（默认 false：目录+摘要+引用）"},
		},
	}
}

// toKnowledgeRequest builds the moraclient request from tool args. query +
// workspace_id are required; the rest are opt-in. asset_types passes through as
// the caller-declared string list (the Broker parses + validates AssetType).
func toKnowledgeRequest(args map[string]any) (moraclient.KnowledgeQueryRequest, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return moraclient.KnowledgeQueryRequest{}, err
	}
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return moraclient.KnowledgeQueryRequest{}, err
	}
	return moraclient.KnowledgeQueryRequest{
		Query:          query,
		WorkspaceID:    wsID,
		AgentID:        optString(args, "agent_id"),
		AssetTypes:     optStringSlice(args, "asset_types"),
		Filters:        optMap(args, "filters"),
		MaxTokens:      optInt(args, "max_tokens", 0),
		MaxItems:       optInt(args, "max_items", 0),
		TimeoutMS:      optInt(args, "timeout_ms", 0),
		IncludeContent: optBool(args, "include_content", false),
	}, nil
}

// knowledgeFaultResult is the §15/§11.5 provider-fault result: an empty
// candidate set + a short diagnostic note distinguishing "knowledge provider
// unavailable" from a genuine empty result. The note names only the fault
// class (no asset/workspace internals leak — §8.2).
func knowledgeFaultResult(note string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: note}},
	}
}

// knowledgeFaultNote maps a provider-fault error to a terse diagnostic note.
// Only the fault class is named — never the workspace, asset, or provider
// internals (§8.2 no-leak applies to diagnostics too).
func knowledgeFaultNote() string {
	return "knowledge context provider unavailable"
}

// isKnowledgeFault reports whether err is a leak-safe collapse path: a missing
// / no-permission / forbidden Broker result (§8.2) OR a provider fault (§11.5).
// Both yield an empty result, never an error to the Agent; the provider-fault
// branch additionally carries the diagnostic note.
func isKnowledgeFault(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound) ||
		domainerr.Is(err, domainerr.ErrForbidden) ||
		domainerr.Is(err, moraclient.ErrProviderDown)
}

// --- knowledge_search ---

// KnowledgeSearchTool implements knowledge_search (§11.4): the Broker's typed
// search route (POST /internal/v1/knowledge/search) — the same ten-step
// pipeline as knowledge_context but documented as "direct typed search, no
// context assembly". Read-only; leak-safe (§8.2); three-state (§11.5).
type KnowledgeSearchTool struct{ base }

// NewKnowledgeSearchTool builds a knowledge_search tool over the moraclient
// internal API client.
func NewKnowledgeSearchTool(client moraclient.MoraClient) *KnowledgeSearchTool {
	return &KnowledgeSearchTool{base: base{client: client}}
}

// Definition returns the tool schema. NOT deprecated — this is the new
// converged-search tool the compatibility aliases (search_knowledge_base)
// migrate toward (§11.4).
func (t *KnowledgeSearchTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_search",
		Description: "Context Broker 收敛检索：按意图路由到类型化工具（文档/代码/记忆/Skill），在权限与预算内返回带引用的候选清单。结果严格遵循调用方权限；无权或无结果均返回空清单，不泄露存在性。",
		InputSchema: knowledgeInputSchema(),
	}
}

// IsWrite is false — knowledge_search is read-only.
func (t *KnowledgeSearchTool) IsWrite() bool { return false }

func (t *KnowledgeSearchTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	req, err := toKnowledgeRequest(args)
	if err != nil {
		return nil, err
	}
	res, err := t.client.KnowledgeSearch(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if domainerr.Is(err, moraclient.ErrProviderDown) {
			// Provider fault → empty + diagnostic (§11.5), distinct from genuine empty.
			return knowledgeFaultResult(knowledgeFaultNote()), nil
		}
		if isKnowledgeFault(err) {
			// authorized-empty / no-permission → EMPTY success (no leak — §8.2).
			return emptyTextResult(), nil
		}
		return nil, err
	}
	if res == nil {
		return emptyTextResult(), nil
	}
	return asTextResult(res)
}

// --- knowledge_context ---

// KnowledgeContextTool implements knowledge_context (§11.4): the full Context
// Broker route (POST /internal/v1/knowledge/context) — intent routing →
// two-pass authz → parallel typed query → dedup+conflict-preserving merge →
// budget → citation. Read-only; leak-safe (§8.2); three-state (§11.5).
type KnowledgeContextTool struct{ base }

// NewKnowledgeContextTool builds a knowledge_context tool over the moraclient
// internal API client.
func NewKnowledgeContextTool(client moraclient.MoraClient) *KnowledgeContextTool {
	return &KnowledgeContextTool{base: base{client: client}}
}

// Definition returns the tool schema. NOT deprecated — the new full-context
// tool. The input schema is shared with knowledge_search (§11.3).
func (t *KnowledgeContextTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_context",
		Description: "Context Broker 全流水线：意图路由→两段授权→并行跨类型查询→去重并保留冲突→预算裁剪→引用构建。返回带引用与冲突关系的最小上下文；部分类型降级时附 degraded_sources，预算不足附截断原因与继续读取工具。无权或无结果返回空，不泄露存在性。",
		InputSchema: knowledgeInputSchema(),
	}
}

// IsWrite is false — knowledge_context is read-only.
func (t *KnowledgeContextTool) IsWrite() bool { return false }

func (t *KnowledgeContextTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	req, err := toKnowledgeRequest(args)
	if err != nil {
		return nil, err
	}
	res, err := t.client.KnowledgeContext(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if domainerr.Is(err, moraclient.ErrProviderDown) {
			return knowledgeFaultResult(knowledgeFaultNote()), nil
		}
		if isKnowledgeFault(err) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	if res == nil {
		return emptyTextResult(), nil
	}
	return asTextResult(res)
}
