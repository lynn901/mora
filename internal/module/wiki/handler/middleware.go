package handler

// Package handler wires HTTP handlers for all wiki domains per the API contract
// (04-api-contract.md). Middleware: auth (JWT), audit, rate limit.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/platform/auth"
	"github.com/wiki/wiki-backend/internal/platform/ratelimit"
	pkgerr "github.com/wiki/wiki-backend/internal/pkg/errors"
	"github.com/wiki/wiki-backend/internal/pkg/response"
)

// contextKey for storing auth state.
const ctxAuth = "auth_ctx"

// AuthMiddleware validates the JWT Bearer token and injects AuthContext.
//
// It also accepts the INTERNAL_SERVICE_TOKEN as a trusted internal-service
// credential (design doc 02 §2.2): when the Bearer token matches the configured
// internal token, the caller is treated as a trusted service (the MCP Server
// calling wiki-api). The end-principal is carried in the X-Identity-Id header
// (set by the MCP HTTPClient) and used as the AuthState UserID so RBAC is
// enforced as that principal; if absent, the caller is treated as admin.
func AuthMiddleware(tm *auth.TokenManager, internalToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token := auth.ExtractBearer(header)

		// Trusted internal service token (MCP → wiki-api).
		if internalToken != "" && token == internalToken {
			st := AuthState{IsAdmin: true}
			if iid := c.GetHeader("X-Identity-Id"); iid != "" {
				if uid := parseUUID(iid); uid != uuid.Nil {
					st.UserID = uid
					// The MCP server propagates X-Identity-Admin when the bound
					// token's identity is the platform admin, so the admin bypass
					// (RBAC, search visible_to) applies through the MCP path too.
					st.IsAdmin = c.GetHeader("X-Identity-Admin") == "true"
				}
			}
			c.Set(ctxAuth, st)
			c.Next()
			return
		}

		if token == "" {
			response.Fail(c, pkgerr.Unauthorized("missing token"))
			c.Abort()
			return
		}
		claims, err := tm.Verify(token)
		if err != nil {
			response.Fail(c, pkgerr.Unauthorized("invalid or expired token"))
			c.Abort()
			return
		}
		uid, err := uuid.Parse(claims.UserID)
		if err != nil {
			response.Fail(c, pkgerr.Unauthorized("invalid token subject"))
			c.Abort()
			return
		}
		groups := make([]domain.UUID, 0, len(claims.Groups))
		for _, g := range claims.Groups {
			if id := parseUUID(g); id != uuid.Nil {
				groups = append(groups, id)
			}
		}
		c.Set(ctxAuth, AuthState{
			UserID:  uid,
			Name:    claims.Name,
			Email:   claims.Email,
			Groups:  groups,
			IsAdmin: claims.IsAdmin,
		})
		c.Next()
	}
}

// AuthState is the authenticated caller context, injected by AuthMiddleware.
type AuthState struct {
	UserID  domain.UUID
	Name    string
	Email   string
	Groups  []domain.UUID
	IsAdmin bool
}

// MustAuth extracts the AuthState set by AuthMiddleware.
func MustAuth(c *gin.Context) AuthState {
	v, ok := c.Get(ctxAuth)
	if !ok {
		return AuthState{}
	}
	return v.(AuthState)
}

// RateLimitMiddleware enforces a per-user rate limit.
func RateLimitMiddleware(limiter *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		st := MustAuth(c)
		if !limiter.Allow(st.UserID.String()) {
			c.Header("Retry-After", "60")
			response.Fail(c, pkgerr.RateLimited("rate limit exceeded"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// CORSMiddleware adds permissive CORS for development (production uses Ingress).
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,If-Match,Idempotency-Key")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func parseUUID(s string) domain.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return domain.UUID{}
	}
	return id
}

func parseUUIDErr(s string) (domain.UUID, error) {
	return uuid.Parse(s)
}

// splitTags parses a comma-separated tag query param.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
