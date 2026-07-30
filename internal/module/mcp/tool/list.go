package tool

import (
	"context"

	"github.com/wiki/wiki-backend/internal/module/mcp/auth"
	"github.com/wiki/wiki-backend/internal/module/mcp/server"
	"github.com/wiki/wiki-backend/internal/module/mcp/wikiclient"
	domainerr "github.com/wiki/wiki-backend/internal/pkg/errors"
)

// ListDocumentsTool implements list_documents (design doc 06 §5.1). Read-only:
// lists documents under a workspace/directory, RBAC-filtered upstream. No
// permission on the workspace returns an empty list (not an error).
type ListDocumentsTool struct{ base }

// NewListDocumentsTool builds a list_documents tool.
func NewListDocumentsTool(client wikiclient.WikiClient) *ListDocumentsTool {
	return &ListDocumentsTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *ListDocumentsTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "list_documents",
		Description: "列出工作区/目录下的文档（受权限过滤）。无权限工作区返回空列表。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"workspace_id"},
			"properties": map[string]any{
				"workspace_id": map[string]any{"type": "string"},
				"directory_id": map[string]any{"type": "string"},
				"tag":          map[string]any{"type": "string"},
				"status":       map[string]any{"type": "string", "enum": []string{"draft", "published", "archived"}},
				"page":         map[string]any{"type": "integer", "default": 1},
				"page_size":    map[string]any{"type": "integer", "default": 20},
			},
		},
	}
}

// IsWrite is false — list_documents is read-only.
func (t *ListDocumentsTool) IsWrite() bool { return false }

// Execute lists documents. Read permission is enforced upstream; no permission
// yields an empty list (existence-leak prevention).
func (t *ListDocumentsTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return nil, err
	}
	params := wikiclient.ListDocumentsParams{
		WorkspaceID: wsID,
		DirectoryID: optString(args, "directory_id"),
		Tag:         optString(args, "tag"),
		Status:      optString(args, "status"),
		Page:        optInt(args, "page", 1),
		PageSize:    optInt(args, "page_size", 20),
	}
	items, total, err := t.client.ListDocuments(ctx, toWikiAuth(auth.FromContext(ctx)), params)
	if err != nil {
		if isNotFound(err) {
			return asTextResult(map[string]any{"items": []any{}, "total": 0})
		}
		return nil, err
	}
	return asTextResult(map[string]any{"items": items, "total": total})
}

// GetTagsTool implements get_tags (design doc 06 §5.1). Read-only: returns the
// tag taxonomy of a workspace. No read permission → empty result.
type GetTagsTool struct{ base }

// NewGetTagsTool builds a get_tags tool.
func NewGetTagsTool(client wikiclient.WikiClient) *GetTagsTool {
	return &GetTagsTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *GetTagsTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "get_tags",
		Description: "获取工作区标签体系。无权限工作区返回空结果。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"workspace_id"},
			"properties": map[string]any{
				"workspace_id": map[string]any{"type": "string"},
			},
		},
	}
}

// IsWrite is false — get_tags is read-only.
func (t *GetTagsTool) IsWrite() bool { return false }

// Execute fetches tags. No read permission → empty result (existence-leak safe).
func (t *GetTagsTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return nil, err
	}
	tags, err := t.client.GetTags(ctx, toWikiAuth(auth.FromContext(ctx)), wsID)
	if err != nil {
		if isNotFound(err) {
			return asTextResult([]any{})
		}
		return nil, err
	}
	return asTextResult(tags)
}

// isNotFound reports whether err is the not-found/not-visible sentinel. Tools
// translate it to an empty result to prevent existence leakage.
func isNotFound(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound)
}
