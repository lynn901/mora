package handler

// memory_recall.go is the HTTP adapter for the Phase 4 memory recall +
// feedback + evidence-read surface (design-docs/18 §7.1, §8, §4.3).
//
//   GET    /api/v1/memory/units            — structured recall (§8, §9.3)
//   POST   /api/v1/memory/units/{id}/feedback — useful/incorrect/stale (§8.3)
//   POST   /api/v1/memory/evidence/{id}:read  — §4.3 Evidence ACL excerpt
//
// The handler only binds HTTP ↔ service inputs and maps service errors to the
// §11.4 envelope. Business logic (authority ranking, leak-safe filtering, the
// §4.3 ACL chain, feedback authority adjust + revalidate trigger) lives in the
// recall service (internal/module/memory/recall). Leak-safe by construction
// (§9.3): an unauthorized / unpublished / non-owner-private result returns an
// EMPTY success list, never a 403/404 — existence does not leak.
//
// All write paths are outbox-in-tx (§6.3); the feedback sink owns the
// memory_feedback row + the evidence.revalidate event boundary.

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
)

// MemoryRecallHandler exposes the recall + feedback + evidence-read endpoints.
// It composes the recall service (Recall + ReadExcerpt) and the feedback
// service. The services enforce RBAC + leak-safe filtering; the handler is a
// thin HTTP adapter.
type MemoryRecallHandler struct {
	recall   *recall.RecallService
	feedback *recall.FeedbackService
}

// NewMemoryRecallHandler wires the recall + feedback handlers over their
// services.
func NewMemoryRecallHandler(recallSvc *recall.RecallService, feedbackSvc *recall.FeedbackService) *MemoryRecallHandler {
	return &MemoryRecallHandler{recall: recallSvc, feedback: feedbackSvc}
}

// recallResponse serializes one KnowledgeCandidate for the REST surface. The
// ProjectionRef is internal-only (§8.1) and never serialized.
type recallResponse struct {
	UnitID     string             `json:"unit_id"`
	AssetID    string             `json:"asset_id"`
	MemoryType string             `json:"memory_type"`
	Title      string             `json:"title"`
	Snippet    string             `json:"snippet"`
	Score      float64            `json:"score"`
	Authority  float64            `json:"authority"`
	Freshness  float64            `json:"freshness"`
	Confidence *float64           `json:"confidence,omitempty"`
	Relations  []relationResponse `json:"relations,omitempty"`
	Citation   citationResponse   `json:"citation"`
	State      string             `json:"state"`
}

type relationResponse struct {
	RelationType string `json:"relation_type"`
	TargetID     string `json:"target_id"`
	TargetTitle  string `json:"target_title,omitempty"`
}

type citationResponse struct {
	AssetID         string         `json:"asset_id"`
	AssetVersionID  string         `json:"asset_version_id,omitempty"`
	EvidenceID      string         `json:"evidence_id,omitempty"`
	QuoteLocator    map[string]any `json:"quote_locator,omitempty"`
	SupportType     string         `json:"support_type,omitempty"`
	EvidenceMissing bool           `json:"evidence_missing"`
}

// recallListResponse is the §11.4 success body for GET /memory/units.
type recallListResponse struct {
	Items []recallResponse `json:"items"`
}

// Recall handles GET /api/v1/memory/units (structured recall, §8). The caller
// supplies the workspace + optional filters (owner / memory_type / valid_at /
// asset_id / include_candidates / query). Leak-safe (§9.3): an unauthorized /
// unpublished / non-owner-private result returns an EMPTY list, never an error.
func (h *MemoryRecallHandler) Recall(c *gin.Context) {
	st := MustAuth(c)

	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}

	q := recall.KnowledgeQuery{
		WorkspaceID: wsID,
		MaxItems:    optIntQuery(c, "max_items", 20),
	}
	if qstr := c.Query("query"); qstr != "" {
		q.Query = qstr
	}
	if mt := c.Query("memory_type"); mt != "" {
		q.MemoryType = &mt
	}
	if oid := c.Query("owner_id"); oid != "" {
		if id, err := uuid.Parse(oid); err == nil {
			q.OwnerID = &id
		}
	}
	if aid := c.Query("asset_id"); aid != "" {
		if id, err := uuid.Parse(aid); err == nil {
			q.AssetID = &id
		}
	}
	if va := c.Query("valid_at"); va != "" {
		if t, err := time.Parse(time.RFC3339, va); err == nil {
			q.ValidAt = &t
		}
	}
	if c.Query("include_candidates") == "true" {
		q.IncludeCandidates = true
	}

	items, err := h.recall.Recall(c.Request.Context(), memoryRecallAuth(st), q)
	if err != nil {
		// §9.3 leak-safe: a bad query surfaces as 400; an auth denial is
		// indistinguishable from empty — empty list, never 403.
		if errors.Is(err, recall.ErrInvalidQuery) {
			response.Fail(c, badRequestErr("invalid recall query"))
			return
		}
		// Any other error: return an empty list (existence never leaks) + log.
		response.OK(c, recallListResponse{Items: []recallResponse{}})
		return
	}

	out := make([]recallResponse, 0, len(items))
	for _, it := range items {
		out = append(out, toRecallResponse(it))
	}
	response.OK(c, recallListResponse{Items: out})
}

// feedbackRequest is the JSON body for POST /memory/units/{id}/feedback (§8.3).
type feedbackRequest struct {
	FeedbackType      string `json:"feedback_type" binding:"required"`
	RationaleRedacted string `json:"rationale_redacted"`
}

// Feedback handles POST /api/v1/memory/units/{id}/feedback (§8.3). The service
// applies the authority delta + triggers revalidate for incorrect/stale; the
// statement is never modified (§8.5). Leak-safe (§9.3): a caller without
// use/read on the unit gets a 404 (indistinguishable from not-found).
func (h *MemoryRecallHandler) Feedback(c *gin.Context) {
	st := MustAuth(c)

	unitID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid unit id"))
		return
	}
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}

	var body feedbackRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badRequestErr("invalid request body"))
		return
	}
	ft := resolveFeedbackType(body.FeedbackType)
	if ft == "" {
		response.Fail(c, badRequestErr("invalid feedback_type"))
		return
	}

	req := recall.FeedbackRequest{
		WorkspaceID:       wsID,
		MemoryUnitID:      unitID,
		FeedbackType:      ft,
		RationaleRedacted: body.RationaleRedacted,
	}
	id, err := h.feedback.Submit(c.Request.Context(), memoryRecallAuth(st), req)
	if err != nil {
		response.Fail(c, mapFeedbackErr(err))
		return
	}
	response.OK(c, gin.H{"feedback_id": id.String()})
}

// evidenceReadResponse is the §4.3 leak-safe excerpt result. Readable=false
// means the caller got the redacted reference + evidence_type + verification
// status — never an error distinguishing 403/404 (§9.3).
type evidenceReadResponse struct {
	EvidenceID         string `json:"evidence_id"`
	Readable           bool   `json:"readable"`
	Excerpt            string `json:"excerpt,omitempty"`
	EvidenceType       string `json:"evidence_type,omitempty"`
	VerificationStatus string `json:"verification_status,omitempty"`
	EvidenceMissing    bool   `json:"evidence_missing,omitempty"`
}

// EvidenceRead handles POST /api/v1/memory/evidence/{id}:read (§4.3 ACL chain).
// The caller supplies the unit id the evidence is read THROUGH (step 1 of the
// §4.3 chain). Leak-safe (§9.3): a missing/purged/denied evidence returns
// Readable=false with the same shape — no 403/404 distinction.
func (h *MemoryRecallHandler) EvidenceRead(c *gin.Context) {
	st := MustAuth(c)

	evID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.Fail(c, badRequestErr("invalid evidence id"))
		return
	}
	wsID, err := uuid.Parse(c.Query("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}
	unitID, err := uuid.Parse(c.Query("memory_unit_id"))
	if err != nil || unitID == uuid.Nil {
		response.Fail(c, badRequestErr("memory_unit_id required"))
		return
	}

	req := recall.ReadExcerptRequest{
		WorkspaceID:  wsID,
		EvidenceID:   evID,
		MemoryUnitID: unitID,
	}
	res, err := h.recall.ReadExcerpt(c.Request.Context(), memoryRecallAuth(st), req)
	if err != nil {
		// §9.3 leak-safe: a malformed request surfaces as 400; an ACL deny / a
		// missing evidence returns the not-readable shape, never an error.
		if errors.Is(err, recall.ErrEvidenceReadInvalid) {
			response.Fail(c, badRequestErr("invalid evidence read request"))
			return
		}
		response.OK(c, evidenceReadResponse{
			EvidenceID:      evID.String(),
			EvidenceMissing: true,
		})
		return
	}
	response.OK(c, evidenceReadResponse{
		EvidenceID:         res.EvidenceID.String(),
		Readable:           res.Readable,
		Excerpt:            res.Excerpt,
		EvidenceType:       res.EvidenceType,
		VerificationStatus: res.VerificationStatus,
		EvidenceMissing:    res.EvidenceMissing,
	})
}

// toRecallResponse shapes a KnowledgeCandidate into the REST response (§8.1
// carries the traceable evidence citation; §8.2 carries relations).
func toRecallResponse(c recall.KnowledgeCandidate) recallResponse {
	out := recallResponse{
		UnitID:     c.UnitID.String(),
		AssetID:    c.AssetID.String(),
		MemoryType: c.MemoryType,
		Title:      c.Title,
		Snippet:    c.Snippet,
		Score:      c.Score,
		Authority:  c.Authority,
		Freshness:  c.Freshness,
		Confidence: c.Confidence,
		State:      c.State,
		Citation: citationResponse{
			AssetID:         c.Citation.AssetID.String(),
			EvidenceMissing: c.Citation.EvidenceMissing,
		},
	}
	if c.Citation.AssetVersionID != nil {
		out.Citation.AssetVersionID = c.Citation.AssetVersionID.String()
	}
	if c.Citation.EvidenceID != uuid.Nil {
		out.Citation.EvidenceID = c.Citation.EvidenceID.String()
	}
	out.Citation.QuoteLocator = c.Citation.QuoteLocator
	out.Citation.SupportType = c.Citation.SupportType
	if len(c.Relations) > 0 {
		out.Relations = make([]relationResponse, 0, len(c.Relations))
		for _, r := range c.Relations {
			out.Relations = append(out.Relations, relationResponse{
				RelationType: r.RelationType,
				TargetID:     r.TargetID.String(),
				TargetTitle:  r.TargetTitle,
			})
		}
	}
	return out
}

// memoryRecallAuth maps the HTTP AuthState to the recall service's AuthContext.
// Mirrors memoryAuth for the evidence capture service (same shape, same RBAC
// plumbing). The recall service uses it for the §4.3 Evidence ACL chain + the
// §8.5 owner-only candidate read.
func memoryRecallAuth(s AuthState) recall.AuthContext {
	return recall.AuthContext{
		SubjectType:     principalSubjectType(s),
		PrincipalID:     principalID(s),
		GroupIDs:        s.Groups,
		IsAdmin:         s.IsAdmin,
		IsServiceCaller: s.IsServiceCaller,
	}
}

// resolveFeedbackType maps the body's feedback_type to the domain enum (§2.5).
// "" on an unknown value → the handler rejects as 400.
func resolveFeedbackType(s string) domain.FeedbackType {
	switch strings.ToLower(s) {
	case "useful":
		return domain.FeedbackUseful
	case "incorrect":
		return domain.FeedbackIncorrect
	case "stale":
		return domain.FeedbackStale
	}
	return ""
}

// mapFeedbackErr maps the recall feedback service's sentinels to the §11.4
// envelope. Leak-safe (§9.3): a forbidden/not-found is a 404 (no existence
// leak); an invalid type is a 400.
func mapFeedbackErr(err error) error {
	switch {
	case errors.Is(err, recall.ErrFeedbackForbidden):
		return pkgerr.NotFound("memory: unit not found or not visible")
	case errors.Is(err, recall.ErrFeedbackInvalid):
		return badRequestErr("invalid feedback")
	default:
		return err
	}
}

// optIntQuery reads an optional integer query param with a default.
func optIntQuery(c *gin.Context, key string, def int) int {
	s := c.Query(key)
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}
