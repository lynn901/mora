package tool

import (
	"context"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// GetDocumentTool implements get_document (design doc 06 §5.2.2). Read-only.
// No read permission → empty success result (existence-leak prevention, §6.4).
type GetDocumentTool struct{ base }

// NewGetDocumentTool builds a get_document tool.
func NewGetDocumentTool(client moraclient.MoraClient) *GetDocumentTool {
	return &GetDocumentTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *GetDocumentTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "get_document",
		Description: "读取文档正文（Block JSON 或 Markdown）。无权限时返回空结果而非错误，防止存在性泄露。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"document_id"},
			"properties": map[string]any{
				"document_id": map[string]any{"type": "string"},
				"format":      map[string]any{"type": "string", "enum": []string{"blocks", "markdown"}, "default": "markdown"},
				"version_no":  map[string]any{"type": "integer", "description": "指定版本（可选，默认最新）"},
			},
		},
	}
}

// IsWrite is false — get_document is read-only.
func (t *GetDocumentTool) IsWrite() bool { return false }

// Execute fetches the document. ErrNotFound/ErrForbidden from upstream are
// converted to an empty success result so the Agent cannot infer existence.
func (t *GetDocumentTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	docID, err := requireString(args, "document_id")
	if err != nil {
		return nil, err
	}
	doc, err := t.client.GetDocument(ctx, toMoraAuth(auth.FromContext(ctx)), docID,
		optString(args, "format"), optInt(args, "version_no", 0))
	if err != nil {
		if domainerr.Is(err, domainerr.ErrNotFound) || domainerr.Is(err, domainerr.ErrForbidden) {
			return emptyTextResult(), nil
		}
		return nil, err
	}
	return asTextResult(doc)
}

// CreateDraftTool implements create_draft (design doc 06 §5.2.3, P1 S3). Write
// tool: writes never publish directly — they create a draft/review state. Token
// scope=readonly → ErrScopeDenied (rejected before upstream call).
type CreateDraftTool struct{ base }

// NewCreateDraftTool builds a create_draft tool.
func NewCreateDraftTool(client moraclient.MoraClient) *CreateDraftTool {
	return &CreateDraftTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *CreateDraftTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "create_draft",
		Description: "创建文档草稿，不直接发布。需人工/流程审阅后发布并触发向量化。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"workspace_id", "title", "content"},
			"properties": map[string]any{
				"workspace_id": map[string]any{"type": "string"},
				"parent_id":    map[string]any{"type": "string", "description": "父目录（可选）"},
				"title":        map[string]any{"type": "string"},
				"content":      map[string]any{"type": "string", "description": "Markdown 或 Block JSON"},
				"format":       map[string]any{"type": "string", "enum": []string{"blocks", "markdown"}, "default": "markdown"},
			},
		},
	}
}

// IsWrite is true — create_draft is a write tool.
func (t *CreateDraftTool) IsWrite() bool { return true }

// Execute creates the draft after scope gating. Missing write permission is
// surfaced as a forbidden error (403) — write ops have no existence-leak
// concern (design doc 06 §6.4).
func (t *CreateDraftTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	if err := auth.CheckWriteScope(auth.FromContext(ctx)); err != nil {
		return nil, err
	}
	wsID, err := requireString(args, "workspace_id")
	if err != nil {
		return nil, err
	}
	title, err := requireString(args, "title")
	if err != nil {
		return nil, err
	}
	content, err := requireString(args, "content")
	if err != nil {
		return nil, err
	}
	req := moraclient.CreateDraftRequest{
		WorkspaceID: wsID,
		ParentID:    optString(args, "parent_id"),
		Title:       title,
		Content:     content,
		Format:      optString(args, "format"),
	}
	if req.Format == "" {
		req.Format = "markdown"
	}
	res, err := t.client.CreateDraft(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		return nil, err
	}
	return asTextResult(res)
}

// UpdateDocumentTool implements update_document (design doc 06 §5.2.4, P1 S3).
// Write tool: produces a new draft version pending review.
type UpdateDocumentTool struct{ base }

// NewUpdateDocumentTool builds an update_document tool.
func NewUpdateDocumentTool(client moraclient.MoraClient) *UpdateDocumentTool {
	return &UpdateDocumentTool{base: base{client: client}}
}

// Definition returns the tool schema.
func (t *UpdateDocumentTool) Definition() server.ToolDef {
	return server.ToolDef{
		Name:        "update_document",
		Description: "更新文档内容，产生新版本草稿，待审阅发布。",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"document_id", "content"},
			"properties": map[string]any{
				"document_id": map[string]any{"type": "string"},
				"content":     map[string]any{"type": "string"},
				"format":      map[string]any{"type": "string", "enum": []string{"blocks", "markdown"}, "default": "markdown"},
				"summary":     map[string]any{"type": "string", "description": "变更摘要"},
			},
		},
	}
}

// IsWrite is true — update_document is a write tool.
func (t *UpdateDocumentTool) IsWrite() bool { return true }

// Execute updates the document into a draft version after scope gating.
func (t *UpdateDocumentTool) Execute(ctx context.Context, args map[string]any) (*server.ToolCallResult, error) {
	if err := auth.CheckWriteScope(auth.FromContext(ctx)); err != nil {
		return nil, err
	}
	docID, err := requireString(args, "document_id")
	if err != nil {
		return nil, err
	}
	content, err := requireString(args, "content")
	if err != nil {
		return nil, err
	}
	req := moraclient.UpdateDocumentRequest{
		DocumentID: docID,
		Content:    content,
		Format:     optString(args, "format"),
		Summary:    optString(args, "summary"),
	}
	if req.Format == "" {
		req.Format = "markdown"
	}
	res, err := t.client.UpdateDocument(ctx, toMoraAuth(auth.FromContext(ctx)), req)
	if err != nil {
		return nil, err
	}
	return asTextResult(res)
}
