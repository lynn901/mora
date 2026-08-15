package tool

// code.go implements the eight code_* MCP tools (design-docs/17 §6.2). Each
// delegates to the upstream codegraph query service via MoraClient, enforcing
// token-scope gating locally (IsWrite=false → readonly tokens allowed) while
// RBAC is applied server-side by the codegraph service (via
// asset.ReadService.GetAsset).
//
// Existence never leaks (§8.2 / §6.4): an upstream ErrNotFound / ErrForbidden
// (a missing / cross-workspace / no-permission codebase) → an EMPTY success
// result, never an error to the Agent. A provider fault
// (capability_unavailable / source_snapshot_unavailable / asset_version
// mismatch) also yields an empty result but with a diagnostic text note so the
// Agent can tell "no results" from "graph not available" — §15 fault table:
// never confuse provider fault, authorized-empty, and genuine no-results.
//
// Every tool requires a codebase_id (the knowledge_assets asset id). Query
// tools require their respective query/symbol argument.

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// codegraphEmptyResult is the leak-safe empty result for code_* tools when the
// upstream returns not-found / forbidden (a missing/no-permission codebase).
// Same shape as emptyTextResult but kept distinct so a provider-fault path can
// append a diagnostic note (§15) without changing the no-leak contract here.
func codegraphEmptyResult() *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: ""}},
	}
}

// codegraphFaultResult is the §15 provider-fault result: empty hits + a short
// diagnostic note. Used when the upstream surfaces capability_unavailable /
// source_snapshot_unavailable / asset_version mismatch — the Agent gets an
// empty result (no faked code) but a note distinguishing it from genuine empty.
func codegraphFaultResult(note string) *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: note}},
	}
}

// isCodegraphFault reports whether err is a provider-fault sentinel the
// codegraph service surfaces distinctly from authorized-empty (§15).
func isCodegraphFault(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound) ||
		domainerr.Is(err, domainerr.ErrForbidden)
}

// codeNoteFor maps a provider-fault error to a §15 diagnostic note. Kept terse
// so it does not leak asset internals (§8.2 — the note names the fault class,
// never the codebase or commit).
func codeNoteFor(err error) string {
	switch {
	case domainerr.Is(err, domainerr.ErrForbidden):
		return "codegraph capability unavailable"
	default:
		return "codegraph query unavailable"
	}
}

// --- code_status ---

// CodeStatusTool implements code_status (§6.2). Read-only: returns the active
// graph's version metadata. No-permission/absent codebase → empty result.
type CodeStatusTool struct{ base }

// NewCodeStatusTool builds a code_status tool.
func NewCodeStatusTool(client moraclient.MoraClient) *CodeStatusTool {
	return &CodeStatusTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *CodeStatusTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_status",
		Description: "返回代码库当前激活的 codegraph 版本元数据（commit/source_tree_hash/provider）。无权限或不存在时返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string", "description": "代码库资产 id"},
			},
		},
	}
}

// IsWrite is false — code_status is read-only.
func (t *CodeStatusTool) IsWrite() bool { return false }

func (t *CodeStatusTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	st, err := t.client.CodeStatus(ctx, toMoraAuth(auth.FromContext(ctx)), cbID)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return nil, err
	}
	if st == nil {
		return codegraphEmptyResult(), nil
	}
	return asTextResult(st)
}

// --- code_files ---

// CodeFilesTool implements code_files (§6.2). Read-only: source tree listing.
type CodeFilesTool struct{ base }

func NewCodeFilesTool(client moraclient.MoraClient) *CodeFilesTool {
	return &CodeFilesTool{base: base{client: client}}
}

func (t *CodeFilesTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_files",
		Description: "列出代码库的源文件树。无权限或不存在时返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"path_prefix": map[string]any{"type": "string", "description": "路径前缀过滤（可选）"},
			},
		},
	}
}

func (t *CodeFilesTool) IsWrite() bool { return false }

func (t *CodeFilesTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	tree, err := t.client.CodeFiles(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, optString(args, "path_prefix"))
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return nil, err
	}
	if tree == nil {
		return codegraphEmptyResult(), nil
	}
	return asTextResult(tree)
}

// --- code_search ---

// CodeSearchTool implements code_search (§6.2). Read-only: code search.
type CodeSearchTool struct{ base }

func NewCodeSearchTool(client moraclient.MoraClient) *CodeSearchTool {
	return &CodeSearchTool{base: base{client: client}}
}

func (t *CodeSearchTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_search",
		Description: "在代码库中检索符号/文本（受权限过滤）。无权限返回空结果，正常无匹配亦为空。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "query"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"query":       map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"path_glob":   map[string]any{"type": "string"},
				"limit":       map[string]any{"type": "integer", "default": 50},
			},
		},
	}
}

func (t *CodeSearchTool) IsWrite() bool { return false }

func (t *CodeSearchTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	req := moraclient.CodeSearchQuery{
		Query:    query,
		Language: optString(args, "language"),
		PathGlob: optString(args, "path_glob"),
		Limit:    optInt(args, "limit", 50),
	}
	hits, err := t.client.CodeSearch(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, req)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return codegraphFaultResult(codeNoteFor(err)), nil
	}
	if hits == nil {
		hits = &moraclient.CodeHits{}
	}
	return asTextResult(hits)
}

// --- code_explore ---

// CodeExploreTool implements code_explore (§6.2). Read-only: combined query
// returning hits + the symbols they resolve to.
type CodeExploreTool struct{ base }

func NewCodeExploreTool(client moraclient.MoraClient) *CodeExploreTool {
	return &CodeExploreTool{base: base{client: client}}
}

func (t *CodeExploreTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_explore",
		Description: "代码库探索：返回匹配的符号定义与命中。无权限返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "query"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"query":       map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"limit":       map[string]any{"type": "integer", "default": 50},
			},
		},
	}
}

func (t *CodeExploreTool) IsWrite() bool { return false }

func (t *CodeExploreTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	req := moraclient.CodeExploreQuery{
		Query:    query,
		Language: optString(args, "language"),
		Limit:    optInt(args, "limit", 50),
	}
	res, err := t.client.CodeExplore(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, req)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return codegraphFaultResult(codeNoteFor(err)), nil
	}
	if res == nil {
		res = &moraclient.CodeExploreResult{}
	}
	return asTextResult(res)
}

// --- code_node ---

// CodeNodeTool implements code_node (§6.2). Read-only: resolves one symbol.
type CodeNodeTool struct{ base }

func NewCodeNodeTool(client moraclient.MoraClient) *CodeNodeTool {
	return &CodeNodeTool{base: base{client: client}}
}

func (t *CodeNodeTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_node",
		Description: "解析单个符号的定义（签名/文档/位置）。无权限返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "symbol"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"symbol":      map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string", "description": "重名消歧（可选）"},
			},
		},
	}
}

func (t *CodeNodeTool) IsWrite() bool { return false }

func (t *CodeNodeTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	symbol, err := requireString(args, "symbol")
	if err != nil {
		return nil, err
	}
	req := moraclient.CodeSymbolQuery{
		Symbol:   symbol,
		Language: optString(args, "language"),
		Path:     optString(args, "path"),
	}
	node, err := t.client.CodeNode(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, req)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return codegraphFaultResult(codeNoteFor(err)), nil
	}
	if node == nil {
		return codegraphEmptyResult(), nil
	}
	return asTextResult(node)
}

// --- code_callers ---

// CodeCallersTool implements code_callers (§6.2). Read-only: incoming call edges.
type CodeCallersTool struct{ base }

func NewCodeCallersTool(client moraclient.MoraClient) *CodeCallersTool {
	return &CodeCallersTool{base: base{client: client}}
}

func (t *CodeCallersTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_callers",
		Description: "返回调用某符号的调用方边集合。无权限返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "symbol"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"symbol":      map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
			},
		},
	}
}

func (t *CodeCallersTool) IsWrite() bool { return false }

func (t *CodeCallersTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	return runCodeEdgesTool(ctx, args, t.client.CodeCallers)
}

// --- code_callees ---

// CodeCalleesTool implements code_callees (§6.2). Read-only: outgoing call edges.
type CodeCalleesTool struct{ base }

func NewCodeCalleesTool(client moraclient.MoraClient) *CodeCalleesTool {
	return &CodeCalleesTool{base: base{client: client}}
}

func (t *CodeCalleesTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_callees",
		Description: "返回某符号的下游被调用方边集合。无权限返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "symbol"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"symbol":      map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
			},
		},
	}
}

func (t *CodeCalleesTool) IsWrite() bool { return false }

func (t *CodeCalleesTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	return runCodeEdgesTool(ctx, args, t.client.CodeCallees)
}

// runCodeEdgesTool is the shared body for code_callers / code_callees: bind
// codebase_id + symbol (optional language/path disambiguator), call the
// upstream edges method, map not-found/forbidden to empty (no leak) and a
// provider fault to empty + diagnostic (§15).
func runCodeEdgesTool(ctx context.Context, args map[string]any,
	call func(context.Context, *moraclient.AuthContext, string, moraclient.CodeSymbolQuery) (*moraclient.CodeEdges, error),
) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	symbol, err := requireString(args, "symbol")
	if err != nil {
		return nil, err
	}
	req := moraclient.CodeSymbolQuery{
		Symbol:   symbol,
		Language: optString(args, "language"),
		Path:     optString(args, "path"),
	}
	edges, err := call(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, req)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return codegraphFaultResult(codeNoteFor(err)), nil
	}
	if edges == nil {
		edges = &moraclient.CodeEdges{}
	}
	return asTextResult(edges)
}

// --- code_impact ---

// CodeImpactTool implements code_impact (§6.2). Read-only: change-impact set.
type CodeImpactTool struct{ base }

func NewCodeImpactTool(client moraclient.MoraClient) *CodeImpactTool {
	return &CodeImpactTool{base: base{client: client}}
}

func (t *CodeImpactTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "code_impact",
		Description: "计算修改某符号的影响范围（受权限过滤）。无权限返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"codebase_id", "symbol"},
			"properties": map[string]any{
				"codebase_id": map[string]any{"type": "string"},
				"symbol":      map[string]any{"type": "string"},
				"language":    map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
				"depth":       map[string]any{"type": "integer", "default": 2, "description": "传播深度（可选，默认 2）"},
			},
		},
	}
}

func (t *CodeImpactTool) IsWrite() bool { return false }

func (t *CodeImpactTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	cbID, err := requireString(args, "codebase_id")
	if err != nil {
		return nil, err
	}
	symbol, err := requireString(args, "symbol")
	if err != nil {
		return nil, err
	}
	req := moraclient.CodeImpactQuery{
		Symbol:   symbol,
		Language: optString(args, "language"),
		Path:     optString(args, "path"),
		Depth:    optInt(args, "depth", 2),
	}
	hits, err := t.client.CodeImpact(ctx, toMoraAuth(auth.FromContext(ctx)), cbID, req)
	if err != nil {
		if isCodegraphFault(err) {
			return codegraphEmptyResult(), nil
		}
		return codegraphFaultResult(codeNoteFor(err)), nil
	}
	if hits == nil {
		hits = &moraclient.CodeHits{}
	}
	return asTextResult(hits)
}
