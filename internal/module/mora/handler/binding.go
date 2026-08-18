package handler

// binding.go is the HTTP adapter for the Agent 配装（Binding）management REST
// control plane (design-docs/19 §6.1 — Phase 5-3, YS-163). It mirrors
// source.go: cursor-paginated lists, Idempotency-Key on the batch, If-Match
// ETag on PATCH, and a leak-safe error mapping (binding.ErrBindingNotFound →
// 404, indistinguishable from a permission denial so existence never leaks,
// §8.2 / §1.2). Business logic + RBAC + the transactional sink stay in the
// binding service (internal/module/binding); this file only binds HTTP ↔
// service inputs.
//
// Management operations (list / batch / patch / revoke) are gated on the
// `assign` action on the workspace (§6.1 管理型) and do NOT enter the default
// Agent MCP tool set (§6.3).

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
	binding "github.com/lynn901/mora/internal/module/binding"
)

// BindingHandler exposes the Agent 配装 management REST endpoints (§6.1).
type BindingHandler struct {
	svc *binding.Service
}

// NewBindingHandler wires the binding management handler over the binding service.
func NewBindingHandler(svc *binding.Service) *BindingHandler {
	return &BindingHandler{svc: svc}
}

// --- GET /agents/:id/bindings (列表，cursor 分页) ---

func (h *BindingHandler) List(c *gin.Context) {
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid agent id"))
		return
	}
	// The workspace_id comes from the query (the caller names the workspace
	// whose binding set it wants; a cross-workspace caller is denied as
	// not-found by the service's `assign` gate — no existence leak).
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	var after *uuid.UUID
	if cur := c.Query("cursor"); cur != "" {
		if id, err := uuid.Parse(cur); err == nil {
			after = &id
		}
	}
	limit, _ := strconv.Atoi(c.Query("page_size"))
	items, err := h.svc.ListBindings(c.Request.Context(), bindingAuth(MustAuth(c)), agentID, wsID, after, limit)
	if err != nil {
		response.Fail(c, mapBindingErr(err))
		return
	}
	// Cursor = the last item's id (uuid-keyed, monotonic per agent — see
	// BindingRepo.List's id > after cursor). nil/empty → no next page.
	var next string
	if n := len(items); n > 0 && (limit == 0 || n >= limit) {
		next = items[n-1].ID.String()
	}
	c.Header("X-Next-Cursor", next)
	response.OK(c, gin.H{"items": items, "next_cursor": next})
}

// --- POST /agents/:id/bindings:batch (批量配装，Idempotency-Key) ---

type batchBindingItem struct {
	ID              *uuid.UUID `json:"id,omitempty"`
	ETag            int64      `json:"etag,omitempty"`
	ScopeKind       string     `json:"scope_kind" binding:"required"`
	AssetID         *uuid.UUID `json:"asset_id,omitempty"`
	AssetType       *string    `json:"asset_type,omitempty"`
	Effect          string     `json:"effect" binding:"required"`
	VersionPolicy   string     `json:"version_policy" binding:"required"`
	PinnedVersionID *uuid.UUID `json:"pinned_version_id,omitempty"`
	DeliveryMode    string     `json:"delivery_mode" binding:"required"`
	Priority        int        `json:"priority"`
}

type batchBindingReq struct {
	WorkspaceID uuid.UUID         `json:"workspace_id" binding:"required"`
	Items       []batchBindingItem `json:"items" binding:"required"`
}

func (h *BindingHandler) Batch(c *gin.Context) {
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid agent id"))
		return
	}
	var req batchBindingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	inputs := make([]binding.BindingInput, 0, len(req.Items))
	for _, it := range req.Items {
		in := binding.BindingInput{
			ID:            it.ID,
			ETag:          it.ETag,
			ScopeKind:     domain.BindingScopeKind(it.ScopeKind),
			AssetID:       it.AssetID,
			Effect:        domain.BindingEffect(it.Effect),
			VersionPolicy: domain.BindingVersionPolicy(it.VersionPolicy),
			PinnedVersionID: it.PinnedVersionID,
			DeliveryMode:  domain.BindingDeliveryMode(it.DeliveryMode),
			Priority:      it.Priority,
		}
		if it.AssetType != nil {
			at := domain.AssetType(*it.AssetType)
			in.AssetType = &at
		}
		inputs = append(inputs, in)
	}
	res, err := h.svc.BatchUpsertBindings(c.Request.Context(), bindingAuth(MustAuth(c)), agentID, req.WorkspaceID,
		c.GetHeader("Idempotency-Key"), inputs)
	if err != nil {
		response.Fail(c, mapBindingErr(err))
		return
	}
	response.OK(c, gin.H{
		"results":         res.Results,
		"alerted":         res.Alerted,
		"new_revision":    res.NewRevision,
		"idempotent_hit":  res.IdempotentHit,
	})
}

// --- PATCH /agents/:id/bindings/:binding_id (更新 delivery_mode/priority) ---

type patchBindingReq struct {
	DeliveryMode *string `json:"delivery_mode"`
	Priority     *int    `json:"priority"`
	// ETag (If-Match) is required: the update path is revoke-old + create-new,
	// gated by the old binding's ETag (created_at epoch-ms). The caller MUST
	// have read the binding first to know its ETag.
}

func (h *BindingHandler) Patch(c *gin.Context) {
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid agent id"))
		return
	}
	bindingID, err := uuid.Parse(c.Param("binding_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid binding_id"))
		return
	}
	// The workspace_id is required to scope the `assign` gate + the
	// cross-workspace guard. The caller passes it as a query param (the binding
	// itself carries workspace_id, but the caller must name the workspace it
	// believes the binding lives in — a mismatch is denied as not-found).
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	etag, _ := strconv.ParseInt(c.GetHeader("If-Match"), 10, 64)
	if etag == 0 {
		response.Fail(c, badRequestErr("If-Match ETag required"))
		return
	}
	var req patchBindingReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, badRequestErr("invalid body"))
		return
	}
	// Load the current binding to carry forward the fields the PATCH leaves
	// untouched (scope/effect/version_policy/etc.). The update path is
	// revoke-old + create-new, so the new binding must repeat the full shape.
	// GetBinding enforces the `assign` gate + the cross-workspace/agent guard
	// (a mismatch → ErrBindingNotFound, no leak).
	cur, err := h.svc.GetBinding(c.Request.Context(), bindingAuth(MustAuth(c)), bindingID, agentID, wsID)
	if err != nil {
		response.Fail(c, mapBindingErr(err))
		return
	}
	dm := string(cur.DeliveryMode)
	if req.DeliveryMode != nil {
		dm = *req.DeliveryMode
	}
	prio := cur.Priority
	if req.Priority != nil {
		prio = *req.Priority
	}
	in := binding.BindingInput{
		ID:              &bindingID,
		ETag:            etag,
		ScopeKind:       cur.ScopeKind,
		AssetID:         cur.AssetID,
		AssetType:       cur.AssetType,
		Effect:          cur.Effect,
		VersionPolicy:   cur.VersionPolicy,
		PinnedVersionID: cur.PinnedVersionID,
		DeliveryMode:    domain.BindingDeliveryMode(dm),
		Priority:        prio,
	}
	res, err := h.svc.BatchUpsertBindings(c.Request.Context(), bindingAuth(MustAuth(c)), agentID, wsID,
		c.GetHeader("Idempotency-Key"), []binding.BindingInput{in})
	if err != nil {
		response.Fail(c, mapBindingErr(err))
		return
	}
	if len(res.Results) == 0 {
		response.Fail(c, mapBindingErr(binding.ErrBindingNotFound))
		return
	}
	c.Header("ETag", strconv.FormatInt(res.Results[0].Binding.CreatedAt.UnixMilli(), 10))
	response.OK(c, res.Results[0].Binding)
}

func (h *BindingHandler) Revoke(c *gin.Context) {
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid agent id"))
		return
	}
	bindingID, err := uuid.Parse(c.Param("binding_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid binding_id"))
		return
	}
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	newRev, err := h.svc.RevokeBinding(c.Request.Context(), bindingAuth(MustAuth(c)), bindingID, agentID, wsID)
	if err != nil {
		response.Fail(c, mapBindingErr(err))
		return
	}
	response.OK(c, gin.H{"binding_id": bindingID, "revoked": true, "new_revision": newRev})
}

// --- helpers ---

// mapBindingErr maps a binding-service error to the §11.4 envelope. The key
// invariant (§8.2): a missing/cross-workspace/unreadable binding returns
// NotFound (404 + 40400) — the SAME shape a permission denial takes — so
// existence never leaks. An ETag mismatch / idempotency-key conflict returns
// Conflict (409 + 40900). An invalid binding input returns BadRequest (400).
func mapBindingErr(err error) error {
	switch {
	case pkgerr.Is(err, binding.ErrBindingNotFound):
		return pkgerr.NotFound("not found")
	case pkgerr.Is(err, binding.ErrBindingConflict),
		pkgerr.Is(err, binding.ErrIdempotencyConflict):
		return pkgerr.Conflict("conflict")
	case pkgerr.Is(err, binding.ErrInvalidBinding):
		return pkgerr.BadRequest("invalid binding")
	}
	return err
}

// bindingAuth maps the HTTP AuthState to the binding service's AuthContext
// (mirrors the source handler's srcAuth helper).
func bindingAuth(s AuthState) binding.AuthContext {
	return binding.AuthContext{
		SubjectType: principalSubjectType(s),
		PrincipalID: principalID(s),
		GroupIDs:    s.Groups,
		IsAdmin:     s.IsAdmin,
	}
}
