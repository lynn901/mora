// handler.go — REST/internal handler 签名（§11.1 / §11.2）。
//
// REST control plane (§11.1):
//
//	POST /api/v1/workspaces/{ws}/knowledge/search      # direct typed search
//	POST /api/v1/workspaces/{ws}/knowledge/context      # Context Broker
//	GET  /api/v1/workspaces/{ws}/knowledge/policies     # list authority policies
//	PUT  /api/v1/workspaces/{ws}/knowledge/policies/{intent}  # update (PM)
//
// Internal API (§11.2): POST /internal/v1/knowledge/search + /knowledge/context
// (MCP Server + Context Proxy callers). Internal requests use service identity
// + a Mora-issued short-lived delegated context; INTERNAL_SERVICE_TOKEN alone
// does NOT stand for the end-user authority (§11.2).
//
// This file lands the handler struct + method signatures as TODO stubs. The
// real HTTP wiring (Gin route registration, request binding, AuthState →
// AuthContext mapping, response shaping) lands in a follow-up sub-task.

package contextbroker

import (
	"net/http"
)

// Handler is the REST + internal HTTP surface for the Context Broker (§11.1 /
// §11.2). It wraps a ContextBroker; the four methods map 1:1 to the routes.
// Methods are TODO stubs — signatures fixed so the route registration compiles.
type Handler struct {
	broker ContextBroker
}

// NewHandler wires the handler to a ContextBroker.
func NewHandler(b ContextBroker) *Handler { return &Handler{broker: b} }

// Search handles POST /api/v1/workspaces/{ws}/knowledge/search — the direct
// typed search that does NOT assemble context (§11.1). TODO: bind request,
// map AuthState → AuthContext, call the document port, shape response.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
	// TODO: §11.1 direct typed search.
}

// Context handles POST /api/v1/workspaces/{ws}/knowledge/context — the
// Context Broker route (intent routing + budget + citation, §11.1). TODO: bind
// KnowledgeQuery, call broker.Execute, shape the §11.3 response (candidates /
// degraded_sources / truncation / intent / policy_version / authz_revision /
// decision_id).
func (h *Handler) Context(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
	// TODO: §11.1 / §11.3 Context Broker response.
}

// ListPolicies handles GET /api/v1/workspaces/{ws}/knowledge/policies (§11.1).
// TODO: list context_authority_policies (is_current=true) for the workspace.
func (h *Handler) ListPolicies(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
	// TODO: §11.1 list policies.
}

// PutPolicy handles PUT /api/v1/workspaces/{ws}/knowledge/policies/{intent}
// (§11.1, PM-governed). TODO: supersede the current version (new policy_version,
// set is_current, supersede_at the old row) + invalidate the cache key.
func (h *Handler) PutPolicy(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
	// TODO: §11.1 supersede + invalidate cache (§5.3).
}
