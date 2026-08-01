package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/pkg/response"
)

// TagHandler exposes the workspace tag taxonomy (API 04 §4 / §15).
type TagHandler struct {
	repo service.TagRepo
}

func NewTagHandler(repo service.TagRepo) *TagHandler {
	return &TagHandler{repo: repo}
}

// List returns all tags defined in a workspace (GET /workspaces/:workspace_id/tags).
func (h *TagHandler) List(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	tags, err := h.repo.ListByWorkspace(c.Request.Context(), wsID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": tags})
}
