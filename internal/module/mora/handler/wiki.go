// Package handler — Wiki maintenance REST control plane (design-docs/16
// §7.1 / api/wiki.yaml). It lives in mora/handler alongside the other Gin
// handlers so it reuses the shared AuthMiddleware + AuthState + response/error
// conventions. Business logic stays in wiki/service; this file only binds
// HTTP ↔ service inputs and maps service errors to the §11.4 envelope.
//
// Existence never leaks (§8.2): every read path maps the wiki not-found
// sentinels to the same 404 + 40400 the response package emits for a
// permission denial, so a caller can't tell not-found from not-allowed.
package handler

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/pkg/response"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	wikisvc "github.com/lynn901/mora/internal/module/knowledge/wiki/service"
)

// WikiHandler exposes the Wiki maintenance REST endpoints (§7.1).
type WikiHandler struct {
	svc *wikisvc.Service
}

// NewWikiHandler wires the Wiki handler over the wiki service.
func NewWikiHandler(svc *wikisvc.Service) *WikiHandler {
	return &WikiHandler{svc: svc}
}

// --- GET /workspaces/:workspace_id/wiki-spaces ---

func (h *WikiHandler) ListSpaces(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	items, total, err := h.svc.ListSpaces(c.Request.Context(), wikiAuth(MustAuth(c)), wsID, page, pageSize)
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.Paged(c, items, total, page, pageSize)
}

// --- POST /workspaces/:workspace_id/wiki-spaces ---

type createWikiSpaceReq struct {
	Name                string         `json:"name" binding:"required"`
	SchemaAssetID       string         `json:"schema_asset_id" binding:"required"`
	SchemaVersionID     string         `json:"schema_version_id" binding:"required"`
	GovernanceProfileID string         `json:"governance_profile_id" binding:"required"`
	MaintenancePolicy   map[string]any `json:"maintenance_policy"`
}

func (h *WikiHandler) CreateSpace(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req createWikiSpaceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	schemaAsset, err := uuid.Parse(req.SchemaAssetID)
	if err != nil {
		response.Fail(c, badRequestErr("invalid schema_asset_id"))
		return
	}
	schemaVer, err := uuid.Parse(req.SchemaVersionID)
	if err != nil {
		response.Fail(c, badRequestErr("invalid schema_version_id"))
		return
	}
	gov, err := uuid.Parse(req.GovernanceProfileID)
	if err != nil {
		response.Fail(c, badRequestErr("invalid governance_profile_id"))
		return
	}
	auth := MustAuth(c)
	sp, err := h.svc.CreateSpace(c.Request.Context(), wikiAuth(auth), wikisvc.CreateWikiSpaceInput{
		WorkspaceID:         wsID,
		Name:                req.Name,
		SchemaAssetID:       schemaAsset,
		SchemaVersionID:     schemaVer,
		GovernanceProfileID: gov,
		MaintenancePolicy:   req.MaintenancePolicy,
		CreatedByType:       principalSubjectType(auth),
		CreatedByID:         principalID(auth),
	})
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.Created(c, sp)
}

// --- GET /wiki-spaces/:id ---

func (h *WikiHandler) GetSpace(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	sp, err := h.svc.GetSpace(c.Request.Context(), wikiAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.OK(c, sp)
}

// --- GET /wiki-spaces/:id/status ---

// GetStatus aggregates the Space directory, latest run, and pending proposals
// (§7.3). It is the single call the MCP wiki_status tool makes; RBAC gates all
// three reads at the service layer so a caller with no read gets the same 404
// as a missing Space (§8.2 — no existence leak).
func (h *WikiHandler) GetStatus(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	st, err := h.svc.Status(c.Request.Context(), wikiAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.OK(c, st)
}

// --- GET /wiki-spaces/:id/maintenance-runs ---

func (h *WikiHandler) ListRuns(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	status := c.Query("status")
	items, total, err := h.svc.ListRuns(c.Request.Context(), wikiAuth(MustAuth(c)), id, status, page, pageSize)
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.Paged(c, items, total, page, pageSize)
}

// --- POST /wiki-spaces/:id/maintenance-runs ---

type triggerRunReq struct {
	Trigger    string         `json:"trigger" binding:"required"`
	PageKey    string         `json:"page_key"`
	AnswerRef  map[string]any `json:"answer_ref"`
	CheckKinds []string       `json:"check_kinds"`
}

func (h *WikiHandler) TriggerRun(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var req triggerRunReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := MustAuth(c)
	run, err := h.svc.TriggerRun(c.Request.Context(), wikiAuth(auth), wikisvc.TriggerRunInput{
		WikiSpaceID:     id,
		Trigger:         wikisvc.TriggerType(req.Trigger),
		PageKey:         req.PageKey,
		AnswerRef:       req.AnswerRef,
		CheckKinds:      req.CheckKinds,
		RequestedByType: principalSubjectType(auth),
		RequestedByID:   principalID(auth),
	})
	if err != nil {
		// An idempotent retry returns the existing run — surface as 200 not error.
		if errors.Is(err, wikisvc.ErrWikiIdempotentRetry) {
			response.OK(c, run)
			return
		}
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.Created(c, run)
}

// --- POST /wiki-spaces/:id/lint ---

type lintReq struct {
	CheckKinds []string `json:"check_kinds"`
}

func (h *WikiHandler) Lint(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var req lintReq
	_ = c.ShouldBindJSON(&req) // body optional
	auth := MustAuth(c)
	run, err := h.svc.TriggerRun(c.Request.Context(), wikiAuth(auth), wikisvc.TriggerRunInput{
		WikiSpaceID:     id,
		Trigger:         wikisvc.TriggerLint,
		CheckKinds:      req.CheckKinds,
		RequestedByType: principalSubjectType(auth),
		RequestedByID:   principalID(auth),
	})
	if err != nil {
		if errors.Is(err, wikisvc.ErrWikiIdempotentRetry) {
			response.OK(c, run)
			return
		}
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.Accepted(c, run)
}

// --- GET /wiki-spaces/:id/pages/:page_key/proposals ---

func (h *WikiHandler) ListProposals(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	pageKey := c.Param("page_key")
	status := c.Query("status")
	items, err := h.svc.ListProposals(c.Request.Context(), wikiAuth(MustAuth(c)), id, pageKey, status)
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	response.OK(c, gin.H{"items": items})
}

// --- POST /wiki-spaces/:id/proposals/:proposal_id ---

type proposalDecisionReq struct {
	Decision  string `json:"decision" binding:"required"`
	Rationale string `json:"rationale"`
}

func (h *WikiHandler) ReviewProposal(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	proposalID, err := uuid.Parse(c.Param("proposal_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid proposal_id"))
		return
	}
	var req proposalDecisionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := MustAuth(c)
	p, err := h.svc.ReviewProposal(c.Request.Context(), wikisvc.ApplyProposalInput{
		ProposalID: proposalID,
		Decision:   req.Decision,
		Rationale:  req.Rationale,
		Auth:       wikiAuth(auth),
	})
	if err != nil {
		response.Fail(c, mapWikiErr(err))
		return
	}
	_ = id // path validated; the proposal carries its own space id
	response.OK(c, p)
}

// --- helpers ---

// mapWikiErr maps a wiki-service error to the §11.4 envelope (mirrors
// mapSourceErr). Existence never leaks: not-found AND read-denial both → 404.
func mapWikiErr(err error) error {
	switch {
	case errors.Is(err, wikisvc.ErrWikiForbidden),
		errors.Is(err, wikisvc.ErrWikiLockedPageCovered):
		return pkgerr.Forbidden("forbidden")
	case errors.Is(err, wikisvc.ErrWikiSpaceNotFound),
		errors.Is(err, wikisvc.ErrWikiPageNotFound),
		errors.Is(err, wikisvc.ErrWikiRunNotFound),
		errors.Is(err, wikisvc.ErrWikiProposalNotFound):
		return pkgerr.NotFound("not found")
	case errors.Is(err, wikisvc.ErrWikiConflict),
		errors.Is(err, wikisvc.ErrWikiSchemaViolation):
		return pkgerr.Conflict("conflict")
	}
	return err
}

// wikiAuth maps the HTTP AuthState to the wiki service's AuthContext (mirrors
// srcAuth). The rbac subject is the acting user; a service-account caller
// resolves to its service account id with no admin bypass.
func wikiAuth(s AuthState) wikisvc.AuthContext {
	return wikisvc.AuthContext{
		SubjectType:     principalSubjectType(s),
		PrincipalID:     principalID(s),
		GroupIDs:        s.Groups,
		IsAdmin:         s.IsAdmin,
		IsServiceCaller: s.IsServiceCaller,
	}
}
