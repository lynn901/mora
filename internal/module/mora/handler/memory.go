package handler

// memory.go is the HTTP adapter for the Phase 4 memory write-entry API
// (design-docs/18 §7.1, §4.1, decision D9). It exposes the single capture
// endpoint POST /api/v1/memory/evidence that serves BOTH write entries:
//
//   - memory_remember   — an Agent explicitly submits a conclusion + minimal
//                          evidence reference (source_kind=tool_call/message).
//   - 会话导入 (session import) — a user/admin selects a session excerpt and
//                          submits it (source_kind=session/message).
//
// Both feed the same Capture pipeline (§4.1 redact → §4.2 encrypt/store →
// §6.3 outbox `evidence.captured`). The endpoint is NOT a transparent proxy
// (D9 excludes that from the first version): the caller supplies the
// already-trimmed minimal fragment (§8.6).
//
// The handler only binds HTTP ↔ service inputs and maps service errors to the
// §11.4 envelope. Business logic (redact / encrypt / outbox) + workspace-write
// RBAC (§4.4) live in the evidence service (internal/module/memory/evidence).
// A secret-bearing payload is rejected as 400 (§4.1, §9.1 fail closed); a
// missing/cross-workspace/denied write is 403 (§4.4) — the denial is auditable.

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
)

// MemoryHandler exposes the memory evidence capture REST endpoint (§7.1).
type MemoryHandler struct {
	svc   *evidence.Service
	sink  evidence.EvidenceSink
}

// NewMemoryHandler wires the memory handler over the evidence capture service
// and its transactional sink. The sink owns the row + outbox-event boundary
// (§6.3); the service stays repo-agnostic.
func NewMemoryHandler(svc *evidence.Service, sink evidence.EvidenceSink) *MemoryHandler {
	return &MemoryHandler{svc: svc, sink: sink}
}

// captureRequest is the JSON body for POST /api/v1/memory/evidence. The
// caller supplies the workspace + the non-executable source locator (D9) +
// the already-trimmed minimal fragment (§8.6). owner_type/visibility default
// when omitted so the common (Agent tool_call / session import) shapes need
// only the snippet + source_ref.
type captureRequest struct {
	WorkspaceID           string `json:"workspace_id" binding:"required"`
	SourceKind            string `json:"source_kind" binding:"required"`
	SourceRef             string `json:"source_ref" binding:"required"`
	SourceAssetID         string `json:"source_asset_id"`
	SourceAssetVersionID  string `json:"source_asset_version_id"`
	Visibility            string `json:"visibility"`
	OwnerType             string `json:"owner_type"`
	Snippet               string `json:"snippet" binding:"required"`
	// RetentionPolicyID is optional; the service resolves the effective policy
	// from (workspace, source_kind proxy = memory_type none) when nil (§9.2).
	RetentionPolicyID string `json:"retention_policy_id"`
}

// captureResponse is the §11.4 success body for capture.
type captureResponse struct {
	EvidenceID   string `json:"evidence_id"`
	Inline       bool   `json:"inline"`
	ContentHash  string `json:"content_hash"`
	Classification string `json:"classification"`
}

// Capture handles POST /api/v1/memory/evidence (memory_remember + session
// import entry, D9). The AuthMiddleware already authenticated the caller and
// set the AuthState; the evidence service enforces workspace-write RBAC.
func (h *MemoryHandler) Capture(c *gin.Context) {
	var body captureRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badRequestErr("invalid request body"))
		return
	}

	wsID, err := uuid.Parse(body.WorkspaceID)
	if err != nil {
		response.Fail(c, badRequestErr("invalid workspace_id"))
		return
	}
	if body.Snippet == "" {
		response.Fail(c, badRequestErr("snippet must not be empty"))
		return
	}

	st := MustAuth(c)
	ownerType := resolveOwnerType(body.OwnerType, st)
	req := evidence.CaptureRequest{
		WorkspaceID:  wsID,
		OwnerType:     ownerType,
		OwnerID:       principalID(st),
		SourceKind:    resolveSourceKind(body.SourceKind),
		SourceRef:     body.SourceRef,
		Visibility:    resolveVisibility(body.Visibility),
		RawSnippet:    body.Snippet,
		AuthzRevision: 0, // §4.4 audit-only; populated by a follow-up from the authz revision repo
	}
	if body.SourceAssetID != "" {
		if id, err := uuid.Parse(body.SourceAssetID); err == nil {
			req.SourceAssetID = &id
		} else {
			response.Fail(c, badRequestErr("invalid source_asset_id"))
			return
		}
	}
	if body.SourceAssetVersionID != "" {
		if id, err := uuid.Parse(body.SourceAssetVersionID); err == nil {
			req.SourceAssetVersionID = &id
		} else {
			response.Fail(c, badRequestErr("invalid source_asset_version_id"))
			return
		}
	}
	if body.RetentionPolicyID != "" {
		if id, err := uuid.Parse(body.RetentionPolicyID); err == nil {
			req.RetentionPolicyID = &id
		} else {
			response.Fail(c, badRequestErr("invalid retention_policy_id"))
			return
		}
	}

	res, err := h.svc.Capture(c.Request.Context(), memoryAuth(st), req, h.sink)
	if err != nil {
		response.Fail(c, mapMemoryErr(err))
		return
	}
	response.OK(c, captureResponse{
		EvidenceID:     res.EvidenceID.String(),
		Inline:         res.Inline,
		ContentHash:    res.ContentHash,
		Classification: string(res.Classification),
	})
}

// memoryAuth maps the HTTP AuthState to the evidence service's AuthContext
// (mirrors the source/asset handlers' srcAuth/assetAuth helpers). The RBAC
// subject is the acting principal (principalID); a service-account caller
// resolves to its service account id with no admin bypass; GroupIDs is plumbed
// so group-inherited grants apply.
func memoryAuth(s AuthState) evidence.AuthContext {
	return evidence.AuthContext{
		SubjectType:     principalSubjectType(s),
		PrincipalID:     principalID(s),
		GroupIDs:        s.Groups,
		IsAdmin:         s.IsAdmin,
		IsServiceCaller: s.IsServiceCaller,
	}
}

// resolveOwnerType maps the body's owner_type to a domain OwnerType, defaulting
// from the authenticated caller (an agent caller → OwnerAgent; otherwise the
// resolved subject type). The owner drives the evidence row's ACL owner + the
// `evidence.captured` event actor (§4.4).
func resolveOwnerType(s string, st AuthState) domain.OwnerType {
	switch strings.ToLower(s) {
	case "user":
		return domain.OwnerUser
	case "agent":
		return domain.OwnerAgent
	case "service_account":
		return domain.OwnerServiceAccount
	case "group":
		return domain.OwnerGroup
	case "":
		switch principalSubjectType(st) {
		case domain.SubjectAgent:
			return domain.OwnerAgent
		case domain.SubjectServiceAccount:
			return domain.OwnerServiceAccount
		default:
			return domain.OwnerUser
		}
	}
	return domain.OwnerUser
}

// resolveSourceKind maps the body's source_kind to the domain enum, defaulting
// to session (the session-import entry's most common shape) on an unknown
// value. A document/code source_kind REQUIRES source_asset_id — the handler
// does NOT enforce that here (the service/validator surfaces it); the loose
// mapping keeps the endpoint forward-compatible with new source kinds.
func resolveSourceKind(s string) domain.EvidenceSourceKind {
	switch strings.ToLower(s) {
	case "session":
		return domain.EvidenceSourceSession
	case "message":
		return domain.EvidenceSourceMessage
	case "tool_call":
		return domain.EvidenceSourceToolCall
	case "document":
		return domain.EvidenceSourceDocument
	case "code":
		return domain.EvidenceSourceCode
	}
	return domain.EvidenceSourceSession
}

// resolveVisibility maps the body's visibility to the domain enum, defaulting
// to 'private' (the §2.2 default for session/message/tool_call evidence — only
// the owner can read it unless explicitly shared, 11 §8.3).
func resolveVisibility(s string) domain.EvidenceVisibility {
	switch strings.ToLower(s) {
	case "restricted":
		return domain.EvidenceRestricted
	case "private":
		return domain.EvidencePrivate
	}
	return domain.EvidencePrivate
}

// mapMemoryErr routes evidence-service sentinels to the §11.4 envelope. A
// forbidden capture is 403 (§4.4 — the caller is authenticated and asked to
// write, so the denial is allowed to surface); a rejected capture (secret
// detected, §4.1) is 400. Everything else falls through to the generic 500.
func mapMemoryErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, evidence.ErrCaptureForbidden) {
		return pkgerr.Forbidden("capture forbidden")
	}
	if errors.Is(err, evidence.ErrCaptureRejected) {
		return pkgerr.BadRequest(err.Error())
	}
	if errors.Is(err, evidence.ErrCryptoNotConfigured) {
		// A mis-configured KEK/ObjectStore is a server fault, not caller input.
		return pkgerr.Internal("memory capture unavailable")
	}
	return err
}
