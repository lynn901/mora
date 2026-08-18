package handler

// skill_internal.go is the §6.2 internal API (MCP → Mora) delivery handler.
// It is the surface the MCP Server's skill_list/skill_read/skill_resources tool
// layer calls (via moraclient) to obtain a Skill's SKILL.md header + resource
// manifest + compatibility_report, and to progressively read individual
// resource files — all trimmed by the agent's binding delivery_mode.
//
// Auth model (§11.2): the route is under the same AuthMiddleware as the public
// API. The caller presents INTERNAL_SERVICE_TOKEN (service identity) + a
// delegated JWT carrying the acting AgentID + WorkspaceID. A service_token-
// only caller (no agent context) is refused by the delivery service — the
// token never authorizes skill delivery alone. The handler passes the resolved
// AgentID + WorkspaceID from AuthState down; it does NOT trust any client
// header for the agent identity.
//
// Existence never leaks (§8.2): every not-found / denied / cross-workspace /
// wrong-binding path maps to the same 404 + 40400, indistinguishable so a
// caller cannot tell a missing skill from one it is not bound to.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	skill "github.com/lynn901/mora/internal/module/skill"
)

// SkillInternalHandler exposes the §6.2 internal delivery endpoints.
type SkillInternalHandler struct {
	svc *skill.DeliveryService
}

// NewSkillInternalHandler wires the internal skill delivery handler.
func NewSkillInternalHandler(svc *skill.DeliveryService) *SkillInternalHandler {
	return &SkillInternalHandler{svc: svc}
}

// Deliver handles GET /internal/v1/skills/:id/versions/:version.
//
// The :id is the knowledge asset id; :version is "latest", a version id, or
// empty (treated as latest). The agent's effective binding resolves the
// delivery_mode that trims the response (tool/summary/inline). A caller whose
// agent has no allow binding gets 404 (no leak).
func (h *SkillInternalHandler) Deliver(c *gin.Context) {
	assetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid skill id"))
		return
	}
	versionSpec := c.Param("version")
	if versionSpec == "" {
		versionSpec = "latest"
	}
	auth := MustAuth(c)
	agentID := auth.AgentID
	wsID := auth.WorkspaceID
	// §11.2: the internal token alone never authorizes delivery — an agent
	// context (AgentID + WorkspaceID) is required. A service_token-only caller
	// is refused as not-found (no leak).
	if agentID == uuid.Nil || wsID == uuid.Nil {
		response.Fail(c, mapSkillInternalErr(skill.ErrPackageNotFound))
		return
	}
	res, err := h.svc.Deliver(c.Request.Context(), agentID, wsID, assetID, versionSpec)
	if err != nil {
		response.Fail(c, mapSkillInternalErr(err))
		return
	}
	// The content bytes are NOT inlined here — inline delivery fetches bytes
	// via the progressive ReadResource endpoint. The manifest lists paths +
	// hashes so the consumer knows what to fetch.
	response.OK(c, res)
}

// ReadResource handles GET /internal/v1/skills/:id/resources/*path.
//
// The :id is the knowledge asset id; the wildcard path is the manifest entry
// path. The agent's binding delivery_mode gates whether raw reads are
// permitted (inline/tool allow; summary does not). A caller with no allow
// binding, or a summary-mode binding, gets 404 (no leak).
func (h *SkillInternalHandler) ReadResource(c *gin.Context) {
	assetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid skill id"))
		return
	}
	// Gin wildcard param. Trim leading slashes so a request for "/resources/a/b"
	// (the route is .../resources/*path) yields "a/b". An empty path is a 400.
	resourcePath := strings.Trim(c.Param("path"), "/")
	if resourcePath == "" {
		response.Fail(c, badRequestErr("missing resource path"))
		return
	}
	versionSpec := c.Query("version")
	if versionSpec == "" {
		versionSpec = "latest"
	}
	auth := MustAuth(c)
	agentID := auth.AgentID
	wsID := auth.WorkspaceID
	if agentID == uuid.Nil || wsID == uuid.Nil {
		response.Fail(c, mapSkillInternalErr(skill.ErrPackageNotFound))
		return
	}
	rc, err := h.svc.ReadResource(c.Request.Context(), agentID, wsID, assetID, versionSpec, resourcePath)
	if err != nil {
		response.Fail(c, mapSkillInternalErr(err))
		return
	}
	// Stream the bytes with integrity headers so the consumer can verify the
	// sha256 against the manifest.
	c.Header("X-Content-Hash", rc.ContentHash)
	c.Header("X-Resource-Hash", rc.Hash)
	c.Header("X-Resource-Kind", rc.Kind)
	c.Data(http.StatusOK, "application/octet-stream", rc.Content)
}

// mapSkillInternalErr maps a delivery-service error to the §11.4 envelope.
// The §8.2 invariant: every failure (missing skill, no binding, denied,
// cross-workspace, summary-mode read refusal) surfaces as 404 + 40400 so
// existence never leaks. A structurally invalid request (missing path) is a
// 400 (the only non-404 path, and it is the handler's own validation, not the
// service's).
func mapSkillInternalErr(err error) error {
	switch {
	case pkgerr.Is(err, skill.ErrPackageNotFound):
		return pkgerr.NotFound("not found")
	case pkgerr.Is(err, skill.ErrInvalidPackage):
		return pkgerr.BadRequest("invalid package")
	}
	return err
}
