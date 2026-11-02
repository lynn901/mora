package handler

// context_broker.go is the HTTP adapter for the Phase 6 Context Broker
// (design-docs/19 §11 REST + internal handler).
//
//   POST /api/v1/workspaces/{ws}/knowledge/search    — typed search (no context)
//   POST /api/v1/workspaces/{ws}/knowledge/context   — Context Broker (route+budget+cite)
//   GET  /api/v1/workspaces/{ws}/knowledge/policies  — list authority policies
//   PUT  /api/v1/workspaces/{ws}/knowledge/policies/{intent} — update a policy (PM gov)
//   POST /internal/v1/knowledge/search               — MCP Server typed search
//   POST /internal/v1/knowledge/context              — MCP Server + Context Proxy (doc 15 §12.3)
//
// The handler only binds HTTP ↔ Broker inputs and maps the Broker's
// ContextResponse three-state to the §11.5 error semantics. Business logic
// (the ten-step pipeline) lives in the context.Broker. Leak-safe (§8.2): an
// unauthorized / not-found result returns an EMPTY success, never a 403/404
// distinction — the three-state methods decide the envelope, and only
// IsProviderDown surfaces a non-2xx (503, §11.5) so a fault is never confused
// with "no knowledge".

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	knowledgectx "github.com/lynn901/mora/internal/module/knowledge/context"
	"github.com/lynn901/mora/internal/domain"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/response"
)

// ContextBrokerHandler exposes the Context Broker REST + internal surface. It
// composes the Broker (Execute) + the PolicyRepo (GET/PUT policies). The Broker
// enforces the §3.2 two-pass authz; the handler is a thin HTTP adapter.
type ContextBrokerHandler struct {
	broker    *knowledgectx.Broker
	policies  knowledgectx.PolicyRepo
	principal knowledgePrincipalResolver
}

// knowledgePrincipalResolver resolves the HTTP AuthState into the Broker's
// Principal (§7.1 step 1). The handler does not read client headers for the
// principal; it derives it from the AuthMiddleware-set AuthState.
type knowledgePrincipalResolver func(c *gin.Context) (knowledgectx.Principal, error)

// NewContextBrokerHandler wires the handler over the Broker + the policy repo.
// The principal resolver is the seam that maps AuthState → Principal; the
// default (authStatePrincipal) is installed here so the handler does not
// depend on a caller-side resolver.
func NewContextBrokerHandler(broker *knowledgectx.Broker, policies knowledgectx.PolicyRepo) *ContextBrokerHandler {
	return &ContextBrokerHandler{
		broker:    broker,
		policies:  policies,
		principal: authStatePrincipal,
	}
}

// contextRequest is the JSON body for POST /knowledge/context + /knowledge/search
// (§11.3 request). Mirrors knowledgectx.KnowledgeQuery with string-typed IDs
// (HTTP wire shape) the handler parses into uuids.
type contextRequest struct {
	Query          string         `json:"query"`
	WorkspaceID    string         `json:"workspace_id,omitempty"` // path overrides; body is fallback for internal
	AgentID        string         `json:"agent_id,omitempty"`
	AssetTypes     []string       `json:"asset_types,omitempty"`
	Filters        map[string]any `json:"filters,omitempty"`
	MaxTokens      int            `json:"max_tokens,omitempty"`
	MaxItems       int            `json:"max_items,omitempty"`
	TimeoutMS      int            `json:"timeout_ms,omitempty"`
	IncludeContent bool           `json:"include_content,omitempty"`
}

// candidateResponse serializes one KnowledgeCandidate for the §11.3 response.
// Field names align with the §11.3 example.
type candidateResponse struct {
	AssetID        string             `json:"asset_id"`
	AssetType      string             `json:"asset_type"`
	Title          string             `json:"title"`
	Snippet        string             `json:"snippet"`
	Score          float64            `json:"score"`
	Authority      float64            `json:"authority"`
	Freshness      float64            `json:"freshness,omitempty"`
	Confidence     *float64           `json:"confidence,omitempty"`
	ConflictTags   []string           `json:"conflict_tags,omitempty"`
	Relations      []relationResp     `json:"relations,omitempty"`
	Citation       citationResp       `json:"citation"`
}

type relationResp struct {
	RelationType string `json:"relation_type"`
	TargetID     string `json:"target_id"`
	TargetTitle  string `json:"target_title,omitempty"`
}

type citationResp struct {
	AssetID            string         `json:"asset_id"`
	AssetType          string         `json:"asset_type"`
	SourceRef          string         `json:"source_ref,omitempty"`
	VersionOrRevision  string         `json:"version_or_revision,omitempty"`
	UpdatedAt          string         `json:"updated_at,omitempty"`
	Authority          float64        `json:"authority"`
	Confidence         *float64       `json:"confidence,omitempty"`
	Locator            map[string]any `json:"locator,omitempty"`
}

type degradedSourceResponse struct {
	AssetType string `json:"asset_type"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"`
}

type truncationResponse struct {
	Reason            string   `json:"reason,omitempty"`
	TruncatedAssetIDs []string `json:"truncated_asset_ids,omitempty"`
	ContinueTools     []string `json:"continue_tools,omitempty"`
}

// contextResponse is the §11.3 success body for POST /knowledge/context.
type contextResponse struct {
	Intent          string                 `json:"intent"`
	PolicyVersion   int                    `json:"policy_version"`
	AuthzRevision   int64                  `json:"authz_revision"`
	DecisionID      string                 `json:"decision_id,omitempty"`
	Candidates      []candidateResponse    `json:"candidates"`
	DegradedSources []degradedSourceResponse `json:"degraded_sources,omitempty"`
	Truncation      truncationResponse     `json:"truncation,omitempty"`
}

// Context handles POST /api/v1/workspaces/{ws}/knowledge/context (§11.1).
// The Broker runs the ten-step pipeline; the handler maps the ContextResponse
// three-state to the §11.5 envelope. Leak-safe: authorized-empty + no-results
// both return 200 with an empty candidate list (existence never leaks);
// provider-down returns 503 (§11.5 — a fault is never "no knowledge").
func (h *ContextBrokerHandler) Context(c *gin.Context) {
	resp, status := h.runBroker(c)
	if status != 0 {
		return // runBroker already wrote the error envelope
	}
	response.OK(c, resp)
}

// Search handles POST /api/v1/workspaces/{ws}/knowledge/search (§11.1). This
// is the typed-search surface (no context assembly): it runs the Broker with
// the same pipeline but is documented as "direct typed search, no context" —
// the response shape is the same candidates list; callers wanting the full
// context envelope use /knowledge/context. The handler reuses runBroker; the
// distinction is the route + the caller's intent, not a different code path.
func (h *ContextBrokerHandler) Search(c *gin.Context) {
	resp, status := h.runBroker(c)
	if status != 0 {
		return
	}
	response.OK(c, resp)
}

// runBroker binds the request, resolves the principal, runs the Broker, and
// maps the ContextResponse three-state to the §11.5 envelope. Returns the
// response + a status code: 0 means "write the success envelope", non-zero
// means "the error envelope was already written".
func (h *ContextBrokerHandler) runBroker(c *gin.Context) (contextResponse, int) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		// internal routes have no :workspace_id param; fall back to the body.
		var body contextRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			response.Fail(c, badRequestErr("invalid request body"))
			return contextResponse{}, 1
		}
		if body.WorkspaceID != "" {
			if id, perr := uuid.Parse(body.WorkspaceID); perr == nil {
				wsID = id
			}
		}
		if wsID == uuid.Nil {
			response.Fail(c, badRequestErr("workspace_id required"))
			return contextResponse{}, 1
		}
		return h.executeFromBody(c, wsID, body)
	}
	var body contextRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badRequestErr("invalid request body"))
		return contextResponse{}, 1
	}
	return h.executeFromBody(c, wsID, body)
}

// executeFromBody builds the KnowledgeQuery + Principal and runs the Broker.
func (h *ContextBrokerHandler) executeFromBody(c *gin.Context, wsID uuid.UUID, body contextRequest) (contextResponse, int) {
	q := knowledgectx.KnowledgeQuery{
		Query:          body.Query,
		WorkspaceID:    wsID,
		Filters:        body.Filters,
		MaxTokens:      body.MaxTokens,
		MaxItems:       body.MaxItems,
		IncludeContent: body.IncludeContent,
	}
	if body.TimeoutMS > 0 {
		q.Timeout = millisToDuration(body.TimeoutMS)
	}
	if body.AgentID != "" {
		if id, err := uuid.Parse(body.AgentID); err == nil {
			q.AgentID = &id
		}
	}
	if len(body.AssetTypes) > 0 {
		q.AssetTypes = parseAssetTypes(body.AssetTypes)
	}

	principal, err := h.principal(c)
	if err != nil {
		// No resolvable principal → the Broker cannot authorize (§3.2). Map to
		// 401 (not 403 — leak-safe: a 403 would imply the resource exists).
		response.Fail(c, pkgerr.Unauthorized("principal required"))
		return contextResponse{}, 1
	}

	resp, err := h.broker.Execute(c.Request.Context(), q, principal)
	if err != nil {
		response.Fail(c, mapBrokerErr(err))
		return contextResponse{}, 1
	}

	// §11.5 three-state mapping. IsProviderDown → 503 (a fault, not "no
	// knowledge"). The other two states (authorized-empty, no-results) return
	// 200 with an empty candidate list — existence never leaks (§8.2).
	if resp.IsProviderDown() {
		response.Fail(c, pkgerr.ServiceUnavailable("knowledge provider unavailable"))
		return contextResponse{}, 1
	}
	return toContextResponse(resp), 0
}

// ListPolicies handles GET /api/v1/workspaces/{ws}/knowledge/policies (§11.1).
// Lists the current policies for all four intents; an intent with no DB row
// reports the built-in default (policy_version=0, §5.3).
func (h *ContextBrokerHandler) ListPolicies(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}
	out := make([]knowledgectx.PolicyResponse, 0, len(knowledgectx.KnownIntents))
	for _, intent := range knowledgectx.KnownIntents {
		rec, err := h.policies.LoadCurrent(c.Request.Context(), wsID, intent)
		if err != nil {
			// ErrPolicyNotFound → report the built-in default (§5.3).
			out = append(out, builtinPolicyResponse(intent))
			continue
		}
		out = append(out, policyRecordToResponse(rec))
	}
	response.OK(c, gin.H{"policies": out})
}

// policyRequest is the JSON body for PUT /knowledge/policies/{intent} (§2.2).
type policyRequest struct {
	PrimaryBasis         []string                `json:"primary_basis"`
	MustSurfaceConflicts []string                `json:"must_surface_conflicts"`
	Weights              map[string]float64      `json:"weights"`
	ExcludeWhen          []string                `json:"exclude_when"`
}

// UpdatePolicy handles PUT /api/v1/workspaces/{ws}/knowledge/policies/{intent}
// (§11.1, PM governance). Validates the §2.2 six-field schema + the §7.2
// mutual-exclusion (exclude_when ∩ must_surface_conflicts = ∅), then upserts
// a new versioned row (§5.3). A config violation is rejected with 400.
func (h *ContextBrokerHandler) UpdatePolicy(c *gin.Context) {
	wsID, err := uuid.Parse(c.Param("workspace_id"))
	if err != nil || wsID == uuid.Nil {
		response.Fail(c, badRequestErr("workspace_id required"))
		return
	}
	intent := knowledgectx.Intent(c.Param("intent"))
	if !knownIntent(intent) {
		response.Fail(c, badRequestErr("invalid intent"))
		return
	}
	var body policyRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Fail(c, badRequestErr("invalid request body"))
		return
	}
	cfg := policyRequestToConfig(body)
	if collisions, ok := knowledgectx.ValidatePolicyConfig(cfg); !ok {
		if len(collisions) > 0 {
			response.Fail(c, badRequestErr("exclude_when and must_surface_conflicts must not overlap: "+joinTags(collisions)))
			return
		}
		response.Fail(c, badRequestErr("invalid policy config"))
		return
	}
	rec := knowledgectx.AuthorityPolicyRecord{
		WorkspaceID: wsID,
		Intent:      intent,
		IsCurrent:   true,
		Config:      cfg,
	}
	saved, err := h.policies.Upsert(c.Request.Context(), rec)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, policyRecordToResponse(saved))
}

// toContextResponse maps the Broker's ContextResponse to the §11.3 wire shape.
func toContextResponse(r knowledgectx.ContextResponse) contextResponse {
	out := contextResponse{
		Intent:        string(r.Intent),
		PolicyVersion: r.PolicyVersion,
		AuthzRevision: r.AuthzRevision,
		Candidates:    make([]candidateResponse, 0, len(r.Candidates)),
	}
	if r.DecisionID != nil {
		out.DecisionID = r.DecisionID.String()
	}
	for _, c := range r.Candidates {
		out.Candidates = append(out.Candidates, toCandidateResponse(c))
	}
	if len(r.DegradedSources) > 0 {
		out.DegradedSources = make([]degradedSourceResponse, 0, len(r.DegradedSources))
		for _, d := range r.DegradedSources {
			out.DegradedSources = append(out.DegradedSources, degradedSourceResponse{
				AssetType: string(d.AssetType),
				Reason:    string(d.Reason),
				Detail:    d.Detail,
			})
		}
	}
	if r.Truncation.Reason != "" {
		out.Truncation = truncationResponse{
			Reason:            string(r.Truncation.Reason),
			TruncatedAssetIDs: r.Truncation.TruncatedAssetIDs,
			ContinueTools:     r.Truncation.ContinueTools,
		}
	}
	return out
}

// toCandidateResponse maps one KnowledgeCandidate to the §11.3 wire shape.
func toCandidateResponse(c knowledgectx.KnowledgeCandidate) candidateResponse {
	out := candidateResponse{
		AssetID:      c.AssetID.String(),
		AssetType:    string(c.AssetType),
		Title:        c.Title,
		Snippet:      c.Snippet,
		Score:        c.Score,
		Authority:    c.Authority,
		Freshness:    c.Freshness,
		Confidence:   c.Confidence,
		ConflictTags: c.ConflictTags,
		Citation: citationResp{
			AssetID:    c.Citation.AssetID.String(),
			AssetType:  string(c.Citation.AssetType),
			SourceRef:  c.Citation.SourceRef,
			UpdatedAt:  formatTime(c.Citation.UpdatedAt),
			Authority:  c.Citation.Authority,
			Confidence: c.Citation.Confidence,
			Locator:    c.Citation.Locator,
		},
	}
	if c.Citation.VersionOrRevision != "" {
		out.Citation.VersionOrRevision = c.Citation.VersionOrRevision
	}
	if len(c.Relations) > 0 {
		out.Relations = make([]relationResp, 0, len(c.Relations))
		for _, r := range c.Relations {
			out.Relations = append(out.Relations, relationResp{
				RelationType: r.RelationType,
				TargetID:     r.TargetID.String(),
				TargetTitle:  r.TargetTitle,
			})
		}
	}
	return out
}

// policyRecordToResponse maps a repo record to the §11.1 GET response shape.
func policyRecordToResponse(rec knowledgectx.AuthorityPolicyRecord) knowledgectx.PolicyResponse {
	return knowledgectx.PolicyResponse{
		Intent:               rec.Intent,
		PolicyVersion:        rec.PolicyVersion,
		IsCurrent:            rec.IsCurrent,
		PrimaryBasis:         rec.Config.PrimaryBasis,
		MustSurfaceConflicts: rec.Config.MustSurfaceConflicts,
		Weights:              rec.Config.Weights,
		ExcludeWhen:          rec.Config.ExcludeWhen,
	}
}

// builtinPolicyResponse is the §5.3 fallback: an intent with no DB row reports
// the built-in default at policy_version=0 (the "default, not DB" sentinel).
// Delegates to the context package so the built-in config extraction stays
// internal to the context package (builtInPolicy is unexported).
func builtinPolicyResponse(intent knowledgectx.Intent) knowledgectx.PolicyResponse {
	return knowledgectx.BuiltinPolicyResponse(intent)
}

// policyRequestToConfig maps the wire policyRequest (string-typed) to the
// domain PolicyConfig (typed AssetType keys).
func policyRequestToConfig(body policyRequest) knowledgectx.PolicyConfig {
	cfg := knowledgectx.PolicyConfig{
		MustSurfaceConflicts: body.MustSurfaceConflicts,
		ExcludeWhen:          body.ExcludeWhen,
		Weights:              make(map[domain.AssetType]float64, len(body.Weights)),
	}
	for _, t := range body.PrimaryBasis {
		if at, ok := parseAssetTypeOk(t); ok {
			cfg.PrimaryBasis = append(cfg.PrimaryBasis, at)
		}
	}
	for t, w := range body.Weights {
		if at, ok := parseAssetTypeOk(t); ok {
			cfg.Weights[at] = w
		}
	}
	return cfg
}

// mapBrokerErr maps the Broker's caller/config errors to the §11.4 envelope.
// Leak-safe (§8.2): a no-workspace / no-principal / no-policy is a 400/401/500
// caller/config fault, NOT a 403 (a 403 would imply the resource exists).
func mapBrokerErr(err error) error {
	switch {
	case isBrokerErr(err, knowledgectx.ErrBrokerNoWorkspace):
		return pkgerr.BadRequest("workspace_id required")
	case isBrokerErr(err, knowledgectx.ErrBrokerNoPrincipal):
		return pkgerr.Unauthorized("principal required")
	case isBrokerErr(err, knowledgectx.ErrBrokerNoPolicy):
		return pkgerr.Internal("no authority policy configured")
	default:
		return err
	}
}

// isBrokerErr reports whether err wraps the target Broker sentinel.
func isBrokerErr(err, target error) bool {
	if err == nil {
		return false
	}
	return err == target || err.Error() == target.Error()
}

// authStatePrincipal resolves the HTTP AuthState into the Broker's Principal
// (§7.1 step 1). The handler does not trust any client header for the
// principal — it derives it from the AuthMiddleware-set state. A service
// caller (internal route) without a delegated context resolves to a
// service_account principal; a delegated agent context carries AgentID +
// ActingUserID.
func authStatePrincipal(c *gin.Context) (knowledgectx.Principal, error) {
	st := MustAuth(c)
	if st.UserID == uuid.Nil && st.AgentID == uuid.Nil {
		// No resolvable principal. A service-token-only caller with no
		// delegated context cannot be authorized (§11.2 — the token alone does
		// not represent the end user).
		return knowledgectx.Principal{}, pkgerr.Unauthorized("principal required")
	}
	p := knowledgectx.Principal{
		SubjectType: principalSubjectType(st),
		GroupIDs:    st.Groups,
	}
	if st.SubjectType == domain.SubjectAgent && st.AgentID != uuid.Nil {
		p.AgentID = &st.AgentID
		p.PrincipalID = st.AgentID
		if st.UserID != uuid.Nil {
			uid := st.UserID
			p.ActingUserID = &uid
		}
	} else {
		p.PrincipalID = st.UserID
	}
	return p, nil
}

// knownIntent reports whether intent is one of the four built-in intents.
func knownIntent(intent knowledgectx.Intent) bool {
	for _, i := range knowledgectx.KnownIntents {
		if i == intent {
			return true
		}
	}
	return false
}

// parseAssetTypes converts a string list to []AssetType, dropping unknowns.
func parseAssetTypes(ts []string) []domain.AssetType {
	out := make([]domain.AssetType, 0, len(ts))
	for _, s := range ts {
		if at, ok := parseAssetTypeOk(s); ok {
			out = append(out, at)
		}
	}
	return out
}

func parseAssetTypeOk(s string) (domain.AssetType, bool) {
	switch domain.AssetType(s) {
	case domain.AssetTypeDocument, domain.AssetTypeCodebase, domain.AssetTypeMemory, domain.AssetTypeSkill:
		return domain.AssetType(s), true
	}
	return "", false
}

func joinTags(tags []string) string {
	out := ""
	for i, t := range tags {
		if i > 0 {
			out += ", "
		}
		out += t
	}
	return out
}

// formatTime renders a time.Time as RFC3339, empty when zero (§11.3 omitempty
// on the wire — the handler emits "" so the JSON omitempty hides it).
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// millisToDuration converts milliseconds to a time.Duration (§11.3 timeout_ms).
func millisToDuration(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}
