package handler

// asset.go is the HTTP adapter for the asset read API (design-docs/14 §4.4
// D13). It mirrors source.go: the handler only binds HTTP ↔ service inputs and
// maps service errors to the §11.4 envelope. Business logic + RBAC stay in the
// asset read service (internal/module/knowledge/asset).
//
// Existence never leaks (§8.2): every read path maps asset.ErrAssetNotFound to
// the same 404 + 40400 the response package emits for any not-found resource,
// so a caller cannot tell a missing asset from a cross-workspace / no-permission
// one (§10.4 用例 26/27).

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
)

// AssetHandler exposes the asset read REST endpoints (§4.4).
type AssetHandler struct {
	svc *asset.ReadService
}

// NewAssetHandler wires the asset read handler over the asset read service.
func NewAssetHandler(svc *asset.ReadService) *AssetHandler {
	return &AssetHandler{svc: svc}
}

// --- GET /workspaces/:workspace_id/knowledge/assets ---

func (h *AssetHandler) List(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	q := asset.ListQuery{
		WorkspaceID: wsID,
		Cursor:      c.Query("cursor"),
		PageSize:    pageSize,
		AssetType:   c.Query("asset_type"),
		Status:      c.Query("status"),
	}
	items, next, err := h.svc.ListAssets(c.Request.Context(), assetAuth(MustAuth(c)), q)
	if err != nil {
		response.Fail(c, mapAssetErr(err))
		return
	}
	c.Header("X-Next-Cursor", next)
	response.OK(c, gin.H{"items": items, "next_cursor": next})
}

// --- GET /knowledge/assets/:id ---

func (h *AssetHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	a, err := h.svc.GetAsset(c.Request.Context(), assetAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapAssetErr(err))
		return
	}
	response.OK(c, a)
}

// --- GET /knowledge/assets/:id/versions ---

func (h *AssetHandler) ListVersions(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	items, err := h.svc.ListVersions(c.Request.Context(), assetAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapAssetErr(err))
		return
	}
	response.OK(c, gin.H{"items": items})
}

// --- GET /knowledge/assets/:id/relations ---

func (h *AssetHandler) ListRelations(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	items, err := h.svc.ListRelations(c.Request.Context(), assetAuth(MustAuth(c)), id, c.Query("relation_type"))
	if err != nil {
		response.Fail(c, mapAssetErr(err))
		return
	}
	response.OK(c, gin.H{"items": items})
}

// --- helpers ---

// mapAssetErr maps an asset-service error to the §11.4 envelope. A read
// not-found maps to NotFound (404 + 40400) — indistinguishable from a
// permission denial so existence never leaks (§8.2 / §10.4 用例 26/27).
func mapAssetErr(err error) error {
	if errors.Is(err, asset.ErrAssetNotFound) {
		return errors.NotFound("not found")
	}
	return err
}

// assetAuth maps the HTTP AuthState to the asset service's AuthContext (mirrors
// the source handler's srcAuth helper). The RBAC subject is the acting user
// (principalID); a service-account caller resolves to its service account id
// with no admin bypass. GroupIDs is plumbed so group-inherited grants apply.
func assetAuth(s AuthState) asset.AuthContext {
	return asset.AuthContext{
		SubjectType:     principalSubjectType(s),
		PrincipalID:     principalID(s),
		GroupIDs:        s.Groups,
		IsAdmin:         s.IsAdmin,
		IsServiceCaller: s.IsServiceCaller,
	}
}
