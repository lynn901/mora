package handler

// codegraph.go is the HTTP adapter for the codebase codegraph query API
// (design-docs/17 §6.1). It mirrors asset.go: the handler only binds HTTP ↔
// service inputs and maps service errors to the §11.4 envelope. Business logic
// + resource-level RBAC stay in the codegraph service / asset read service.
//
// Existence never leaks (§8.2): every read path maps
// codegraph.ErrCodebaseNotFound to the same 404 + 40400 the response package
// emits for any not-found resource, so a caller cannot tell a missing codebase
// from a cross-workspace / no-permission one (§10.4 用例 26/27). A provider
// fault (capability_unavailable / source_snapshot_unavailable) surfaces
// distinctly so it is never confused with authorized-empty / genuine no-results
// (§15).

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	stderrors "errors"
)

// CodeGraphHandler exposes the codebase codegraph query REST endpoints (§6.1).
type CodeGraphHandler struct {
	svc *cgservice.Service
}

// NewCodeGraphHandler wires the codegraph query handler over the codegraph
// query service.
func NewCodeGraphHandler(svc *cgservice.Service) *CodeGraphHandler {
	return &CodeGraphHandler{svc: svc}
}

// parseAssetID binds the :id path param as the codebase asset id. Centralized so
// every codegraph method maps a malformed id to the same 400 + 40000.
func parseAssetID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return uuid.Nil, false
	}
	return id, true
}

// mapCodeGraphErr maps a codegraph-service error to the §11.4 envelope. A
// not-found (missing / cross-workspace / no-permission codebase) → NotFound
// (404 + 40400), indistinguishable across the three cases so existence never
// leaks (§8.2 / §10.4 用例 26/27). Provider capability faults surface with a
// distinct code so they are never confused with authorized-empty (§15).
func mapCodeGraphErr(err error) error {
	switch {
	case stderrors.Is(err, cgservice.ErrCodebaseNotFound), stderrors.Is(err, asset.ErrAssetNotFound):
		return pkgerr.NotFound("not found")
	case stderrors.Is(err, cgservice.ErrGraphNotReady):
		// No ready projection yet → 409 conflict (build pending / failed), not a
		// 404: the codebase resolves (RBAC passed) so its existence is already
		// known to the caller; surfacing "not ready" leaks nothing (§8.2).
		return pkgerr.Conflict("codegraph not ready")
	case stderrors.Is(err, cgprovider.ErrCapabilityUnavailable):
		// Sidecar down / unconfigured → 503 service unavailable (§15 row 3).
		// Distinct from authorized-empty + genuine no-results.
		return pkgerr.ServiceUnavailable("codegraph capability unavailable")
	case stderrors.Is(err, cgprovider.ErrSourceSnapshotUnavailable), stderrors.Is(err, cgprovider.ErrAssetVersionMismatch):
		// §4.2 fail-closed: source tree / version misaligned → 410 gone, never
		// return possibly-misaligned source (§15 row 2).
		return pkgerr.Gone("codegraph source snapshot unavailable")
	default:
		return err
	}
}

// --- GET /knowledge/assets/:id/codegraph/status ---

func (h *CodeGraphHandler) Status(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	st, err := h.svc.Status(c.Request.Context(), assetAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, st)
}

// --- GET /knowledge/assets/:id/codegraph/files ---

func (h *CodeGraphHandler) Files(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	req := cgprovider.FilesRequest{PathPrefix: c.Query("path_prefix")}
	tree, err := h.svc.Files(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, tree)
}

// --- GET /knowledge/assets/:id/codegraph/search ---

func (h *CodeGraphHandler) Search(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	req := cgprovider.CodeSearchRequest{
		Query:    c.Query("q"),
		Language: c.Query("language"),
		PathGlob: c.Query("path_glob"),
		Limit:    limit,
	}
	if req.Query == "" {
		response.Fail(c, badRequestErr("missing q"))
		return
	}
	hits, err := h.svc.Search(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, gin.H{"items": hits})
}

// --- GET /knowledge/assets/:id/codegraph/explore ---

func (h *CodeGraphHandler) Explore(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	req := cgprovider.ExploreRequest{
		Query:    c.Query("q"),
		Language: c.Query("language"),
		Limit:    limit,
	}
	if req.Query == "" {
		response.Fail(c, badRequestErr("missing q"))
		return
	}
	res, err := h.svc.Explore(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, res)
}

// --- GET /knowledge/assets/:id/codegraph/node ---

func (h *CodeGraphHandler) Node(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	req := cgprovider.NodeRequest{
		Symbol:   c.Query("symbol"),
		Language: c.Query("language"),
		Path:     c.Query("path"),
	}
	if req.Symbol == "" {
		response.Fail(c, badRequestErr("missing symbol"))
		return
	}
	node, err := h.svc.Node(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, node)
}

// --- GET /knowledge/assets/:id/codegraph/callers ---

func (h *CodeGraphHandler) Callers(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	req := cgprovider.NodeRequest{
		Symbol:   c.Query("symbol"),
		Language: c.Query("language"),
		Path:     c.Query("path"),
	}
	if req.Symbol == "" {
		response.Fail(c, badRequestErr("missing symbol"))
		return
	}
	edges, err := h.svc.Callers(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, gin.H{"items": edges})
}

// --- GET /knowledge/assets/:id/codegraph/callees ---

func (h *CodeGraphHandler) Callees(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	req := cgprovider.NodeRequest{
		Symbol:   c.Query("symbol"),
		Language: c.Query("language"),
		Path:     c.Query("path"),
	}
	if req.Symbol == "" {
		response.Fail(c, badRequestErr("missing symbol"))
		return
	}
	edges, err := h.svc.Callees(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, gin.H{"items": edges})
}

// --- GET /knowledge/assets/:id/codegraph/impact ---

func (h *CodeGraphHandler) Impact(c *gin.Context) {
	id, ok := parseAssetID(c)
	if !ok {
		return
	}
	depth, _ := strconv.Atoi(c.Query("depth"))
	req := cgprovider.ImpactRequest{
		Symbol:   c.Query("symbol"),
		Language: c.Query("language"),
		Path:     c.Query("path"),
		Depth:    depth,
	}
	if req.Symbol == "" {
		response.Fail(c, badRequestErr("missing symbol"))
		return
	}
	hits, err := h.svc.Impact(c.Request.Context(), assetAuth(MustAuth(c)), id, req)
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, gin.H{"items": hits})
}

// --- GET /knowledge/codegraph/capabilities ---

// Capabilities exposes the provider's advertised surface (§6.2). Not scoped to
// a codebase — no RBAC needed; it only says what the provider can do.
func (h *CodeGraphHandler) Capabilities(c *gin.Context) {
	caps, err := h.svc.Capabilities(c.Request.Context())
	if err != nil {
		response.Fail(c, mapCodeGraphErr(err))
		return
	}
	response.OK(c, caps)
}
