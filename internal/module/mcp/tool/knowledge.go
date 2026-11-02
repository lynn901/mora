package tool

// knowledge.go implements the knowledge_search + knowledge_context MCP tools
// (design-docs/19 §11.4, Phase 6-5). They delegate to the Context Broker via
// the moraclient internal API (POST /internal/v1/knowledge/{search,context}),
// NOT by importing the knowledge/context package — the MCP layer never
// re-implements Broker business logic (D12 / 12 §3.2: "mcp 只依赖内部 API
// Client"). The Broker enforces the §3.2 two-pass authz + runs the ten-step
// pipeline server-side; the MCP tool only forwards the caller identity (the
// delegated agent context, §11.2) + the query.
//
// Existence never leaks (§8.2): the Broker's three-state response maps
// authorized-empty + no-results to a 200 empty candidate list — the tool
// surfaces that as a normal success. A provider fault (the Broker's
// IsProviderDown → 503) surfaces here as a typed error; the tool layer maps it
// to an EMPTY result + a short diagnostic note (§11.5 — never confuse a fault
// with "no knowledge", and never fake a result). RBAC + rate-limit semantics
// are unchanged from the legacy search_knowledge_base / get_document /
// list_documents tools (§11.4): both tools are read-only (IsWrite=false), so
// readonly tokens are admitted and the read rate-limit bucket applies.

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// knowledgeContextSchema is the shared input schema for knowledge_search +
// knowledge_context (the two tools accept the same §11.3 request shape; the
// distinction is the route + the Broker's intent, not different inputs).
func knowledgeContextSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"query", "workspace_id"},
		"properties": map[string]any{
			"query":           map[string]any{"type": "string", "description": "自然语言查询"},
			"workspace_id":    map[string]any{"type": "string", "description": "工作区 id（收敛检索的作用域，AC-4 隔离）"},
			"agent_id":        map[string]any{"type": "string", "description": "委派 Agent id（§11.2 委派上下文，可选）"},
			"asset_types":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "限定资产类型：document/codebase/memory/skill"},
			"filters":         map[string]any{"type": "object", "description": "类型特定过滤（可选）"},
			"max_tokens":      map[string]any{"type": "integer", "description": "上下文预算上限（可选）"},
			"max_items":       map[string]any{"type": "integer", "description": "候选条目上限（可选）"},
			"timeout_ms":      map[string]any{"type": "integer", "description": "超时毫秒（可选）"},
			"include_content": map[string]any{"type": "boolean", "default": false, "description": "是否内联正文（默认仅目录/摘要/引用，§9.6 D6）"},
		},
	}
}

// buildKnowledgeRequest maps the tool args into the moraclient request shape.
// query + workspace_id are required; the rest are optional. optMap (shared with
// the wiki tools) extracts the optional filters object.
func buildKnowledgeRequest(args map[string]any) (moraclient.KnowledgeContextRequest, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return moraclient.KnowledgeContextRequest{}, err
	}
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return moraclient.KnowledgeContextRequest{}, err
	}
	return moraclient.KnowledgeContextRequest{
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

// knowledgeEmptyResult is the leak-safe empty result for the knowledge_* tools
// (§8.2): an authorized-empty / no-results outcome is a normal success with an
// empty candidate list, never an error. The Agent cannot infer whether the
// workspace has knowledge.
func knowledgeEmptyResult() *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: ""}},
	}
}

// knowledgeFaultResult is the §11.5 provider-fault result: empty + a short
// diagnostic note. The note names the fault class, never the workspace or asset
// (§8.2 — no existence leak via the diagnostic).
func knowledgeFaultResult() *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: "knowledge context unavailable"}},
	}
}

// isKnowledgeFault reports whether err is a provider-fault from the Broker
// (§11.5) — the handler surfaces IsProviderDown as 503, which the moraclient
// HTTP layer propagates as a typed *Error(CodeServiceUnavailable). Distinct
// from the authorized-empty / no-results 200 (§7.3 three-state).
func isKnowledgeFault(err error) bool {
	if pe := domainerr.As(err); pe != nil {
		return pe.Status == 503
	}
	return false
}

// --- knowledge_search ---

// KnowledgeSearchTool implements knowledge_search (design-docs/19 §11.4): the
// typed-search surface over the Context Broker — convergence retrieval without
// the full context envelope. Read-only.
type KnowledgeSearchTool struct{ base }

// NewKnowledgeSearchTool builds a knowledge_search tool.
func NewKnowledgeSearchTool(client moraclient.MoraClient) *KnowledgeSearchTool {
	return &KnowledgeSearchTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *KnowledgeSearchTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_search",
		Description: "跨类型收敛检索（文档/代码/记忆/技能），走 Context Broker 十步流水线，结果严格遵循调用方权限。无权或无结果返回空候选列表。",
		InputSchema: knowledgeContextSchema(),
	}
}

// IsWrite is false — knowledge_search is read-only.
func (t *KnowledgeSearchTool) IsWrite() bool { return false }

// Execute runs the typed search. Missing query/workspace_id → invalid params.
// A provider fault (§11.5) → empty result + diagnostic; authorized-empty /
// no-results → empty result (no leak — §8.2).
func (t *KnowledgeSearchTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	req, err := buildKnowledgeRequest(args)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.KnowledgeSearch(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if isKnowledgeFault(err) {
			return knowledgeFaultResult(), nil
		}
		// A not-found / forbidden from the Broker is defensively an empty
		// result (the Broker never emits 403/404, but the moraclient maps any
		// such to ErrNotExist → empty here — §8.2).
		if domainerr.Is(err, domainerr.ErrNotFound) || domainerr.Is(err, domainerr.ErrForbidden) {
			return knowledgeEmptyResult(), nil
		}
		return nil, err
	}
	if resp == nil || len(resp.Candidates) == 0 {
		return knowledgeEmptyResult(), nil
	}
	return asTextResult(resp)
}

// --- knowledge_context ---

// KnowledgeContextTool implements knowledge_context (design-docs/19 §11.4): the
// full Context Broker pipeline — route + budget + citation. Returns the §11.3
// context envelope (candidates + degraded_sources + truncation). Read-only.
type KnowledgeContextTool struct{ base }

// NewKnowledgeContextTool builds a knowledge_context tool.
func NewKnowledgeContextTool(client moraclient.MoraClient) *KnowledgeContextTool {
	return &KnowledgeContextTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *KnowledgeContextTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "knowledge_context",
		Description: "组装上下文（意图路由 + 权威策略 + 预算裁剪 + 引用构建），走 Context Broker 全流程。默认先返回资产目录/摘要/引用，正文由 Agent 再调类型专用工具读取（§9.6）。",
		InputSchema: knowledgeContextSchema(),
	}
}

// IsWrite is false — knowledge_context is read-only.
func (t *KnowledgeContextTool) IsWrite() bool { return false }

// Execute runs the full context pipeline. Same fault/leak semantics as
// KnowledgeSearch; the response carries the full §11.3 envelope.
func (t *KnowledgeContextTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	req, err := buildKnowledgeRequest(args)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.KnowledgeContext(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		if isKnowledgeFault(err) {
			return knowledgeFaultResult(), nil
		}
		if domainerr.Is(err, domainerr.ErrNotFound) || domainerr.Is(err, domainerr.ErrForbidden) {
			return knowledgeEmptyResult(), nil
		}
		return nil, err
	}
	if resp == nil || len(resp.Candidates) == 0 {
		return knowledgeEmptyResult(), nil
	}
	return asTextResult(resp)
}
