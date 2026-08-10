package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
)

// AuditMiddleware records mutating operations to the append-only audit log.
// Read operations (GET) are not audited at the HTTP layer to reduce volume;
// sensitive reads (e.g. MCP tool calls) are audited at the service layer.
func AuditMiddleware(logger *audit.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if logger == nil {
			return
		}
		method := c.Request.Method
		if method != "POST" && method != "PATCH" && method != "DELETE" {
			return
		}
		st := MustAuth(c)
		uid := st.UserID
		// Attribute internal-service callers as their service identity, not the
		// (possibly empty) user id; the actor type records which path was used.
		actorType := "user"
		if st.IsServiceCaller {
			actorType = "service"
		}
		// Surface any deprecation note the auth middleware stashed (e.g. a caller
		// still sending the legacy X-Identity-* headers). Best-effort: absence
		// is normal.
		var detail gin.H = gin.H{"path": c.Request.URL.Path, "status": c.Writer.Status()}
		if note, ok := c.Get(ctxDelegatedAudit); ok && note != "" {
			detail["deprecation"] = note
		}
		logger.Record(c.Request.Context(), actorType, &uid,
			"http."+method+"."+c.FullPath(),
			"", nil, detail,
			c.ClientIP(), c.Request.UserAgent())
	}
}

// SetAuditTarget can be called by handlers to enrich the audit record with the
// resource target before the deferred middleware flushes.
func SetAuditTarget(c *gin.Context, targetType string, targetID *domain.UUID) {
	c.Set("audit_target_type", targetType)
	if targetID != nil {
		c.Set("audit_target_id", *targetID)
	}
}
