package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wiki/wiki-backend/internal/module/mcp/audit"
	"github.com/wiki/wiki-backend/internal/module/mcp/auth"
)

const (
	// headerSessionID is the MCP streamable-HTTP session header (design doc 06 §2.2).
	headerSessionID = "Mcp-Session-Id"
)

// HTTPTransport mounts the protected MCP HTTP/SSE transport onto a Gin route
// group (the group must already apply AuthMiddleware, e.g. via
// r.Group("/").Use(auth.AuthMiddleware(...))). It exposes:
//   - POST   /mcp          JSON-RPC over HTTP (initialize, tools/*, resources/*)
//   - GET    /mcp          SSE stream (server push channel — design doc 06 §2.2)
//   - DELETE /mcp          close session
//   - GET    /mcp/sessions admin: list sessions
//   - GET    /mcp/tool-calls admin: audit query
//
// Public routes (/mcp/health, /metrics) are mounted separately via PublicRoutes
// so liveness probes and scrapes do not require a token.
func (s *Server) HTTPTransport(r gin.IRoutes, auditStore audit.Store) {
	r.POST("/mcp", s.handlePost)
	r.GET("/mcp", s.handleSSE)
	r.DELETE("/mcp", s.handleDelete)
	r.GET("/mcp/sessions", s.handleListSessions)
	r.GET("/mcp/tool-calls", s.handleListToolCalls(auditStore))
}

// PublicRoutes mounts unauthenticated routes: the health check and Prometheus
// metrics endpoint (design doc 04 §10 / 07 §4).
func (s *Server) PublicRoutes(r *gin.Engine) {
	r.GET("/mcp/health", s.handleHealth)
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
}

// handlePost is the main JSON-RPC endpoint. It supports single requests and
// JSON-RPC batches (spec §6). On initialize it echoes the new session id via
// the Mcp-Session-Id response header.
func (s *Server) handlePost(c *gin.Context) {
	ac := auth.FromGin(c)
	if ac == nil {
		// AuthMiddleware already aborted; defensive guard.
		return
	}
	sessionID := c.GetHeader(headerSessionID)

	var raw json.RawMessage
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusOK, errorResponse(nil, ErrCodeParseError, "parse error: "+err.Error()))
		return
	}

	// Batch?
	if len(raw) > 0 && raw[0] == '[' {
		var reqs []*Request
		if err := json.Unmarshal(raw, &reqs); err != nil {
			c.JSON(http.StatusOK, errorResponse(nil, ErrCodeParseError, "invalid batch"))
			return
		}
		resps := make([]*Response, 0, len(reqs))
		newSession := ""
		for _, req := range reqs {
			ctx := buildCtx(c, ac, sessionID)
			resp, sid := s.Handle(ctx, req)
			if sid != "" {
				newSession = sid
			}
			if resp != nil {
				resps = append(resps, resp)
			}
		}
		if newSession != "" {
			c.Header(headerSessionID, newSession)
		}
		if len(resps) == 0 {
			c.Status(http.StatusAccepted)
			return
		}
		c.JSON(http.StatusOK, resps)
		return
	}

	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusOK, errorResponse(nil, ErrCodeParseError, "invalid request"))
		return
	}
	ctx := buildCtx(c, ac, sessionID)
	resp, newSession := s.Handle(ctx, &req)
	if newSession != "" {
		c.Header(headerSessionID, newSession)
	}
	if resp == nil {
		// Notification: no response body.
		c.Status(http.StatusAccepted)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// handleSSE opens an SSE stream for server→client push (design doc 06 §2.2).
// MVP holds the connection open with periodic keepalive pings; the transport
// is ready to stream notifications/progress when long-running tools emit them.
func (s *Server) handleSSE(c *gin.Context) {
	if auth.FromGin(c) == nil {
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	// Initial endpoint event per MCP streamable HTTP.
	_, _ = c.Writer.WriteString("event: endpoint\ndata: /mcp\n\n")
	c.Writer.Flush()
	notify := c.Request.Context().Done()
	for {
		select {
		case <-notify:
			return
		default:
			_, _ = c.Writer.WriteString(": keepalive\n\n")
			c.Writer.Flush()
		}
	}
}

// handleDelete closes the MCP session identified by Mcp-Session-Id.
func (s *Server) handleDelete(c *gin.Context) {
	if auth.FromGin(c) == nil {
		return
	}
	sid := c.GetHeader(headerSessionID)
	if sid != "" {
		_ = s.sessions.End(c.Request.Context(), sid)
	}
	c.Status(http.StatusOK)
}

// handleHealth is the unauthenticated health check (design doc 04 §10).
func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": s.serverVersion,
		"name":    s.serverName,
	})
}

// handleListSessions lists active sessions (admin, design doc 04 §10).
func (s *Server) handleListSessions(c *gin.Context) {
	if auth.FromGin(c) == nil {
		return
	}
	n, _ := s.sessions.CountActive(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"active": n})
}

// handleListToolCalls exposes the audit query endpoint (design doc 04 §10).
func (s *Server) handleListToolCalls(store audit.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if auth.FromGin(c) == nil {
			return
		}
		if store == nil {
			c.JSON(http.StatusOK, gin.H{"items": []any{}})
			return
		}
		f := audit.Filter{TokenID: c.Query("token_id"), ToolName: c.Query("tool_name"), Limit: 100}
		records, err := store.List(c.Request.Context(), f)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": records})
	}
}

// buildCtx assembles a context.Context carrying the AuthContext and session id.
func buildCtx(c *gin.Context, ac *auth.AuthContext, sessionID string) context.Context {
	ctx := auth.WithAuthContext(c.Request.Context(), ac)
	if sessionID != "" {
		ctx = WithSessionID(ctx, sessionID)
	}
	return ctx
}

// errorResponse builds a JSON-RPC error Response.
func errorResponse(id json.RawMessage, code int, msg string) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}
