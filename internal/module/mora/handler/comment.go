package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/pkg/response"
)

type CommentHandler struct {
	repo service.CommentRepo
}

func NewCommentHandler(repo service.CommentRepo) *CommentHandler {
	return &CommentHandler{repo: repo}
}

func (h *CommentHandler) List(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var blockID *domain.UUID
	if b := c.Query("block_id"); b != "" {
		if bid, err := uuid.Parse(b); err == nil {
			blockID = &bid
		}
	}
	items, err := h.repo.List(c.Request.Context(), id, blockID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

type createCommentReq struct {
	Content  string        `json:"content" binding:"required"`
	BlockID  *domain.UUID  `json:"block_id"`
	ParentID *domain.UUID  `json:"parent_id"`
	Mentions []domain.UUID `json:"mentions"`
}

func (h *CommentHandler) Create(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := MustAuth(c)
	var req createCommentReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	cm := &domain.Comment{
		DocumentID: id, BlockID: req.BlockID, ParentID: req.ParentID,
		AuthorID: auth.UserID, Content: req.Content, Mentions: req.Mentions,
	}
	if err := h.repo.Create(c.Request.Context(), cm); err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, cm)
}

func (h *CommentHandler) Resolve(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	auth := MustAuth(c)
	if err := h.repo.Resolve(c.Request.Context(), id, auth.UserID); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"resolved": true})
}
