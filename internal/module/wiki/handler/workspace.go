package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/response"
)

type WorkspaceHandler struct {
	repo service.WorkspaceRepo
}

func NewWorkspaceHandler(repo service.WorkspaceRepo) *WorkspaceHandler {
	return &WorkspaceHandler{repo: repo}
}

func (h *WorkspaceHandler) List(c *gin.Context) {
	auth := MustAuth(c)
	items, err := h.repo.List(c.Request.Context(), auth.UserID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

type createWSReq struct {
	Name        string `json:"name" binding:"required"`
	Slug        string `json:"slug" binding:"required"`
	Description string `json:"description"`
}

func (h *WorkspaceHandler) Create(c *gin.Context) {
	auth := MustAuth(c)
	var req createWSReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	ws := &domain.Workspace{Name: req.Name, Slug: req.Slug, Description: req.Description, OwnerID: auth.UserID}
	if err := h.repo.Create(c.Request.Context(), ws); err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, ws)
}

func (h *WorkspaceHandler) Get(c *gin.Context) {
	// Param name matches the :workspace_id wildcard shared by all
	// /workspaces/:workspace_id/* routes; gin forbids mixing :id and
	// :workspace_id at the same path segment (route-tree conflict panic).
	id, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	ws, err := h.repo.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, ws)
}

// --- Directory ---

type DirectoryHandler struct {
	repo service.DirectoryRepo
}

func NewDirectoryHandler(repo service.DirectoryRepo) *DirectoryHandler {
	return &DirectoryHandler{repo: repo}
}

func (h *DirectoryHandler) ListTree(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	dirs, err := h.repo.ListByWorkspace(c.Request.Context(), wsID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	tree := service.BuildTree(dirs, nil)
	response.OK(c, gin.H{"items": tree})
}

type createDirReq struct {
	Name     string       `json:"name" binding:"required"`
	ParentID *domain.UUID `json:"parent_id"`
	Order    int          `json:"sort_order"`
}

func (h *DirectoryHandler) Create(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req createDirReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	// build ltree path from parent
	parentPath := ""
	if req.ParentID != nil {
		parent, err := h.repo.Get(c.Request.Context(), *req.ParentID)
		if err != nil {
			response.Fail(c, err)
			return
		}
		parentPath = parent.Path
	}
	d := &domain.Directory{
		WorkspaceID: wsID, ParentID: req.ParentID, Name: req.Name,
		Path: service.ChildPath(parentPath, req.Name), SortOrder: req.Order,
	}
	if err := h.repo.Create(c.Request.Context(), d); err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, d)
}

func (h *DirectoryHandler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	if err := h.repo.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}
