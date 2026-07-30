package handler

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/wiki/search"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
	"github.com/wiki/wiki-backend/internal/pkg/response"
)

// SearchExecutor runs a built search query against the DB and returns hits.
type SearchExecutor interface {
	Search(ctx context.Context, q search.Query) ([]search.Result, int, error)
}

// VisibilityProvider computes the RBAC-visible document set for a user.
type VisibilityProvider interface {
	VisibleDocuments(ctx context.Context, userID domain.UUID, groupIDs []domain.UUID, workspaceID domain.UUID) (map[domain.UUID]bool, error)
}

// SearchHandler exposes full-text search (API §8). RBAC is enforced as a hard
// filter: only documents in the caller's visible set are searched.
type SearchHandler struct {
	rbac      VisibilityProvider
	exec      SearchExecutor
	ftsConfig string
}

func NewSearchHandler(rbac VisibilityProvider, exec SearchExecutor, ftsConfig string) *SearchHandler {
	return &SearchHandler{rbac: rbac, exec: exec, ftsConfig: ftsConfig}
}

func (h *SearchHandler) Search(c *gin.Context) {
	auth := MustAuth(c)
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}
	q := c.Query("q")
	if q == "" {
		response.Fail(c, badRequestErr("q required"))
		return
	}

	f := search.Filter{
		Query:       q,
		WorkspaceID: wsID,
		Sort:        c.DefaultQuery("sort", "relevance"),
		Params:      pagination.From(c),
		FTSConfig:   h.ftsConfig,
	}
	if dir := c.Query("directory_id"); dir != "" {
		if id, err := uuid.Parse(dir); err == nil {
			f.DirectoryID = &id
		}
	}
	if t := c.Query("tag"); t != "" {
		if id, err := uuid.Parse(t); err == nil {
			f.Tag = &id
		}
	}
	if cb := c.Query("created_by"); cb != "" {
		if id, err := uuid.Parse(cb); err == nil {
			f.CreatedBy = &id
		}
	}
	if ua := c.Query("updated_after"); ua != "" {
		f.UpdatedAfter = &ua
	}
	if ub := c.Query("updated_before"); ub != "" {
		f.UpdatedBefore = &ub
	}

	vis, err := h.rbac.VisibleDocuments(c.Request.Context(), auth.UserID, auth.Groups, wsID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	if _, all := vis[domain.UUID{}]; all || auth.IsAdmin {
		f.VisibleAll = true
	} else {
		for id := range vis {
			f.VisibleDocs = append(f.VisibleDocs, id)
		}
	}

	built := f.Build()
	results, total, err := h.exec.Search(c.Request.Context(), built)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Paged(c, results, total, f.Params.Page, f.Params.PageSize)
}
