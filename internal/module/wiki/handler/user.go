package handler

// Package handler (user.go) exposes the identity-listing query endpoints
// added for RBAC UI / author-display (04-api-contract §3.5):
//   GET /api/v1/users  — users within the caller's visible scope (RBAC filtered)
//   GET /api/v1/roles  — role dictionary aligned with Permission.role_id

import (
	"github.com/gin-gonic/gin"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
	"github.com/wiki/wiki-backend/internal/pkg/response"
)

// UserHandler exposes GET /users (RBAC-scoped user listing).
type UserHandler struct {
	users service.UserRepo
}

func NewUserHandler(users service.UserRepo) *UserHandler {
	return &UserHandler{users: users}
}

func (h *UserHandler) List(c *gin.Context) {
	auth := MustAuth(c)
	q := service.UserQuery{
		ViewerID: auth.UserID,
		IsAdmin:  auth.IsAdmin,
		Params:   pagination.From(c),
		Search:   c.Query("search"),
	}
	items, total, err := h.users.List(c.Request.Context(), q)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Paged(c, items, total, q.Params.Page, q.Params.PageSize)
}

// RoleHandler exposes GET /roles (role dictionary).
type RoleHandler struct {
	roles service.RoleRepo
}

func NewRoleHandler(roles service.RoleRepo) *RoleHandler {
	return &RoleHandler{roles: roles}
}

func (h *RoleHandler) List(c *gin.Context) {
	items, err := h.roles.List(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}
