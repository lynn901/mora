package handler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/platform/auth"
	pkgerr "github.com/wiki/wiki-backend/internal/pkg/errors"
	"github.com/wiki/wiki-backend/internal/pkg/response"
	"golang.org/x/crypto/bcrypt"
)

// UserLookup authenticates a user by email/password for login.
type UserLookup interface {
	Authenticate(ctx context.Context, email, password string) (*domain.User, []domain.UUID, error)
}

type AuthHandler struct {
	users UserLookup
	tm    *auth.TokenManager
}

func NewAuthHandler(users UserLookup, tm *auth.TokenManager) *AuthHandler {
	return &AuthHandler{users: users, tm: tm}
}

type loginReq struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req loginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	user, groups, err := h.users.Authenticate(c.Request.Context(), req.Email, req.Password)
	if err != nil {
		response.Fail(c, pkgerr.Unauthorized("invalid credentials"))
		return
	}
	gidStrs := make([]string, len(groups))
	for i, g := range groups {
		gidStrs[i] = g.String()
	}
	isAdmin := user.Status == "active" && user.Email == "admin@wiki.local"
	tok, err := h.tm.Issue(user.ID, user.Email, user.Name, gidStrs, isAdmin)
	if err != nil {
		response.Fail(c, errors.New("failed to issue token"))
		return
	}
	response.OK(c, gin.H{"token": tok, "user": user})
}

// HashPassword and CheckPassword wrap bcrypt for local auth.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}
