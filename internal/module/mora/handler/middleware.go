package handler

// Package handler wires HTTP handlers for all mora domains per the API contract
// (04-api-contract.md). Middleware: auth (JWT), audit, rate limit.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	"github.com/lynn901/mora/internal/platform/auth"
	"github.com/lynn901/mora/internal/platform/authz"
	"github.com/lynn901/mora/internal/platform/ratelimit"
)

// contextKey for storing auth state.
const ctxAuth = "auth_ctx"

// ctxDelegatedAudit carries the deprecation note emitted when a caller still
// sends the legacy X-Identity-* headers. AuditMiddleware reads it (best-effort)
// so the deprecation is observable without blocking the request.
const ctxDelegatedAudit = "delegated_audit"

// AuthMiddleware validates the JWT Bearer token and injects AuthContext.
//
// It handles three distinct credential shapes (design doc 13 §4.4, D7):
//
//  1. INTERNAL_SERVICE_TOKEN (trusted service identity only). Proves the caller
//     is a trusted internal service (the MCP Server). It NO LONGER represents
//     the end-principal's authority: the spec (§4.4 step 2) requires that an
//     internal call lacking a delegated context degrade to the service
//     account's own restricted permissions — never fallback to admin.
//  2. Delegated JWT. When the Bearer is a delegated session token (issued via
//     POST /internal/v1/authz/delegated), VerifyDelegated validates the
//     signature+expiry against the authoritative server-side row
//     (delegated_sessions) and the workspace authz revision. On success the
//     AuthState reflects the delegated acting principal (UserID=acting_user_id,
//     AgentID, Actions); IsAdmin comes only from the claims, never a header.
//  3. User JWT. Browser/session callers present the standard user JWT, verified
//     by auth.TokenManager.
//
// A delegated JWT and a user JWT share the same HS256 secret, so the two are
// told apart by claims SHAPE, not by attempting verify: a delegated token
// always carries a `sid` (session id) claim; a user JWT never does (auth.Claims
// has no such field and Issue never sets one). looksDelegated peeks at the
// unverified payload solely to route to the right validator — both branches
// still enforce signature, so a forged `sid` cannot bypass verification.
//
// The legacy X-Identity-Id / X-Identity-Admin headers are DEPRECATED (§4.4
// step 3): if present they are read-and-ignored and a deprecation audit note is
// stashed for AuditMiddleware. They are not trusted for identity or admin.
func AuthMiddleware(tm *auth.TokenManager, internalToken string, dm *authz.DelegatedManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token := auth.ExtractBearer(header)

		// Deprecation watch (§4.4 step 3): X-Identity-* headers are no longer
		// authoritative. Their mere presence is audited and ignored — a caller
		// that still sends them is not privileged by them.
		if iid := c.GetHeader("X-Identity-Id"); iid != "" || c.GetHeader("X-Identity-Admin") != "" {
			c.Set(ctxDelegatedAudit, "deprecated X-Identity-* header ignored")
		}

		// Trusted internal service credential (MCP → mora-api). Proves service
		// identity ONLY — it never grants the end-principal's authority.
		if internalToken != "" && token == internalToken {
			// §4.4: a trusted internal service WITHOUT a delegated context degrades
			// to its own service_account identity with restricted capability. It
			// must NOT fallback to admin. IsAdmin is false; the caller proceeds
			// under whatever RBAC the service account actually has.
			c.Set(ctxAuth, AuthState{
				IsAdmin:         false,
				SubjectType:     domain.SubjectServiceAccount,
				IsServiceCaller: true,
			})
			c.Next()
			return
		}

		// A delegated JWT carries the acting principal. Route by claims shape:
		// only a delegated token has a `sid`. A failed delegated verify is fatal
		// (do not fall through to the user-JWT path): a caller presenting an
		// invalid delegated context must be refused, not retried as a user.
		if dm != nil && token != "" && looksDelegated(token) {
			claims, err := dm.VerifyDelegated(c.Request.Context(), token)
			if err != nil {
				// Invalid delegated context (bad signature, expired, revoked, or
				// stale revision). Refuse — no fallback to user-JWT or admin.
				response.Fail(c, pkgerr.Unauthorized("invalid delegated context"))
				c.Abort()
				return
			}
			st := AuthState{
				IsServiceCaller: true,
				WorkspaceID:     parseUUIDErrLoose(claims.WorkspaceID),
				Actions:         claims.Actions,
			}
			if claims.ActingUserID != "" {
				if uid, err := uuid.Parse(claims.ActingUserID); err == nil {
					st.UserID = uid
					st.SubjectType = domain.SubjectUser
				}
			}
			if claims.AgentID != "" {
				if aid, err := uuid.Parse(claims.AgentID); err == nil {
					st.AgentID = aid
					// An agent principal acts as agent; UserID (if set) is the
					// user it acts on behalf of.
					st.SubjectType = domain.SubjectAgent
				}
			}
			// IsAdmin is derived ONLY from the delegated session's capability
			// envelope — never from a client header. A delegated service
			// principal is not a platform admin unless the session carried the
			// admin action, which Phase 0 does not grant to delegated sessions.
			c.Set(ctxAuth, st)
			c.Next()
			return
		}

		// User JWT path (browser/session callers).
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
			UserID:      uid,
			Name:        claims.Name,
			Email:       claims.Email,
			Groups:      groups,
			IsAdmin:     claims.IsAdmin,
			SubjectType: domain.SubjectUser,
		})
		c.Next()
	}
}

// looksDelegated reports whether token has the claims shape of a delegated
// session JWT (a non-empty `sid` claim). It decodes the unverified JWT payload
// for ROUTING ONLY — it is not an authentication decision. Both the delegated
// and user-JWT branches enforce signature independently, so a forged `sid`
// cannot bypass verification: it only steers the token to VerifyDelegated,
// which rejects a bad signature or unknown session id.
//
// A user JWT (auth.Claims) never carries `sid` (Issue never sets one), so this
// reliably distinguishes the two shared-secret HS256 token families.
func looksDelegated(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var probe struct {
		SessionID string `json:"sid"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return false
	}
	return probe.SessionID != ""
}

// AuthState is the authenticated caller context, injected by AuthMiddleware.
type AuthState struct {
	UserID  domain.UUID
	Name    string
	Email   string
	Groups  []domain.UUID
	IsAdmin bool

	// SubjectType is the resolved principal kind (user / agent /
	// service_account). Internal-service callers without a delegated context
	// resolve to service_account (§4.4).
	SubjectType domain.SubjectType
	// AgentID is set when the principal is an agent acting on behalf of a user
	// (delegated context only).
	AgentID domain.UUID
	// WorkspaceID is the workspace scope carried by a delegated context.
	WorkspaceID domain.UUID
	// Actions is the capability envelope from the delegated session (§5.1).
	Actions []string
	// IsServiceCaller marks internal-service (INTERNAL_SERVICE_TOKEN or
	// delegated) callers so audit/rate-limit can attribute them correctly.
	IsServiceCaller bool
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

// parseUUIDErrLoose parses a UUID string returning uuid.Nil (not an error) on
// failure. Used for delegated-context fields that may be empty (workspace_id,
// agent_id) where a nil is a valid "absent" signal rather than an error.
func parseUUIDErrLoose(s string) domain.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return domain.UUID{}
	}
	return id
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
