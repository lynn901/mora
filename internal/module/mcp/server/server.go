package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lynn901/mora/internal/module/mcp/audit"
	"github.com/lynn901/mora/internal/module/mcp/auth"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/observ"
)

// ToolHandler is implemented by each MCP tool. The server dispatches tools/call
// to the registered handler; the handler pulls the caller AuthContext from ctx
// via auth.FromContext.
type ToolHandler interface {
	// Definition returns the tool's name/description/inputSchema for tools/list.
	Definition() ToolDef
	// IsWrite reports whether this is a write tool (governs the rate-limit
	// bucket and scope gating — design doc 06 §5.1/§7.2).
	IsWrite() bool
	// Execute runs the tool. Read tools MUST translate no-permission into an
	// empty success result (existence-leak prevention, design doc 06 §6.4);
	// write tools return domainerr.ErrScopeDenied/ErrForbidden on denial.
	Execute(ctx context.Context, args map[string]any) (*ToolCallResult, error)
}

// ResourceRegistry resolves resources/list and resources/read. Implemented by
// the resource package; the server delegates to it.
type ResourceRegistry interface {
	List(ctx context.Context) ([]ResourceDef, error)
	Read(ctx context.Context, uri string) (*ResourceReadResult, error)
}

// sessionCtxKey carries the active MCP session id for audit linkage.
type sessionCtxKey struct{}

// WithSessionID returns a ctx carrying the MCP session id.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, id)
}

// SessionIDFromContext returns the carried session id, if any.
func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// Server is the MCP protocol core: it holds tool/resource registries and
// dispatches JSON-RPC methods. Transports feed it parsed Requests and render
// its Responses (design doc 06 §8).
type Server struct {
	tools     map[string]ToolHandler
	toolOrder []string // stable ordering for tools/list
	resources ResourceRegistry
	sessions  SessionStore
	audit     audit.Store
	limiter   auth.RateLimiter
	rateRead  int
	rateWrite int

	protocolVersion string
	serverName      string
	serverVersion   string
}

// Option configures a Server.
type Option func(*Server)

// WithRateLimits overrides the default per-token rate limits (req/min).
func WithRateLimits(read, write int) Option {
	return func(s *Server) {
		if read > 0 {
			s.rateRead = read
		}
		if write > 0 {
			s.rateWrite = write
		}
	}
}

// WithProtocolVersion overrides the advertised protocol version.
func WithProtocolVersion(v string) Option {
	return func(s *Server) { s.protocolVersion = v }
}

// NewServer builds an MCP Server with the given registries and stores.
func NewServer(resources ResourceRegistry, sessions SessionStore, aud audit.Store, limiter auth.RateLimiter, name, version string, opts ...Option) *Server {
	s := &Server{
		tools:           make(map[string]ToolHandler),
		resources:       resources,
		sessions:        sessions,
		audit:           aud,
		limiter:         limiter,
		rateRead:        100,
		rateWrite:       20,
		protocolVersion: "2025-06-18",
		serverName:      name,
		serverVersion:   version,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// RegisterTool registers a tool handler. Tools appear in tools/list in
// registration order.
func (s *Server) RegisterTool(h ToolHandler) {
	name := h.Definition().Name
	if _, exists := s.tools[name]; !exists {
		s.toolOrder = append(s.toolOrder, name)
	}
	s.tools[name] = h
}

// Handle dispatches a single JSON-RPC request. It returns the response (nil for
// notifications) and, for initialize, the new session id (so the transport can
// set the Mcp-Session-Id header — design doc 06 §2.2).
func (s *Server) Handle(ctx context.Context, req *Request) (*Response, string) {
	// Notifications (no id) get no response.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	respond := func(result any, err *RPCError) *Response {
		if isNotification && err == nil {
			return nil
		}
		if isNotification {
			return nil
		}
		return &Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: err}
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, req, respond)
	case "notifications/initialized":
		// Acknowledgement notification; no response, no state change.
		return respond(nil, nil), ""
	case "ping":
		return respond(PingResult{}, nil), ""
	case "tools/list":
		return s.handleToolsList(ctx, respond), ""
	case "tools/call":
		return s.handleToolsCall(ctx, req, respond), ""
	case "resources/list":
		return s.handleResourcesList(ctx, respond), ""
	case "resources/read":
		return s.handleResourcesRead(ctx, req, respond), ""
	default:
		return respond(nil, &RPCError{Code: ErrCodeMethodNotFound, Message: "method not found"}), ""
	}
}

func (s *Server) handleInitialize(ctx context.Context, req *Request, respond func(any, *RPCError) *Response) (*Response, string) {
	var params InitializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return respond(nil, &RPCError{Code: ErrCodeInvalidParams, Message: "invalid initialize params"}), ""
		}
	}
	ac := auth.FromContext(ctx)
	clientInfo := map[string]any{"name": params.ClientInfo.Name, "version": params.ClientInfo.Version}
	sess := &Session{
		TokenID:      tokenIDFor(ac),
		Transport:    "http_sse",
		ClientInfo:   clientInfo,
		Capabilities: decodeCaps(params.Capabilities),
	}
	if err := s.sessions.Create(ctx, sess); err != nil {
		return respond(nil, &RPCError{Code: ErrCodeInternal, Message: "session create failed"}), ""
	}
	observ.MCPSessionsGauge.Inc()
	result := InitializeResult{
		ProtocolVersion: s.protocolVersion,
		Capabilities: ServerCaps{
			Tools:     &ToolCaps{ListChanged: true},
			Resources: &ResourceCaps{List: true, Read: true, ListChanged: true},
			Logging:   &struct{}{},
		},
		ServerInfo: ServerInfo{Name: s.serverName, Version: s.serverVersion},
	}
	return respond(result, nil), sess.ID
}

func (s *Server) handleToolsList(_ context.Context, respond func(any, *RPCError) *Response) *Response {
	defs := make([]ToolDef, 0, len(s.toolOrder))
	for _, name := range s.toolOrder {
		if h, ok := s.tools[name]; ok {
			defs = append(defs, h.Definition())
		}
	}
	return respond(ToolsListResult{Tools: defs}, nil)
}

func (s *Server) handleToolsCall(ctx context.Context, req *Request, respond func(any, *RPCError) *Response) *Response {
	var params ToolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return respond(nil, &RPCError{Code: ErrCodeInvalidParams, Message: "invalid tools/call params"})
	}
	h, ok := s.tools[params.Name]
	if !ok {
		return respond(nil, &RPCError{Code: ErrCodeInvalidParams, Message: fmt.Sprintf("unknown tool: %s", params.Name)})
	}
	ac := auth.FromContext(ctx)

	// Rate limit: write tools use the stricter write bucket (design doc 06 §7.2).
	bucket := auth.BucketRead
	if h.IsWrite() {
		bucket = auth.BucketWrite
	}
	if decision, err := s.limiter.Allow(ctx, tokenIDFor(ac), bucket, s.limitFor(bucket)); err == nil && !decision.Allowed {
		observ.MCPRateLimitedTotal.WithLabelValues(string(bucket)).Inc()
		s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusError, "", 0)
		return respond(nil, &RPCError{
			Code: ErrCodeRateLimited, Message: "rate limit exceeded",
			Data: map[string]any{"retry_after_ms": decision.RetryAfter.Milliseconds()},
		})
	}

	start := time.Now()
	result, err := h.Execute(ctx, params.Arguments)
	duration := time.Since(start)
	target := targetOf(params.Name, params.Arguments)
	if err != nil {
		// Map domain errors to JSON-RPC errors (design doc 06 §6.4).
		switch {
		case errors.Is(err, domainerr.ErrScopeDenied):
			s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusForbidden, target, duration)
			return respond(nil, &RPCError{Code: ErrCodeScopeDenied, Message: "token scope forbids write operations"})
		case errors.Is(err, domainerr.ErrForbidden):
			observ.MCPForbiddenTotal.WithLabelValues(params.Name).Inc()
			s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusForbidden, target, duration)
			return respond(nil, &RPCError{Code: ErrCodeForbidden, Message: "forbidden"})
		case errors.Is(err, domainerr.ErrRateLimited):
			observ.MCPRateLimitedTotal.WithLabelValues(string(bucket)).Inc()
			s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusError, target, duration)
			return respond(nil, &RPCError{Code: ErrCodeRateLimited, Message: "rate limit exceeded"})
		case errors.Is(err, domainerr.ErrInvalidParams):
			s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusError, target, duration)
			return respond(nil, &RPCError{Code: ErrCodeInvalidParams, Message: err.Error()})
		default:
			s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusError, target, duration)
			// Surface tool-internal errors as an isError result so the Agent
			// sees the message rather than a transport-level rejection.
			return respond(&ToolCallResult{
				Content: []Content{{Type: "text", Text: "tool error: " + err.Error()}},
				IsError: true,
			}, nil)
		}
	}
	s.recordAudit(ctx, ac, params.Name, params.Arguments, audit.StatusSuccess, target, duration)
	observ.MCPToolDuration.WithLabelValues(params.Name).Observe(duration.Seconds())
	if result == nil {
		result = &ToolCallResult{}
	}
	return respond(result, nil)
}

func (s *Server) handleResourcesList(ctx context.Context, respond func(any, *RPCError) *Response) *Response {
	defs, err := s.resources.List(ctx)
	if err != nil {
		return respond(nil, &RPCError{Code: ErrCodeInternal, Message: err.Error()})
	}
	return respond(ResourcesListResult{Resources: defs}, nil)
}

func (s *Server) handleResourcesRead(ctx context.Context, req *Request, respond func(any, *RPCError) *Response) *Response {
	var params ResourceReadParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return respond(nil, &RPCError{Code: ErrCodeInvalidParams, Message: "invalid resources/read params"})
	}
	ac := auth.FromContext(ctx)
	// Resources are read-only: use the read rate-limit bucket.
	if decision, err := s.limiter.Allow(ctx, tokenIDFor(ac), auth.BucketRead, s.rateRead); err == nil && !decision.Allowed {
		observ.MCPRateLimitedTotal.WithLabelValues(string(auth.BucketRead)).Inc()
		return respond(nil, &RPCError{Code: ErrCodeRateLimited, Message: "rate limit exceeded"})
	}
	start := time.Now()
	result, err := s.resources.Read(ctx, params.URI)
	duration := time.Since(start)
	toolName := "resources/read:" + params.URI
	target := params.URI
	if err != nil {
		if errors.Is(err, domainerr.ErrNotFound) || errors.Is(err, domainerr.ErrForbidden) {
			// Existence-leak prevention: return empty contents, not an error
			// (design doc 06 §6.4).
			s.recordAudit(ctx, ac, toolName, nil, audit.StatusSuccess, target, duration)
			return respond(&ResourceReadResult{Contents: []ResourceContent{}}, nil)
		}
		s.recordAudit(ctx, ac, toolName, nil, audit.StatusError, target, duration)
		return respond(nil, &RPCError{Code: ErrCodeInternal, Message: err.Error()})
	}
	s.recordAudit(ctx, ac, toolName, nil, audit.StatusSuccess, target, duration)
	if result == nil {
		result = &ResourceReadResult{}
	}
	return respond(result, nil)
}

// limitFor returns the configured req/min for a bucket.
func (s *Server) limitFor(bucket auth.LimitBucket) int {
	if bucket == auth.BucketWrite {
		return s.rateWrite
	}
	return s.rateRead
}

func (s *Server) recordAudit(ctx context.Context, ac *auth.AuthContext, tool string, params map[string]any, status audit.ResultStatus, target string, duration time.Duration) {
	if s.audit == nil {
		return
	}
	rec := &audit.Record{
		SessionID:      SessionIDFromContext(ctx),
		TokenID:        tokenIDFor(ac),
		IdentityID:     identityIDFor(ac),
		ToolName:       tool,
		ParamsSummary:  summarizeParams(params),
		ResultStatus:   status,
		TargetResource: target,
		DurationMS:     int(duration.Milliseconds()),
	}
	// Best-effort: audit failures must not break the request flow.
	_ = s.audit.Record(ctx, rec)
}

// summarizeParams produces a redacted parameter summary for audit: long text is
// truncated and known-sensitive keys are stripped (design doc 06 §7.1).
func summarizeParams(params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		lk := k
		switch lk {
		case "token", "secret", "password", "api_key":
			out[k] = "[redacted]"
			continue
		}
		switch val := v.(type) {
		case string:
			if len(val) > 200 {
				out[k] = val[:200] + "...(truncated)"
			} else {
				out[k] = val
			}
		default:
			out[k] = v
		}
	}
	return out
}

func targetOf(toolName string, args map[string]any) string {
	if args == nil {
		return ""
	}
	for _, k := range []string{"document_id", "workspace_id", "query"} {
		if v, ok := args[k]; ok {
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return ""
}

func tokenIDFor(ac *auth.AuthContext) string {
	if ac == nil {
		return ""
	}
	return ac.TokenID
}

func identityIDFor(ac *auth.AuthContext) string {
	if ac == nil {
		return ""
	}
	return ac.IdentityID
}

func decodeCaps(caps map[string]any) map[string]any {
	if caps == nil {
		return map[string]any{}
	}
	return caps
}
