package tool

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
)

// SearchTool implements search_knowledge_base (design doc 06 §5.2.1): semantic
// hybrid retrieval (Dense+BM25+Rerank) via the RAG service, RBAC-filtered
// upstream. Read-only: no-permission results are simply absent from the result
// set (the RAG service hard-filters invisible docs).
type SearchTool struct{ base }

// NewSearchTool builds a search_knowledge_base tool.
func NewSearchTool(client moraclient.MoraClient) *SearchTool {
	return &SearchTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *SearchTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "search_knowledge_base",
		Description: "在 Mora 知识库中进行语义混合检索（稠密向量+BM25+重排），结果严格遵循调用方权限。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"query"},
			"properties": map[string]any{
				"query":        map[string]any{"type": "string", "description": "自然语言查询"},
				"workspace_id": map[string]any{"type": "string", "description": "限定工作区（可选）"},
				"directory_id": map[string]any{"type": "string", "description": "限定目录（可选）"},
				"tags":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"top_k":        map[string]any{"type": "integer", "default": 50},
				"top_n":        map[string]any{"type": "integer", "default": 10},
				"rerank":       map[string]any{"type": "boolean", "default": false, "description": "启用重排（P1）"},
			},
		},
	}
}

// IsWrite is false — search is read-only.
func (t *SearchTool) IsWrite() bool { return false }

// Execute runs the search. Missing query → invalid params. Empty results are a
// normal success (no permission is not distinguishable from no matches).
func (t *SearchTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	query, err := requireString(args, "query")
	if err != nil {
		return nil, err
	}
	req := moraclient.SearchRequest{
		Query:       query,
		WorkspaceID: optString(args, "workspace_id"),
		DirectoryID: optString(args, "directory_id"),
		Tags:        optStringSlice(args, "tags"),
		TopK:        optInt(args, "top_k", 50),
		TopN:        optInt(args, "top_n", 10),
		Rerank:      optBool(args, "rerank", false),
	}
	result, err := t.client.Search(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &moraclient.SearchResult{}
	}
	return asTextResult(result)
}
