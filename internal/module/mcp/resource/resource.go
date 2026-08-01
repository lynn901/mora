// Package resource implements the MCP Resources exposed by the server (design
// doc 06 §4). Resources are read-only knowledge structure URIs (wiki:// scheme)
// that let an Agent discover the Mora layout. All handlers delegate to the
// upstream MoraClient; no read permission surfaces as a not-found error which
// the server converts to empty contents (existence-leak prevention, §6.4).
package resource

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	"github.com/lynn901/mora/internal/module/mcp/server"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

const mimeTypeJSON = "application/json"

// Registry implements server.ResourceRegistry, dispatching wiki:// URIs to the
// matching handler backed by the MoraClient.
type Registry struct {
	client moraclient.MoraClient
}

// NewRegistry builds a resource Registry.
func NewRegistry(client moraclient.MoraClient) *Registry {
	return &Registry{client: client}
}

// List returns the resource definitions visible to the caller (design doc 06
// §4.1). It always advertises wiki://workspaces, then for each visible
// workspace advertises its tree and tags resources. Document meta URIs are
// discovered through the tree (which lists documents), keeping List bounded.
func (r *Registry) List(ctx context.Context) ([]server.ResourceDef, error) {
	ac := auth.FromContext(ctx)
	wss, err := r.client.ListWorkspaces(ctx, toMoraAuth(ac))
	if err != nil {
		return nil, err
	}
	defs := make([]server.ResourceDef, 0, 1+2*len(wss))
	defs = append(defs, server.ResourceDef{URI: "wiki://workspaces", Name: "可见工作区", MimeType: mimeTypeJSON})
	for _, ws := range wss {
		defs = append(defs, server.ResourceDef{
			URI: "wiki://workspaces/" + ws.ID + "/tree", Name: ws.Name + "-目录树", MimeType: mimeTypeJSON,
		})
		defs = append(defs, server.ResourceDef{
			URI: "wiki://workspaces/" + ws.ID + "/tags", Name: ws.Name + "-标签", MimeType: mimeTypeJSON,
		})
	}
	return defs, nil
}

// Read resolves a wiki:// URI to its JSON content (design doc 06 §4.2).
func (r *Registry) Read(ctx context.Context, uri string) (*server.ResourceReadResult, error) {
	ac := auth.FromContext(ctx)
	parts, err := parseURI(uri)
	if err != nil {
		return nil, domainerr.ErrNotFound
	}
	var payload any
	switch parts.kind {
	case "workspaces":
		payload, err = r.client.ListWorkspaces(ctx, toMoraAuth(ac))
	case "tree":
		payload, err = r.client.GetDirectoryTree(ctx, toMoraAuth(ac), parts.id)
	case "tags":
		payload, err = r.client.GetTags(ctx, toMoraAuth(ac), parts.id)
	case "meta":
		payload, err = r.client.GetDocumentMeta(ctx, toMoraAuth(ac), parts.id)
	case "versions":
		payload, err = r.client.GetDocumentVersions(ctx, toMoraAuth(ac), parts.id)
	default:
		return nil, domainerr.ErrNotFound
	}
	if err != nil {
		// Not-found / forbidden both collapse to not-found at this layer; the
		// server then returns empty contents (existence-leak prevention).
		if domainerr.Is(err, domainerr.ErrNotFound) || domainerr.Is(err, domainerr.ErrForbidden) {
			return nil, domainerr.ErrNotFound
		}
		return nil, err
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &server.ResourceReadResult{
		Contents: []server.ResourceContent{{URI: uri, MimeType: mimeTypeJSON, Text: string(text)}},
	}, nil
}

// uriParts is a parsed wiki:// URI.
type uriParts struct {
	kind string // workspaces / tree / tags / meta / versions
	id   string // workspace or document id
}

// parseURI parses a wiki:// URI into its kind and id.
//   - wiki://workspaces                          → {kind: workspaces}
//   - wiki://workspaces/{id}/tree                → {kind: tree, id}
//   - wiki://workspaces/{id}/tags                → {kind: tags, id}
//   - wiki://documents/{id}/meta                 → {kind: meta, id}
//   - wiki://documents/{id}/versions             → {kind: versions, id}
func parseURI(uri string) (uriParts, error) {
	const prefix = "wiki://"
	if !strings.HasPrefix(uri, prefix) {
		return uriParts{}, domainerr.ErrNotFound
	}
	rest := strings.TrimPrefix(uri, prefix)
	rest = strings.TrimSuffix(rest, "/")
	segs := strings.Split(rest, "/")
	if len(segs) == 0 || segs[0] == "" {
		return uriParts{}, domainerr.ErrNotFound
	}
	switch segs[0] {
	case "workspaces":
		if len(segs) == 1 {
			return uriParts{kind: "workspaces"}, nil
		}
		if len(segs) == 3 && (segs[2] == "tree" || segs[2] == "tags") {
			return uriParts{kind: segs[2], id: segs[1]}, nil
		}
		return uriParts{}, domainerr.ErrNotFound
	case "documents":
		if len(segs) == 3 && (segs[2] == "meta" || segs[2] == "versions") {
			return uriParts{kind: segs[2], id: segs[1]}, nil
		}
		return uriParts{}, domainerr.ErrNotFound
	}
	return uriParts{}, domainerr.ErrNotFound
}

// toMoraAuth converts the MCP AuthContext into the moraclient.AuthContext.
func toMoraAuth(ac *auth.AuthContext) *moraclient.AuthContext {
	if ac == nil {
		return nil
	}
	return &moraclient.AuthContext{
		TokenID:      ac.TokenID,
		IdentityType: ac.IdentityType,
		IdentityID:   ac.IdentityID,
		IdentityName: ac.IdentityName,
		Scope:        ac.Scope,
		Groups:       ac.Groups,
		IsAdmin:      ac.IsAdmin,
	}
}
