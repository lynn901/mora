package handler

// parse.go wires the multi-format parsing HTTP surface (design-docs/10 §7.2):
//   POST   /workspaces/:ws/documents/upload          — upload + enqueue parse
//   POST   /workspaces/:ws/documents/reparse          — batch re-parse
//   GET    /documents/:id/parse-progress              — staged timeline
//   POST   /rag/chunk-preview                          — preview chunker (no persist)
//   GET    /workspaces/:ws/parse-configs              — list config templates
//   POST   /workspaces/:ws/parse-configs              — create template
//   PUT    /workspaces/:ws/parse-configs/:cid         — update template
//   DELETE /workspaces/:ws/parse-configs/:cid         — delete template
//
// RBAC is enforced in the service layer (workspace write for upload/reparse,
// document read for parse-progress); existence never leaks (forbidden and
// missing both surface as 404).

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/pkg/response"
)

// ParseHandler exposes the parse upload/reparse/preview/progress/config routes.
type ParseHandler struct {
	svc *service.ParseService
}

func NewParseHandler(svc *service.ParseService) *ParseHandler { return &ParseHandler{svc: svc} }

// Upload handles multipart document upload + parse enqueue (10 §7.2).
// Form fields: file (binary), directory_id?, title?, parse_config_id?,
// parse_options (inline JSON string, overrides template).
func (h *ParseHandler) Upload(c *gin.Context) {
	wsUUID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		response.Fail(c, badRequestErr("missing file"))
		return
	}
	src, err := file.Open()
	if err != nil {
		response.Fail(c, badRequestErr("cannot open file"))
		return
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		response.Fail(c, badRequestErr("cannot read file"))
		return
	}
	req := service.UploadRequest{
		WorkspaceID: wsUUID,
		Filename:    file.Filename,
		MIME:        detectMIME(file.Filename, file.Header.Get("Content-Type")),
		FileData:    data,
		Title:       c.PostForm("title"),
	}
	if dir := c.PostForm("directory_id"); dir != "" {
		if id, err := uuid.Parse(dir); err == nil {
			req.DirectoryID = &id
		}
	}
	if cid := c.PostForm("parse_config_id"); cid != "" {
		if id, err := uuid.Parse(cid); err == nil {
			req.ParseConfigID = &id
		}
	}
	if po := c.PostForm("parse_options"); po != "" {
		var opts map[string]any
		if err := json.Unmarshal([]byte(po), &opts); err == nil {
			req.ParseOptions = opts
		}
	}
	auth := svcAuth(MustAuth(c))
	out, err := h.svc.UploadFile(c.Request.Context(), auth, req)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, out)
}

// Reparse handles batch re-parse (10 §5.2, §7.2).
type reparseReq struct {
	DocumentIDs  []string       `json:"document_ids"`
	DirectoryID  string         `json:"directory_id,omitempty"`
	ParseOptions map[string]any `json:"parse_options,omitempty"`
}

func (h *ParseHandler) Reparse(c *gin.Context) {
	wsUUID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req reparseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	// cap at 500 documents per call (10 §7.3)
	if len(req.DocumentIDs) > 500 {
		req.DocumentIDs = req.DocumentIDs[:500]
	}
	docIDs := make([]domain.UUID, 0, len(req.DocumentIDs))
	for _, s := range req.DocumentIDs {
		if id, err := uuid.Parse(s); err == nil {
			docIDs = append(docIDs, id)
		}
	}
	auth := svcAuth(MustAuth(c))
	out, err := h.svc.Reparse(c.Request.Context(), auth, service.ReparseRequest{
		WorkspaceID:  wsUUID,
		DocumentIDs:  docIDs,
		ParseOptions: req.ParseOptions,
	})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Accepted(c, out)
}

// ParseProgress returns the staged timeline + badges (10 §6.3).
func (h *ParseHandler) ParseProgress(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := svcAuth(MustAuth(c))
	out, err := h.svc.ParseProgress(c.Request.Context(), auth, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// ChunkPreview runs the chunker on input text without persisting (10 §2.2, §7.2).
type chunkPreviewReq struct {
	Text         string         `json:"text"`
	ParseOptions map[string]any `json:"parse_options,omitempty"`
}

func (h *ParseHandler) ChunkPreview(c *gin.Context) {
	var req chunkPreviewReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	opts := parser.ParseOptions{}
	if b, err := json.Marshal(req.ParseOptions); err == nil {
		_ = json.Unmarshal(b, &opts)
	}
	out, err := h.svc.ChunkPreview(c.Request.Context(), req.Text, opts)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// --- parse config templates (10 §7.1) ---

func (h *ParseHandler) ListConfigs(c *gin.Context) {
	wsUUID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	cfgs, err := h.svc.ListConfigs(c.Request.Context(), wsUUID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"configs": cfgs})
}

type createConfigReq struct {
	Name      string         `json:"name" binding:"required"`
	Config    map[string]any `json:"config"`
	IsDefault bool           `json:"is_default"`
}

func (h *ParseHandler) CreateConfig(c *gin.Context) {
	wsUUID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req createConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	out, err := h.svc.CreateConfig(c.Request.Context(), wsUUID, service.ParseConfig{Name: req.Name, Config: req.Config, IsDefault: req.IsDefault})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, out)
}

func (h *ParseHandler) UpdateConfig(c *gin.Context) {
	cid := c.Param("cid")
	var req createConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	out, err := h.svc.UpdateConfig(c.Request.Context(), cid, service.ParseConfig{Name: req.Name, Config: req.Config, IsDefault: req.IsDefault})
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

func (h *ParseHandler) DeleteConfig(c *gin.Context) {
	cid := c.Param("cid")
	if err := h.svc.DeleteConfig(c.Request.Context(), cid); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

// detectMIME picks a MIME from the filename extension when the multipart
// header's Content-Type is the generic "application/octet-stream".
func detectMIME(filename, headerCT string) string {
	if headerCT != "" && !strings.HasPrefix(headerCT, "application/octet-stream") {
		return headerCT
	}
	fn := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(fn, ".md"), strings.HasSuffix(fn, ".markdown"):
		return "text/markdown"
	case strings.HasSuffix(fn, ".html"), strings.HasSuffix(fn, ".htm"):
		return "text/html"
	case strings.HasSuffix(fn, ".json"):
		return "application/json"
	case strings.HasSuffix(fn, ".csv"):
		return "text/csv"
	case strings.HasSuffix(fn, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(fn, ".docx"):
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case strings.HasSuffix(fn, ".xlsx"):
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case strings.HasSuffix(fn, ".pptx"):
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case strings.HasSuffix(fn, ".epub"):
		return "application/epub+zip"
	case strings.HasSuffix(fn, ".mhtml"), strings.HasSuffix(fn, ".mht"):
		return "message/rfc822"
	case strings.HasSuffix(fn, ".txt"):
		return "text/plain"
	}
	return headerCT
}
