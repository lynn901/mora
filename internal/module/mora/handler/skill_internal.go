package handler

// skill_internal.go is the §6.2 internal API (MCP → Mora) delivery handler.
// It is the surface the MCP Server's skill_list/skill_read/skill_resources /
// skill_propose tool layer calls (via moraclient) to list an agent's visible
// skills, obtain a Skill's SKILL.md header + resource manifest +
// compatibility_report, progressively read individual resource files — all
// trimmed by the agent's binding delivery_mode — and to submit a candidate
// proposal (never published directly).
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
	"github.com/lynn901/mora/internal/domain"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	skill "github.com/lynn901/mora/internal/module/skill"
)

// SkillInternalHandler exposes the §6.2 internal delivery endpoints.
type SkillInternalHandler struct {
	svc      *skill.DeliveryService
	proposals skill.ProposalSink
}

// NewSkillInternalHandler wires the internal skill delivery handler. proposals
// may be nil when the skill_propose path is not wired (the handler then rejects
// proposals with 503).
func NewSkillInternalHandler(svc *skill.DeliveryService, proposals skill.ProposalSink) *SkillInternalHandler {
	return &SkillInternalHandler{svc: svc, proposals: proposals}
}

// List handles GET /internal/v1/skills.
//
// It enumerates the skills an agent is bound to in the delegated workspace
// (skill_list backing, §6.3). A nil agent context (service_token-only) yields
// an EMPTY list — not an error — so existence never leaks (§8.2): an unbound
// agent and a workspace with no skills are indistinguishable.
func (h *SkillInternalHandler) List(c *gin.Context) {
	auth := MustAuth(c)
	agentID := auth.AgentID
	wsID := auth.WorkspaceID
	if agentID == uuid.Nil || wsID == uuid.Nil {
		// §11.2: no agent context → empty list (no leak).
		response.OK(c, map[string]any{"items": []any{}, "total": 0})
		return
	}
	items, err := h.svc.List(c.Request.Context(), agentID, wsID)
	if err != nil {
		// A listing fault must not leak — return an empty result (§8.2 no-leak).
		response.OK(c, map[string]any{"items": []any{}, "total": 0})
		return
	}
	if items == nil {
		items = []skill.SkillListItem{}
	}
	response.OK(c, map[string]any{"items": items, "total": len(items)})
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
// service's). A proposal write-denial surfaces as 404 too (no leak — the
// caller cannot tell a write-denied workspace from a missing one); a
// structurally invalid proposal is a 400.
func mapSkillInternalErr(err error) error {
	switch {
	case pkgerr.Is(err, skill.ErrPackageNotFound), pkgerr.Is(err, skill.ErrProposalRejected):
		return pkgerr.NotFound("not found")
	case pkgerr.Is(err, skill.ErrInvalidPackage), pkgerr.Is(err, skill.ErrInvalidProposal):
		return pkgerr.BadRequest("invalid package")
	}
	return err
}

// proposeReq is the §6.3 skill_propose body. Name + DraftBody are required;
// Description / Version / SourceRef are optional metadata for the human
// reviewer.
type proposeReq struct {
	Name        string         `json:"name" binding:"required"`
	Description string         `json:"description"`
	Version     string         `json:"version"`
	DraftBody   string         `json:"draft_body" binding:"required"`
	SourceRef   map[string]any `json:"source_ref"`
}

// Propose handles POST /internal/v1/skills/propose.
//
// It submits a candidate skill proposal (§6.3 skill_propose): the agent drafts
// a SKILL.md body, the handler wraps it into a minimal tar.gz archive and
// submits it via the ProposalSink, which creates a candidate asset + version
// (governance_status='candidate') + a pending review_request — NEVER
// publishing or binding the skill. The response carries the candidate +
// review references so the agent can track the proposal through the governance
// review flow.
//
// Write gate: the agent's delegated context must carry ActionWrite on the
// workspace (§5.1). A read-only / no-context caller is refused as 404 (no
// leak — §8.2). No script execution occurs (§4.4): the draft bytes are stored
// verbatim, never materialized with an exec bit.
func (h *SkillInternalHandler) Propose(c *gin.Context) {
	auth := MustAuth(c)
	agentID := auth.AgentID
	wsID := auth.WorkspaceID
	if agentID == uuid.Nil || wsID == uuid.Nil {
		response.Fail(c, mapSkillInternalErr(skill.ErrProposalRejected))
		return
	}
	// Write gate: the delegated actions must include ActionWrite. A read-only
	// delegated context is refused as not-found (no leak — §8.2).
	if !hasAction(auth, domain.ActionWrite) {
		response.Fail(c, mapSkillInternalErr(skill.ErrProposalRejected))
		return
	}
	if h.proposals == nil {
		// The propose sink is not wired (no object storage). Surface as 503 so
		// the operator can wire it; this is a configuration gap, not a leak.
		response.Fail(c, pkgerr.New(pkgerr.CodeInternal, http.StatusServiceUnavailable, "skill proposal unavailable"))
		return
	}
	var req proposeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, mapSkillInternalErr(skill.ErrInvalidProposal))
		return
	}
	// Build a minimal tar.gz archive from the draft SKILL.md body. The bytes
	// are stored verbatim by the sink — no exec bit is synthesized (§4.4).
	archive, err := skill.BuildDraftArchive(req.Name, req.DraftBody)
	if err != nil {
		response.Fail(c, mapSkillInternalErr(skill.ErrInvalidProposal))
		return
	}
	res, err := h.proposals.Submit(c.Request.Context(), skill.ProposalInput{
		WorkspaceID:  wsID,
		Name:         req.Name,
		Description:  req.Description,
		Version:      req.Version,
		DraftArchive: archive,
		SourceRef:    req.SourceRef,
		SubmittedBy:  domain.EventActor{Type: principalSubjectType(auth), ID: principalID(auth)},
	})
	if err != nil {
		response.Fail(c, mapSkillInternalErr(err))
		return
	}
	response.Created(c, map[string]any{
		"asset_id":          res.AssetID,
		"asset_version_id":  res.AssetVersionID,
		"review_request_id": res.ReviewRequestID,
		"status":            "candidate",
	})
}

// hasAction reports whether the delegated AuthState carries the action.
func hasAction(s AuthState, action domain.Action) bool {
	for _, a := range s.Actions {
		if a == string(action) {
			return true
		}
	}
	return false
}
