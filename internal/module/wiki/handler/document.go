package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	wikicontent "github.com/wiki/wiki-backend/internal/module/wiki/content"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
	"github.com/wiki/wiki-backend/internal/pkg/response"
)

// DocumentHandler exposes document CRUD, version diff/rollback per API §5,§6.
type DocumentHandler struct {
	svc *service.DocumentService
}

func NewDocumentHandler(svc *service.DocumentService) *DocumentHandler {
	return &DocumentHandler{svc: svc}
}

type createDocReq struct {
	Title       string           `json:"title" binding:"required"`
	DirectoryID *domain.UUID     `json:"directory_id"`
	Content     []domain.Block   `json:"content"`
	Format      domain.DocumentFormat `json:"format"`
	Tags        []domain.UUID    `json:"tags"`
	Markdown    string           `json:"markdown"`
}

func (h *DocumentHandler) Create(c *gin.Context) {
	wsUUID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req createDocReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := svcAuth(MustAuth(c))
	d := &domain.Document{WorkspaceID: wsUUID, Title: req.Title, DirectoryID: req.DirectoryID, Format: req.Format}
	if req.Markdown != "" {
		d.Content = wikicontent.MarkdownToBlocks(req.Markdown)
		d.Format = domain.FormatMarkdown
	} else {
		d.Content = req.Content
		if d.Format == "" {
			d.Format = domain.FormatBlocks
		}
	}
	out, err := h.svc.Create(c.Request.Context(), auth, d)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, out)
}

type updateDocReq struct {
	Title    string           `json:"title"`
	Content  []domain.Block   `json:"content"`
	Status   domain.DocumentStatus `json:"status"`
	Markdown string           `json:"markdown"`
	Summary  string           `json:"summary"`
}

func (h *DocumentHandler) Update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var req updateDocReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	// If-Match header carries version_no for optimistic concurrency.
	prevVersion := 0
	if v := c.GetHeader("If-Match"); v != "" {
		if n, err := parseInt(v); err == nil {
			prevVersion = n
		}
	}
	auth := svcAuth(MustAuth(c))
	blocks := req.Content
	if req.Markdown != "" {
		blocks = wikicontent.MarkdownToBlocks(req.Markdown)
	}
	out, err := h.svc.Update(c.Request.Context(), auth, id, prevVersion, req.Title, blocks, req.Status)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

func (h *DocumentHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := svcAuth(MustAuth(c))
	out, err := h.svc.Get(c.Request.Context(), auth, id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

func (h *DocumentHandler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := svcAuth(MustAuth(c))
	if err := h.svc.Delete(c.Request.Context(), auth, id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

func (h *DocumentHandler) List(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	q := service.DocumentQuery{WorkspaceID: wsID, Params: pagination.From(c)}
	if dir := c.Query("directory_id"); dir != "" {
		if id, err := uuid.Parse(dir); err == nil {
			q.DirectoryID = &id
		}
	}
	if s := c.Query("status"); s != "" {
		q.Status = domain.DocumentStatus(s)
	}
	if cb := c.Query("created_by"); cb != "" {
		if id, err := uuid.Parse(cb); err == nil {
			q.CreatedBy = &id
		}
	}
	auth := svcAuth(MustAuth(c))
	items, total, err := h.svc.ListVisible(c.Request.Context(), auth, q)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Paged(c, items, total, q.Params.Page, q.Params.PageSize)
}

// --- version ---

func (h *DocumentHandler) ListVersions(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := svcAuth(MustAuth(c))
	// reuse Get for read permission check
	if _, err := h.svc.Get(c.Request.Context(), auth, id); err != nil {
		response.Fail(c, err)
		return
	}
	// version listing delegated; we expose diff via service directly
	response.OK(c, gin.H{"note": "use /documents/{id}/versions via VersionHandler"})
	_ = id
}

func (h *DocumentHandler) DiffVersions(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	from, err1 := parseInt(c.Query("from"))
	to, err2 := parseInt(c.Query("to"))
	if err1 != nil || err2 != nil {
		response.Fail(c, badRequestErr("from and to required"))
		return
	}
	auth := svcAuth(MustAuth(c))
	diff, err := h.svc.DiffVersions(c.Request.Context(), auth, id, from, to)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"from_version": from, "to_version": to, "diff": diff})
}

func (h *DocumentHandler) Rollback(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	vno, err := parseInt(c.Param("version_no"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid version_no"))
		return
	}
	auth := svcAuth(MustAuth(c))
	out, err := h.svc.Rollback(c.Request.Context(), auth, id, vno)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, out)
}

// --- helpers ---

func svcAuth(s AuthState) service.AuthContext {
	return service.AuthContext{UserID: s.UserID, Groups: s.Groups, IsAdmin: s.IsAdmin}
}

func badRequest(msg string) error { return badRequestErr(msg) }
