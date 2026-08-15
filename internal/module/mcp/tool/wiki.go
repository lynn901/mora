package tool

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// WikiStatusTool implements wiki_status (design doc 16 §7.3). It returns a Wiki
// Space's directory, most recent maintenance run, and visible pending
// proposals — all RBAC-filtered upstream. Read-only: no-permission/absent
// Space yields an empty success result, never an error (§8.2 — existence does
// not leak).
type WikiStatusTool struct{ base }

// NewWikiStatusTool builds a wiki_status tool.
func NewWikiStatusTool(client moraclient.MoraClient) *WikiStatusTool {
	return &WikiStatusTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *WikiStatusTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "wiki_status",
		Description: "查询 Wiki Space 的目录、最近维护状态与可见候选（proposals）。只读，受 RBAC 约束；无权限返回空结果而非错误。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"wiki_space_id"},
			"properties": map[string]any{
				"wiki_space_id": map[string]any{"type": "string", "description": "Wiki Space ID"},
			},
		},
	}
}

// IsWrite is false — wiki_status is read-only.
func (t *WikiStatusTool) IsWrite() bool { return false }

// Execute fetches the Space status. ErrNotFound/ErrForbidden upstream convert
// to an empty result so the Agent cannot infer the Space's existence.
func (t *WikiStatusTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	spaceID, err := requireString(args, "wiki_space_id")
	if err != nil {
		return nil, err
	}
	st, err := t.client.WikiStatus(ctx, toMoraAuth(auth.FromContext(ctx)), spaceID)
	if err != nil {
		if domainerr.Is(err, domainerr.ErrNotFound) || domainerr.Is(err, domainerr.ErrForbidden) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	return asTextResult(st)
}

// WikiPageProposeTool implements wiki_page_propose (design doc 16 §7.3 / §11.3).
// When a user/Agent explicitly asks to persist an answer, it triggers a
// maintenance run that lands a candidate proposal — it never publishes
// directly. Write tool: a read-only token scope is rejected with
// ErrScopeDenied before the upstream call; a caller without write permission
// on the Space gets ErrForbidden.
type WikiPageProposeTool struct{ base }

// NewWikiPageProposeTool builds a wiki_page_propose tool.
func NewWikiPageProposeTool(client moraclient.MoraClient) *WikiPageProposeTool {
	return &WikiPageProposeTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *WikiPageProposeTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "wiki_page_propose",
		Description: "在 Wiki Space 中为指定页面沉淀一个候选（proposal），触发维护运行但不直接发布。需要写权限；普通查询不应触发此工具。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"wiki_space_id", "page_key"},
			"properties": map[string]any{
				"wiki_space_id": map[string]any{"type": "string", "description": "Wiki Space ID"},
				"page_key":      map[string]any{"type": "string", "description": "目标页面 key"},
				"answer_ref":    map[string]any{"type": "object", "description": "沉淀回答的引用载荷（可选）"},
			},
		},
	}
}

// IsWrite is true — wiki_page_propose triggers a maintenance run (write).
func (t *WikiPageProposeTool) IsWrite() bool { return true }

// Execute triggers the maintenance run. The server's scope gate (§5.1/§7.2)
// rejects a read-only token before this runs; a write caller without
// resource permission gets ErrForbidden surfaced as 403.
func (t *WikiPageProposeTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	if err := auth.CheckWriteScope(auth.FromContext(ctx)); err != nil {
		return nil, err
	}
	spaceID, err := requireString(args, "wiki_space_id")
	if err != nil {
		return nil, err
	}
	pageKey, err := requireString(args, "page_key")
	if err != nil {
		return nil, err
	}
	res, err := t.client.WikiPagePropose(ctx, toMoraAuth(auth.FromContext(ctx)), moraclient.WikiPageProposeRequest{
		WikiSpaceID: spaceID,
		PageKey:     pageKey,
		AnswerRef:   optMap(args, "answer_ref"),
	})
	if err != nil {
		return nil, err
	}
	return asTextResult(res)
}

// optMap extracts an optional object argument as a map[string]any (for the
// answer_ref payload). Non-map values are ignored.
func optMap(args map[string]any, key string) map[string]any {
	v, ok := args[key]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
