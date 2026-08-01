package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AuthContext is the resolved caller identity carried through an MCP request.
// It is set on the Gin context by AuthMiddleware and consumed by tools,
// resources, audit, and the upstream MoraClient.
type AuthContext struct {
	TokenID      string
	TokenName    string
	TokenPrefix  string
	IdentityType rbac.IdentityType
	IdentityID   string
	IdentityName string
	Scope        rbac.Scope
	Groups       []string
	IsAdmin      bool
}

// AllowsWrite reports whether the token scope permits write tools (design doc
// 06 §6.3: scope=readonly rejects write tools before any upstream call).
func (a *AuthContext) AllowsWrite() bool {
	return a != nil && a.Scope.AllowsWrite()
}

// contextKey is an unexported type for context.AuthContext keys.
type contextKey struct{}

var authCtxKey = contextKey{}

// WithAuthContext returns a context carrying the AuthContext.
func WithAuthContext(ctx context.Context, a *AuthContext) context.Context {
	return context.WithValue(ctx, authCtxKey, a)
}

// FromContext extracts the AuthContext, or nil if absent.
func FromContext(ctx context.Context) *AuthContext {
	a, _ := ctx.Value(authCtxKey).(*AuthContext)
	return a
}

// FromGin extracts the AuthContext from a Gin context.
func FromGin(c *gin.Context) *AuthContext {
	if v, exists := c.Get("auth_ctx"); exists {
		if a, ok := v.(*AuthContext); ok {
			return a
		}
	}
	return nil
}

// HashToken returns the lowercase hex SHA-256 of a token plaintext. Only the
// hash is stored/looked up (design doc 03 §2.8, 06 §6.2).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ExtractBearer pulls the token from an Authorization: Bearer <token> header
// (design doc 06 §2.2). It also tolerates a raw API-Key style header for
// convenience, normalizing to the bearer scheme.
func ExtractBearer(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", domainerr.ErrUnauthorized
	}
	if strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return strings.TrimSpace(header[7:]), nil
	}
	// Tolerate a bare token (API-Key form) — treat as bearer-equivalent.
	if !strings.Contains(header, " ") {
		return header, nil
	}
	return "", domainerr.ErrUnauthorized
}

// AuthMiddleware resolves the Bearer token on every MCP HTTP request, builds
// the AuthContext, and stores it on the Gin context. Invalid/revoked/expired
// tokens get HTTP 401. It does NOT apply rate limiting — that happens per
// tool/resource call (read vs write buckets) in the server dispatch.
func AuthMiddleware(store TokenStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := ExtractBearer(c.GetHeader("Authorization"))
		if err != nil {
			abortUnauthorized(c, "missing or malformed Authorization")
			return
		}
		hash := HashToken(raw)
		rec, err := store.Lookup(c.Request.Context(), hash)
		if err != nil {
			abortUnauthorized(c, "token lookup failed")
			return
		}
		if rec == nil || !rec.IsValid(time.Now()) {
			abortUnauthorized(c, "invalid, expired, or revoked token")
			return
		}
		ac := &AuthContext{
			TokenID:      rec.ID,
			TokenName:    rec.Name,
			TokenPrefix:  rec.Prefix,
			IdentityType: rec.IdentityType,
			IdentityID:   rec.IdentityID,
			IdentityName: rec.IdentityName,
			Scope:        rec.Scope,
			Groups:       rec.Groups,
			IsAdmin:      rec.IsAdmin,
		}
		c.Set("auth_ctx", ac)
		// Best-effort last-used update.
		_ = store.TouchLastUsed(c.Request.Context(), rec.ID)
		c.Next()
	}
}

func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"jsonrpc": "2.0",
		"error": gin.H{
			"code":    -32001,
			"message": msg,
		},
	})
}

// CheckWriteScope returns ErrScopeDenied when the token scope forbids writes.
// Called by write tools before dispatching to the upstream MoraClient.
func CheckWriteScope(a *AuthContext) error {
	if a == nil || !a.AllowsWrite() {
		return domainerr.ErrScopeDenied
	}
	return nil
}
