package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/response"
	"github.com/wiki/wiki-backend/internal/platform/rbac"
)

type RBACHandler struct {
	svc    *service.PermissionService
	engine *rbac.Engine
}

func NewRBACHandler(svc *service.PermissionService, engine *rbac.Engine) *RBACHandler {
	return &RBACHandler{svc: svc, engine: engine}
}

func (h *RBACHandler) List(c *gin.Context) {
	var tt domain.TargetType
	if v := c.Query("target_type"); v != "" {
		tt = domain.TargetType(v)
	}
	var targetID, subjectID *domain.UUID
	if v := c.Query("target_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			targetID = &id
		}
	}
	if v := c.Query("subject_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			subjectID = &id
		}
	}
	items, err := h.svc.List(c.Request.Context(), tt, targetID, subjectID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

type grantReq struct {
	SubjectType  domain.SubjectType  `json:"subject_type" binding:"required"`
	SubjectID    domain.UUID         `json:"subject_id" binding:"required"`
	RoleID       domain.UUID         `json:"role_id" binding:"required"`
	TargetType   domain.TargetType   `json:"target_type" binding:"required"`
	TargetID     domain.UUID         `json:"target_id" binding:"required"`
	Effect       domain.Effect       `json:"effect"`
	InheritScope domain.InheritScope `json:"inherit_scope"`
}

func (h *RBACHandler) Grant(c *gin.Context) {
	auth := MustAuth(c)
	var req grantReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	if req.Effect == "" {
		req.Effect = domain.EffectAllow
	}
	if req.InheritScope == "" {
		req.InheritScope = domain.InheritSubtree
	}
	p := &domain.Permission{
		SubjectType: req.SubjectType, SubjectID: req.SubjectID, RoleID: req.RoleID,
		TargetType: req.TargetType, TargetID: req.TargetID,
		Effect: req.Effect, InheritScope: req.InheritScope, CreatedBy: &auth.UserID,
	}
	if err := h.svc.Grant(c.Request.Context(), p); err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, p)
}

func (h *RBACHandler) Revoke(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	if err := h.svc.Revoke(c.Request.Context(), id); err != nil {
		response.Fail(c, err)
		return
	}
	response.NoContent(c)
}

type checkReq struct {
	SubjectID  domain.UUID      `json:"subject_id" binding:"required"`
	TargetType domain.TargetType `json:"target_type" binding:"required"`
	TargetID   domain.UUID      `json:"target_id" binding:"required"`
	Action     domain.Action    `json:"action" binding:"required"`
}

// Check evaluates whether a subject may perform an action on a target
// (POST /permissions/check). Returns {allowed, reason}.
func (h *RBACHandler) Check(c *gin.Context) {
	var req checkReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	dec, err := h.engine.Check(c.Request.Context(), req.SubjectID, nil, req.TargetType, req.TargetID, req.Action)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"allowed": dec.Allowed, "reason": dec.Reason})
}
