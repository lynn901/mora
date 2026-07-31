// Package tool implements the MCP Tools exposed by the server (design doc 06
// §5). Each tool implements server.ToolHandler and delegates to the upstream
// WikiClient, enforcing token-scope gating and existence-leak prevention
// locally while RBAC is applied server-side by the Wiki/RAG services.
package tool

import (
	"encoding/json"

	"github.com/wiki/wiki-backend/internal/module/mcp/auth"
	"github.com/wiki/wiki-backend/internal/module/mcp/server"
	"github.com/wiki/wiki-backend/internal/module/mcp/wikiclient"
	domainerr "github.com/wiki/wiki-backend/internal/pkg/errors"
)

// base is the common state shared by all tools: the upstream Wiki client.
type base struct {
	client wikiclient.WikiClient
}

// toWikiAuth converts the MCP AuthContext (from the request) into the
// wikiclient.AuthContext propagated to upstream calls.
func toWikiAuth(ac *auth.AuthContext) *wikiclient.AuthContext {
	if ac == nil {
		return nil
	}
	return &wikiclient.AuthContext{
		TokenID:      ac.TokenID,
		IdentityType: ac.IdentityType,
		IdentityID:   ac.IdentityID,
		IdentityName: ac.IdentityName,
		Scope:        ac.Scope,
		Groups:       ac.Groups,
		IsAdmin:      ac.IsAdmin,
	}
}

// emptyTextResult is the existence-leak-safe empty result for read tools:
// no permission returns an empty success result, never an error (design doc
// 06 §6.4). The Agent cannot infer whether the document exists.
func emptyTextResult() *server.ToolCallResult {
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: ""}},
	}
}

// asTextResult marshals a value as a JSON text content item (the MCP convention
// for structured tool output — design doc 06 §5.4).
func asTextResult(v any) (*server.ToolCallResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &server.ToolCallResult{
		Content: []server.Content{{Type: "text", Text: string(b)}},
	}, nil
}

// requireString extracts a required non-empty string argument.
func requireString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", domainerr.ErrInvalidParams
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", domainerr.ErrInvalidParams
	}
	return s, nil
}

// optString extracts an optional string argument (default "").
func optString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// optInt extracts an optional integer argument (default def).
func optInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

// optBool extracts an optional boolean argument (default def).
func optBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

// optStringSlice extracts an optional []string argument.
func optStringSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
