package handler

// source.go is the HTTP adapter for the Source management REST API
// (design-docs/14 §4.4 D13). It lives in the mora/handler package alongside
// the other Gin handlers so it reuses the shared AuthMiddleware + AuthState +
// response/error conventions. The business logic stays in the source service
// (internal/module/knowledge/source/service); this file only binds HTTP ↔
// service inputs and maps service errors to the §11.4 envelope.
//
// Existence never leaks (§8.2): every read path maps source.ErrSourceNotFound
// / ErrRunNotFound / ErrReviewNotFound to the same 404 + 40400 the response
// package emits for a permission denial, so a caller can't tell not-found
// from not-allowed.

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	srcsvc "github.com/lynn901/mora/internal/module/knowledge/source/service"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	"github.com/lynn901/mora/internal/platform/egress"
)

// SourceHandler exposes the Source management REST endpoints (§4.4).
type SourceHandler struct {
	svc *srcsvc.Service
}

// NewSourceHandler wires the Source management handler over the source service.
func NewSourceHandler(svc *srcsvc.Service) *SourceHandler {
	return &SourceHandler{svc: svc}
}

// --- GET /workspaces/:workspace_id/knowledge/sources ---

func (h *SourceHandler) List(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	q := srcsvc.SourceListQuery{
		WorkspaceID: wsID,
		Cursor:      c.Query("cursor"),
		PageSize:    pageSize,
		SourceType: c.Query("source_type"),
	}
	if e := c.Query("enabled"); e != "" {
		b := strings.EqualFold(e, "true") || e == "1"
		q.Enabled = &b
	}
	items, next, err := h.svc.ListSources(c.Request.Context(), srcAuth(MustAuth(c)), q)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	c.Header("X-Next-Cursor", next)
	response.OK(c, gin.H{"items": items, "next_cursor": next})
}

// --- POST /workspaces/:workspace_id/knowledge/sources ---

type createSourceReq struct {
	SourceType         domain.SourceType    `json:"source_type" binding:"required"`
	Name               string               `json:"name" binding:"required"`
	URI                string               `json:"uri" binding:"required"` // raw; credentials stripped below
	SyncPolicy         map[string]any       `json:"sync_policy"`
	TrustLevel         domain.TrustLevel    `json:"trust_level"`
	License            map[string]any       `json:"license"`
	CredentialRef      string               `json:"credential_ref"`
	RequestedAssetType domain.RequestedAssetType `json:"requested_asset_type"`
}

func (h *SourceHandler) Create(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var req createSourceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := MustAuth(c)
	in := srcsvc.CreateSourceInput{
		WorkspaceID:   wsID,
		SourceType:    req.SourceType,
		Name:          req.Name,
		URINormalized: stripURICredentials(req.URI), // §6.5: never store embedded creds
		CredentialRef: req.CredentialRef,
		SyncPolicy:    req.SyncPolicy,
		TrustLevel:    req.TrustLevel,
		License:       req.License,
		CreatedByType: principalSubjectType(auth),
		CreatedByID:   principalID(auth),
	}
	src, err := h.svc.CreateSource(c.Request.Context(), srcAuth(auth), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.Created(c, src)
}

// --- GET /knowledge/sources/:id ---

func (h *SourceHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	src, err := h.svc.GetSource(c.Request.Context(), srcAuth(MustAuth(c)), id)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	response.OK(c, src)
}

// --- PATCH /knowledge/sources/:id ---

type updateSourceReq struct {
	Name        *string              `json:"name"`
	SyncPolicy  map[string]any       `json:"sync_policy"`
	TrustLevel  *domain.TrustLevel   `json:"trust_level"`
	License     map[string]any       `json:"license"`
	Enabled     *bool                `json:"enabled"`
}

func (h *SourceHandler) Update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	etag, _ := strconv.ParseInt(c.GetHeader("If-Match"), 10, 64)
	var req updateSourceReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	patch := srcsvc.SourcePatch{
		Name:       req.Name,
		SyncPolicy: req.SyncPolicy,
		TrustLevel: req.TrustLevel,
		License:    req.License,
		Enabled:    req.Enabled,
	}
	src, err := h.svc.UpdateSource(c.Request.Context(), srcAuth(MustAuth(c)), id, etag, patch)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	c.Header("ETag", strconv.FormatInt(src.ETagVersion, 10))
	response.OK(c, src)
}

// --- DELETE /knowledge/sources/:id (soft-disable) ---

func (h *SourceHandler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	if err := h.svc.DisableSource(c.Request.Context(), srcAuth(MustAuth(c)), id); err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	response.NoContent(c)
}

// --- PUT /knowledge/sources/:id/credentials ---

type setCredentialReq struct {
	CredentialRef string `json:"credential_ref"` // when no CredentialStore is wired
	// PlaintextCredential carries the raw secret when a CredentialStore IS
	// wired; it is consumed in-memory and never stored/logged/echoed (§13.2).
	PlaintextCredential []byte `json:"plaintext_credential"`
}

func (h *SourceHandler) SetCredential(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	// The workspace_id comes from the source itself; the service re-resolves it
	// via the authz SourceLocator, so the handler does not trust a client path.
	var req setCredentialReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	// Caller must pass workspace_id in the path (cross-workspace guard).
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		// This endpoint is mounted under /workspaces/:workspace_id/knowledge/sources/:id/credentials
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	if err := h.svc.SetCredential(c.Request.Context(), srcAuth(MustAuth(c)), id, wsID, req.PlaintextCredential, req.CredentialRef); err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	response.NoContent(c)
}

// --- GET /knowledge/sources/:id/sync-runs ---

func (h *SourceHandler) ListRuns(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	q := srcsvc.SyncRunListQuery{
		SourceID: id,
		Cursor:   c.Query("cursor"),
		PageSize: pageSize,
		Status:   c.Query("status"),
	}
	items, next, err := h.svc.ListRuns(c.Request.Context(), srcAuth(MustAuth(c)), q)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	c.Header("X-Next-Cursor", next)
	response.OK(c, gin.H{"items": items, "next_cursor": next})
}

// --- POST /knowledge/sources/:id/sync-runs ---

type triggerSyncReq struct {
	RequestedRevision  string               `json:"requested_revision"`
	RequestedAssetType domain.RequestedAssetType `json:"requested_asset_type" binding:"required"`
	GovernanceProfileID *uuid.UUID          `json:"governance_profile_id"`
}

func (h *SourceHandler) TriggerSync(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var req triggerSyncReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := MustAuth(c)
	in := srcsvc.TriggerSyncInput{
		SourceID:            id,
		RequestedRevision:   req.RequestedRevision,
		RequestedAssetType:  req.RequestedAssetType,
		GovernanceProfileID: req.GovernanceProfileID,
		RequestedByType:     principalSubjectType(auth),
		RequestedByID:       principalID(auth),
		IdempotencyKey:      c.GetHeader("Idempotency-Key"),
	}
	run, err := h.svc.TriggerSync(c.Request.Context(), srcAuth(auth), in)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	response.Accepted(c, run)
}

// --- GET /workspaces/:workspace_id/knowledge/reviews ---

func (h *SourceHandler) ListReviews(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	items, next, err := h.svc.ListPendingReviews(c.Request.Context(), srcAuth(MustAuth(c)), wsID, c.Query("cursor"), pageSize)
	if err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	c.Header("X-Next-Cursor", next)
	response.OK(c, gin.H{"items": items, "next_cursor": next})
}

// --- POST /knowledge/reviews/:id/decisions ---

type reviewDecisionReq struct {
	Decision          domain.ReviewDecision `json:"decision" binding:"required"`
	PolicyVersion     string                    `json:"policy_version" binding:"required"`
	RationaleRedacted string                    `json:"rationale"`
}

func (h *SourceHandler) PostDecision(c *gin.Context) {
	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid id"))
		return
	}
	var req reviewDecisionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	auth := MustAuth(c)
	in := srcsvc.ReviewDecisionInput{
		ReviewRequestID:   reviewID,
		Decision:          req.Decision,
		DecisionByType:    principalSubjectType(auth),
		DecisionByID:     principalID(auth),
		PolicyVersion:    req.PolicyVersion,
		RationaleRedacted: req.RationaleRedacted,
	}
	if err := h.svc.AppendReviewDecision(c.Request.Context(), srcAuth(auth), in); err != nil {
		response.Fail(c, mapSourceErr(err))
		return
	}
	response.NoContent(c)
}

// --- helpers ---

// mapSourceErr maps a source-service error to the §11.4 envelope. The key
// invariant: a missing/unreadable resource returns NotFound (404 + 40400) —
// the SAME shape a READ permission denial takes — so existence never leaks
// (§8.2 / §10.4 用例 27). A WRITE/governance permission denial returns
// Forbidden (403 + 40300) per §10.4 用例 25/29 (the caller is authenticated
// and asked to mutate, so revealing "forbidden" does not leak existence).
// An ETag / idempotency conflict returns Conflict (409 + 40900).
func mapSourceErr(err error) error {
	switch {
	case errors.Is(err, srcsvc.ErrSourceForbidden):
		return errors.Forbidden("forbidden")
	case errors.Is(err, srcsvc.ErrSourceNotFound),
		errors.Is(err, srcsvc.ErrRunNotFound),
		errors.Is(err, srcsvc.ErrReviewNotFound):
		return errors.NotFound("not found")
	case errors.Is(err, srcsvc.ErrSourceConflict),
		errors.Is(err, srcsvc.ErrIdempotencyConflict):
		return errors.Conflict("conflict")
	}
	return err
}

// srcAuth maps the HTTP AuthState to the source service's AuthContext (mirrors
// the mora/service.svcAuth helper). The rbac subject is the acting user
// (principalID); a service-account caller resolves to its service account id
// with no admin bypass. GroupIDs is plumbed so group-inherited grants apply.
func srcAuth(s AuthState) srcsvc.AuthContext {
	return srcsvc.AuthContext{
		SubjectType:     principalSubjectType(s),
		PrincipalID:     principalID(s),
		GroupIDs:        s.Groups,
		IsAdmin:         s.IsAdmin,
		IsServiceCaller: s.IsServiceCaller,
	}
}

// principalSubjectType resolves the acting principal's SubjectType, defaulting
// to user when the middleware did not set one (a plain JWT caller is a user).
func principalSubjectType(s AuthState) domain.SubjectType {
	if s.SubjectType != "" {
		return s.SubjectType
	}
	return domain.SubjectUser
}

// principalID resolves the acting principal's ID. An agent acting on behalf of
// a user records the user as the actor (audit attributes the action to the
// human); a service-account caller records its service account id.
func principalID(s AuthState) uuid.UUID {
	if s.UserID != uuid.Nil {
		return s.UserID
	}
	return s.AgentID
}

// stripURICredentials removes embedded user:pass@ from a URI so the stored
// uri_normalized never carries plaintext (§6.5 / §2.2). A credential the
// caller wants to persist must go through PUT /credentials. If the URI is
// malformed the input is returned unchanged (best-effort); the egress layer
// rejects malformed URLs earlier at Validate, and the Connector never reads
// plaintext from the stored URI.
func stripURICredentials(raw string) string {
	return egress.RedactURL(raw)
}
