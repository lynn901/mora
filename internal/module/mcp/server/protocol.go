// Package server implements the MCP protocol layer: JSON-RPC 2.0 framing, the
// initialize/capabilities handshake, tools/resources dispatch, and session
// management. Transports (HTTP/SSE, stdio) are separate files sharing this
// core (design doc 06 §2/§3/§8).
package server

import "encoding/json"

// JSON-RPC 2.0 standard error codes (per spec §5.1) plus MCP/server codes in
// the -32000 to -32099 reserved band.
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
	// Server-defined errors (reserved band -32000..-32099).
	ErrCodeUnauthorized = -32001
	ErrCodeForbidden    = -32003
	ErrCodeRateLimited  = -32004
	ErrCodeScopeDenied  = -32005
)

// Request is a JSON-RPC 2.0 request/notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // may be number or string; nil for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result/Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// --- MCP initialize / capabilities types (design doc 06 §3) ---

// InitializeParams is the Agent's initialize request body.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// ClientInfo identifies the Agent client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the server's initialize response.
type InitializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    ServerCaps `json:"capabilities"`
	ServerInfo      ServerInfo `json:"serverInfo"`
}

// ServerCaps advertises the server's supported capabilities (design doc 06 §3.2).
type ServerCaps struct {
	Tools     *ToolCaps     `json:"tools,omitempty"`
	Resources *ResourceCaps `json:"resources,omitempty"`
	Logging   *struct{}     `json:"logging,omitempty"`
}

// ToolCaps advertises tools capability.
type ToolCaps struct {
	ListChanged bool `json:"listChanged"`
}

// ResourceCaps advertises resources capability.
type ResourceCaps struct {
	List        bool `json:"list"`
	Read        bool `json:"read"`
	ListChanged bool `json:"listChanged"`
}

// ServerInfo identifies the MCP server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// --- tools/list & tools/call types (design doc 06 §5) ---

// ToolDef is one tool as advertised in tools/list.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ToolsListResult is the tools/list response.
type ToolsListResult struct {
	Tools      []ToolDef `json:"tools"`
	NextCursor *string   `json:"nextCursor,omitempty"`
}

// ToolCallParams is the params of a tools/call request.
type ToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Content is one content item in a tools/call result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCallResult is the tools/call response body.
type ToolCallResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// --- resources/list & resources/read types (design doc 06 §4) ---

// ResourceDef is one resource as advertised in resources/list.
type ResourceDef struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

// ResourcesListResult is the resources/list response.
type ResourcesListResult struct {
	Resources  []ResourceDef `json:"resources"`
	NextCursor *string       `json:"nextCursor,omitempty"`
}

// ResourceReadParams is the params of a resources/read request.
type ResourceReadParams struct {
	URI string `json:"uri"`
}

// ResourceContent is one resource read result entry.
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

// ResourceReadResult is the resources/read response body.
type ResourceReadResult struct {
	Contents []ResourceContent `json:"contents"`
}

// PingResult is the trivial ping response.
type PingResult struct{}
