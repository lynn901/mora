// Package context — ContextBroker main orchestration (design-docs/19 §7.1,
// the ten-step pipeline).
//
// The Broker is the delivery-convergence layer: it routes a query by intent to
// the typed query ports, runs the four ports in parallel under a shared
// deadline, dedups while keeping conflicts, scores under a versioned authority
// policy, trims to a budget, and returns candidates with traceable citations
// — never bypassing platform/authz (§3.2). This file is steps 1-10 of §7.1.
//
//	step 1-2: resolve principal + authz.Authorize → AuthzContext(decision_id,
//	           AllowedAssetIDs, revision) pushed down as a hard pre-filter.
//	step 3:   IntentRouter.Route → (Intent, assetTypes).
//	step 4:   concurrent fan-out to the typed ports (errgroup + shared deadline
//	           = min(q.Timeout, 2s)); each port returns candidates or a
//	           degraded reason. CodeGraph timeout does NOT block document
//	           results (§7.1 / §9.6).
//	step 5:   merge candidate IDs → authz batch post-check (VisibleAssets) →
//	           allowed subset only.
//	step 6:   DedupAndKeepConflicts (§7.2).
//	step 7:   AuthorityPolicy.Score (load the current policy for the intent).
//	step 8:   Budgeter.Select (§6.2).
//	step 9:   CitationBuilder.Build (§8).
//	step 10:  audit summary (selected/eliminated + policy_version +
//	           authz_revision).
//
// Each port fails independently: a port error or timeout is recorded as a
// DegradedSource and does NOT abort the other ports (§7.1). The three-state
// ContextResponse (§7.3) distinguishes provider-down / empty-authorized /
// empty-no-results so a failure is never reported as "no knowledge" (§11.4).
package context

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
)

// ErrBrokerNoWorkspace is returned when the query carries no workspace_id (the
// Broker cannot resolve an AuthzContext without a workspace scope, §3.2).
var ErrBrokerNoWorkspace = errors.New("context broker: workspace_id required")

// ErrBrokerNoPrincipal is returned when the principal cannot be resolved from
// the caller (the AuthzContext requires a PrincipalType + PrincipalID, §3.2).
var ErrBrokerNoPrincipal = errors.New("context broker: principal required")

// ErrBrokerNoPolicy is returned when no authority policy can be loaded for the
// resolved intent (neither a DB row nor the built-in default). The built-in
// default ALWAYS satisfies this (NewAuthorityPolicy never returns nil for a
// known intent), so this is a fatal wiring error (an unknown intent), not a
// runtime miss — surfaced so a typo does not silently fall back.
var ErrBrokerNoPolicy = errors.New("context broker: no authority policy for intent")

// PrincipalResolver resolves the caller's principal (SubjectType + PrincipalID
// + group memberships + acting user/agent) for the workspace, so the Broker
// can build the authz.Authorize request (§7.1 step 1). The handler/wiring
// layer implements this from the HTTP AuthState; the Broker does not read HTTP
// state. This is the seam that keeps the Broker transport-agnostic.
type PrincipalResolver func(ctx context.Context, workspaceID uuid.UUID) (Principal, error)

// Principal is the resolved caller identity the Broker authorizes as (§7.1
// step 1). Carried by the wiring layer from the HTTP AuthState / delegated
// context; the Broker never trusts a client header for it.
type Principal struct {
	SubjectType  domain.SubjectType
	PrincipalID  uuid.UUID
	GroupIDs     []uuid.UUID // user-path group memberships (nil for agents)
	ActingUserID *uuid.UUID  // agent-on-behalf-of-user (nil for direct users)
	AgentID      *uuid.UUID  // present when the principal is an agent
}

// AuthzService is the narrow slice of platform/authz.Service the Broker calls
// (§7.1 step 2 + step 5). Local interface so the Broker unit test injects a
// fake without depending on the authz package's wider surface; the real
// *authz.Service satisfies it (Authorize + VisibleAssets signatures match).
type AuthzService interface {
	// Authorize is the pre-fetch gate (§7.1 step 2): produces the AuthzContext
	// (DecisionID + AllowedAssetIDs + AuthzRevision) pushed down as a hard
	// pre-filter. The Broker authorizes at workspace scope (ActionUse on the
	// workspace) so AllowedAssetIDs is the workspace-level visible set; a
	// narrower per-asset check runs in step 5 (VisibleAssets).
	Authorize(ctx context.Context, req authz.AuthzRequest) (authz.AuthzContext, error)
	// VisibleAssets is the post-fetch batch check (§7.1 step 5): filters the
	// merged candidate IDs down to the allowed subset. Leak-safe (§8.2): an
	// unauthorized candidate is dropped, never surfaced as a 403/404.
	VisibleAssets(ctx context.Context, scope authz.ListScope, candidates []uuid.UUID) ([]uuid.UUID, error)
}

// PolicyLoader loads the current authority policy for a (workspace, intent),
// falling back to the built-in default when the repo has no row (§5.3). The
// Broker calls this at step 7; the returned policy's Intent() must match the
// routed intent, and the policy_version is recorded in the audit summary
// (step 10). Returns ErrPolicyNotFound only when neither a DB row nor a
// built-in default exists (a fatal wiring error for a known intent).
type PolicyLoader func(ctx context.Context, workspaceID uuid.UUID, intent Intent) (AuthorityPolicy, int, error)

// AuditSummary is the step-10 audit record (§9.3). The Broker emits it via the
// AuditSink; it carries IDs + counts + the redacted degraded/truncation
// summary, NEVER the candidate bodies (§13.4 "不记录完整正文").
type AuditSummary struct {
	WorkspaceID     uuid.UUID
	Intent          Intent
	PolicyVersion   int
	AuthzRevision   int64
	DecisionID      *uuid.UUID
	AssetTypes      []domain.AssetType
	CandidateCount  int // post-dedup, pre-budget (the pool that scored)
	SelectedCount   int
	TruncatedCount  int
	DegradedSources []DegradedSource
	DurationMS      int64
}

// AuditSink records the step-10 audit summary (§9.3). Best-effort: a sink
// failure never aborts the response (audit is relative to the request path,
// same precedent as platform/audit.Logger). Nil → no audit (the test path).
type AuditSink func(ctx context.Context, summary AuditSummary)

// Broker is the ContextBroker (§7.1). Execute runs the ten-step pipeline.
// Construct with NewBroker; the wiring layer (cmd/mora-api) injects the ports,
// the authz service, the policy loader, and the audit sink.
type Broker struct {
	router   IntentRouter
	ports    portSet      // the four typed query ports (nil = not wired)
	authz    AuthzService // pre-fetch + post-check gate (§3.2)
	policies PolicyLoader // step 7 policy load
	budgeter Budgeter
	cite     CitationBuilder
	audit    AuditSink
	defaults BudgetDefaults // workspace-default budget when the query omits it
	metrics  *Metrics       // §9.2 Prometheus instrumentation (nil = no-op, the test path)
}

// portSet holds the four typed query ports. A nil port is not wired (the
// Intent Router's type set is intersected with the wired ports; an unwired
// type is degraded as provider_unavailable rather than crashing the fan-out).
type portSet struct {
	document DocumentQuery
	code     CodeQuery
	memory   MemoryQuery
	skill    SkillQuery
}

// BudgetDefaults is the workspace-default budget when the query omits
// MaxTokens/MaxItems/Timeout (§6.3). The values consume the YS-212 PM
// governance delivery: MaxTokens=4000, MaxItems=20, Timeout=2s,
// max_inject_ratio=0.20, per-type TokenShare/MaxItems quota, and
// PerAssetTokenShare=0.30. The Broker builds the per-type Quota from this.
type BudgetDefaults struct {
	MaxTokens int
	MaxItems  int
	Timeout   time.Duration
	// TypeQuota is the per-type (MaxItems, TokenShare) cap. PerAssetTokenShare
	// is the single-asset cap (§6.2; 0 = disable, fall back to type-level
	// only). PM default 0.30.
	TypeQuota          map[domain.AssetType]Quota
	PerAssetTokenShare float64
}

// DefaultBudgetDefaults is the YS-212 PM governance baseline (§6.3). The
// per-type TokenShare sums to ~1.0 across the four types; MaxItems per type is
// the slice of the 20-item total weighted by the type's typical density. The
// Broker applies these when the query carries no explicit budget.
var DefaultBudgetDefaults = BudgetDefaults{
	MaxTokens:          4000,
	MaxItems:           20,
	Timeout:            2 * time.Second,
	PerAssetTokenShare: 0.30,
	TypeQuota: map[domain.AssetType]Quota{
		domain.AssetTypeDocument: {MaxItems: 8, TokenShare: 0.45},
		domain.AssetTypeCodebase: {MaxItems: 6, TokenShare: 0.25},
		domain.AssetTypeMemory:   {MaxItems: 4, TokenShare: 0.20},
		domain.AssetTypeSkill:    {MaxItems: 4, TokenShare: 0.10},
	},
}

// BrokerOption configures a Broker at construction (NewBroker). The functional
// pattern keeps the constructor signature stable as new ports are wired.
type BrokerOption func(*Broker)

// WithCodePort wires the CodeQuery port (§3.3). Unwired → codebase candidates
// degrade as provider_unavailable (CodeGraph is a sidecar; absent is normal in
// dev, §9.6).
func WithCodePort(p CodeQuery) BrokerOption { return func(b *Broker) { b.ports.code = p } }

// WithDocumentPort wires the DocumentQuery port.
func WithDocumentPort(p DocumentQuery) BrokerOption { return func(b *Broker) { b.ports.document = p } }

// WithSkillPort wires the SkillQuery port.
func WithSkillPort(p SkillQuery) BrokerOption { return func(b *Broker) { b.ports.skill = p } }

// WithAuditSink wires the step-10 audit sink (§9.3). Nil → no audit.
func WithAuditSink(s AuditSink) BrokerOption { return func(b *Broker) { b.audit = s } }

// WithBudgetDefaults overrides the workspace-default budget (§6.3). The wiring
// layer passes the PM-governed workspace config when present; otherwise the
// DefaultBudgetDefaults baseline applies.
func WithBudgetDefaults(d BudgetDefaults) BrokerOption { return func(b *Broker) { b.defaults = d } }

// WithMetrics wires the §9.2 Prometheus instrumentation (§9.2). Pass
// DefaultMetrics() in production; pass NilMetrics (or omit) in unit tests so
// the broker test does not depend on the global Prometheus registry. A nil
// metrics set is treated as no-op at every call site.
func WithMetrics(m *Metrics) BrokerOption {
	return func(b *Broker) {
		if m == nil {
			m = NilMetrics
		}
		b.metrics = m
	}
}

// NewBroker builds a ContextBroker over the always-required MemoryQuery port +
// the core components (router/authz/policy/budgeter/citation). The other typed
// ports are optional (With* options); an unwired type is degraded, not fatal.
// The MemoryQuery port is required because the fallback intent routes to
// [document, memory] (§4.2 rule 6) — a Broker with no memory port cannot
// serve the fallback.
func NewBroker(
	memory MemoryQuery,
	router IntentRouter,
	authzSvc AuthzService,
	policies PolicyLoader,
	budgeter Budgeter,
	cite CitationBuilder,
	defaults BudgetDefaults,
	opts ...BrokerOption,
) *Broker {
	if router == nil {
		router = NewIntentRouter(nil)
	}
	if budgeter == nil {
		budgeter = NewBudgeter()
	}
	if cite == nil {
		cite = NewCitationBuilder(nil)
	}
	b := &Broker{
		router:   router,
		ports:    portSet{memory: memory},
		authz:    authzSvc,
		policies: policies,
		budgeter: budgeter,
		cite:     cite,
		defaults: defaults,
		metrics:  NilMetrics, // default no-op; production wiring passes WithMetrics(DefaultMetrics())
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Execute runs the §7.1 ten-step pipeline and returns the ContextResponse
// (§7.3). It never returns an error for a port failure — that is a
// DegradedSource on the response. It returns an error only when the pipeline
// cannot start (no workspace / no principal / no policy) or when authz itself
// failed at the pre-fetch gate (step 2) — those are caller/config errors, not
// partial responses. Leak-safe (§8.2): an authorization denial surfaces as an
// empty result (IsEmptyAuthorized), never a 403 distinction.
func (b *Broker) Execute(ctx context.Context, q KnowledgeQuery, principal Principal) (ContextResponse, error) {
	start := time.Now()
	m := b.metrics // nil-safe: NewBroker installs NilMetrics, but defend in depth.
	if m == nil {
		m = NilMetrics
	}
	if q.WorkspaceID == uuid.Nil {
		return ContextResponse{}, ErrBrokerNoWorkspace
	}
	if principal.SubjectType == "" || principal.PrincipalID == uuid.Nil {
		return ContextResponse{}, ErrBrokerNoPrincipal
	}

	// step 3 (resolved early so the AuthzRequest can scope): route the intent
	// + asset types. Routing does NOT decide authorization (§4.2).
	intentStart := time.Now()
	intent, assetTypes, err := b.router.Route(ctx, q)
	if err != nil {
		return ContextResponse{}, err
	}
	m.ObserveStage(stageIntent, time.Since(intentStart))

	// steps 1-2: resolve principal + authz.Authorize → AuthzContext. The
	// Broker authorizes at workspace scope (ActionUse on the workspace) so
	// AllowedAssetIDs is the workspace-level visible set pushed down as the
	// hard pre-filter (§3.2). A denial here is a caller error (leak-safe:
	// surfaced downstream as empty-authorized, not a 403).
	authzStart := time.Now()
	ac, err := b.authorizeWorkspace(ctx, q.WorkspaceID, principal)
	if err != nil {
		return ContextResponse{}, err
	}

	// step 4: concurrent fan-out to the routed typed ports under a shared
	// deadline = min(q.Timeout, default.Timeout, 2s) (§7.1). Each port fails
	// independently; failures land in degradedSources.
	timeout := b.sharedDeadline(q.Timeout)
	parallelStart := time.Now()
	candidates, degraded := b.fanOut(ctx, ac, q, assetTypes, timeout)
	m.ObserveStage(stageParallel, time.Since(parallelStart))

	// step 5: merge candidate IDs → authz batch post-check (VisibleAssets) →
	// allowed subset only (§3.2 two-pass). Candidates NOT in the allowed
	// subset are dropped (no leak — they never reach the response).
	allowed, preCheckCount := b.postCheck(ctx, q.WorkspaceID, principal, candidates)
	allowedCands := filterByAllowed(candidates, allowed)
	// §9.2 candidates_total{disposition=denied}: each candidate that existed
	// pre-check but was denied by the post-check is counted by its type. The
	// action label is "use" (the per-asset use decision, §7.1 step 5).
	if deniedCount := preCheckCount - len(allowedCands); deniedCount > 0 {
		b.countDeniedByType(m, candidates, allowed, intent)
	}
	m.ObserveStage(stageAuthz, time.Since(authzStart))

	// step 7 load: the current policy for the intent (built-in default when
	// the repo has no row, §5.3). policy_version → audit (step 10).
	policy, policyVersion, err := b.loadPolicy(ctx, q.WorkspaceID, intent)
	if err != nil {
		return ContextResponse{}, err
	}
	m.SetPolicyVersion(q.WorkspaceID.String(), intent, policyVersion)

	// step 6: DedupAndKeepConflicts (§7.2). Runs BEFORE Score (the policy
	// scores the deduped pool, §7.2). exclude_when drops deprecated/
	// version_mismatch; conflict pairs are kept side-by-side.
	dedupStart := time.Now()
	deduped := DedupAndKeepConflicts(allowedCands, policy)
	m.ObserveStage(stageDedup, time.Since(dedupStart))

	// step 7: AuthorityPolicy.Score (§5.2). Blends authority/freshness/
	// confidence with the policy weights; stable-sorted by blended score desc.
	scored := policy.Score(deduped, q)

	// step 8: Budgeter.Select (§6.2). Trims to the budget; returns the
	// truncation report (reason + truncated asset_ids + continue tools).
	budget := b.budgetFor(q, assetTypes)
	budgetStart := time.Now()
	selected, truncation := b.budgeter.Select(scored, budget)
	m.ObserveStage(stageBudget, time.Since(budgetStart))

	// step 9: CitationBuilder.Build (§8). Completes the citation AFTER the
	// final authorization pass (§8.2 — the selected slice is post-check by
	// construction).
	citationStart := time.Now()
	cited := b.cite.Build(ctx, selected)
	m.ObserveStage(stageCitation, time.Since(citationStart))

	resp := ContextResponse{
		Candidates:             cited,
		DegradedSources:        degraded,
		Truncation:             truncation,
		Intent:                 intent,
		PolicyVersion:          policyVersion,
		AuthzRevision:          ac.AuthzRevision,
		DecisionID:             ac.DecisionID,
		preCheckCandidateCount: preCheckCount,
	}

	// §9.2 instrumentation of the response: tokens by type, candidate
	// dispositions, degraded reasons, and the three empty-state counter.
	b.recordResponseMetrics(m, resp, intent)

	// step 10: audit summary (§9.3). Best-effort; never blocks the response.
	if b.audit != nil {
		b.audit(ctx, AuditSummary{
			WorkspaceID:     q.WorkspaceID,
			Intent:          intent,
			PolicyVersion:   policyVersion,
			AuthzRevision:   ac.AuthzRevision,
			DecisionID:      ac.DecisionID,
			AssetTypes:      assetTypes,
			CandidateCount:  len(deduped),
			SelectedCount:   len(cited),
			TruncatedCount:  len(truncation.TruncatedAssetIDs),
			DegradedSources: degraded,
			DurationMS:      time.Since(start).Milliseconds(),
		})
	}
	m.ObserveStage(stageTotal, time.Since(start))
	return resp, nil
}

// authorizeWorkspace is §7.1 step 2: authz.Authorize at workspace scope. It is
// the workspace-membership gate — "may this principal access this workspace at
// all" — producing the AuthzContext (DecisionID + AuthzRevision) the response +
// audit carry. The per-asset use decision is NOT made here; it is the post-check
// (step 5, VisibleAssets), which calls Authorize(TargetType=asset, Action=use)
// per candidate. Keeping the two concerns separate is the §3.2 two-pass shape:
// step 2 is the coarse workspace gate (read/membership), step 5 is the precise
// per-asset use gate.
//
// A workspace-scope DENY is NOT a pipeline error: it means the principal is not
// a member of the workspace → the response is empty-authorized (the fan-out
// still runs against an empty AllowedAssetIDs, returning no candidates; the
// three-state method reports IsEmptyAuthorized). So an authz.ErrNotFound at this
// gate is converted to Allowed=false with no error — leak-safe (§8.2: a
// non-member never learns whether assets exist). Only a genuine authz
// infrastructure error (e.g. the revisions read failed) aborts the pipeline.
func (b *Broker) authorizeWorkspace(ctx context.Context, workspaceID uuid.UUID, p Principal) (AuthzContext, error) {
	req := authz.AuthzRequest{
		WorkspaceID:   workspaceID,
		PrincipalType: p.SubjectType,
		PrincipalID:   p.PrincipalID,
		GroupIDs:      p.GroupIDs,
		ActingUserID:  p.ActingUserID,
		AgentID:       p.AgentID,
		TargetType:    domain.TargetWorkspace, // workspace-scope membership gate
		TargetID:      workspaceID,
		Action:        domain.ActionRead, // "may this principal see this workspace"
	}
	ac, err := b.authz.Authorize(ctx, req)
	if err != nil {
		// A not-found / denied target at workspace scope is a membership denial,
		// not a pipeline error (§8.2 leak-safe: empty-authorized, never a 403).
		// Surface it as Allowed=false so the fan-out proceeds against an empty
		// AllowedAssetIDs and the three-state method reports IsEmptyAuthorized.
		if errors.Is(err, authz.ErrTargetNotFound) {
			ac.WorkspaceID = workspaceID
			ac.AuthzRevision = 0
			ac.PrincipalType = req.PrincipalType
			ac.PrincipalID = req.PrincipalID
			ac.Allowed = false
			return ac, nil
		}
		return ac, err
	}
	return ac, nil
}

// sharedDeadline is min(q.Timeout, default.Timeout, 2s) (§7.1 step 4). A
// caller-supplied Timeout tighter than the default is honored; a looser one is
// capped at 2s (the §14.3 SLO). Zero → the default.
func (b *Broker) sharedDeadline(q time.Duration) time.Duration {
	d := b.defaults.Timeout
	if d <= 0 || d > 2*time.Second {
		d = 2 * time.Second
	}
	if q > 0 && q < d {
		return q
	}
	return d
}

// fanOut is §7.1 step 4: concurrent typed-port calls under a shared deadline.
// Each port runs in its own goroutine; a port error or deadline is recorded
// as a DegradedSource and does NOT abort the other ports (§7.1). CodeGraph
// timeout does not block document results (§9.6 — the per-port ctx deadline
// enforces this; a code timeout returns while document results already landed).
//
// The routed assetTypes are intersected with the WIRED ports: an unwired type
// (e.g. codegraph sidecar not configured) is degraded as provider_unavailable
// rather than crashing the fan-out.
func (b *Broker) fanOut(ctx context.Context, ac AuthzContext, q KnowledgeQuery, types []domain.AssetType, timeout time.Duration) ([]KnowledgeCandidate, []DegradedSource) {
	// Per-port ctx with the shared deadline. A port that exceeds it returns
	// ctx.Err() → DegradedProviderTimeout (§7.3). The parent ctx cancellation
	// still propagates (request cancelled → all ports abort).
	portCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type portResult struct {
		cands []KnowledgeCandidate
		deg   *DegradedSource
	}

	// Build the per-type port call list. Order is not significant (concurrent);
	// a map keyed by type keeps the result attribution straight.
	calls := make([]struct {
		t   domain.AssetType
		run func() ([]KnowledgeCandidate, *DegradedSource)
	}, 0, len(types))
	for _, t := range types {
		switch t {
		case domain.AssetTypeDocument:
			if b.ports.document != nil {
				calls = append(calls, struct {
					t   domain.AssetType
					run func() ([]KnowledgeCandidate, *DegradedSource)
				}{t, func() ([]KnowledgeCandidate, *DegradedSource) {
					return runPort(portCtx, domain.AssetTypeDocument, func(c context.Context) ([]KnowledgeCandidate, error) {
						return b.ports.document.Search(c, ac, b.docRequest(q))
					})
				}})
			} else {
				calls = append(calls, unwiredDegraded(domain.AssetTypeDocument))
			}
		case domain.AssetTypeCodebase:
			if b.ports.code != nil {
				calls = append(calls, struct {
					t   domain.AssetType
					run func() ([]KnowledgeCandidate, *DegradedSource)
				}{t, func() ([]KnowledgeCandidate, *DegradedSource) {
					return runPort(portCtx, domain.AssetTypeCodebase, func(c context.Context) ([]KnowledgeCandidate, error) {
						return b.ports.code.Search(c, ac, b.codeRequest(q))
					})
				}})
			} else {
				calls = append(calls, unwiredDegraded(domain.AssetTypeCodebase))
			}
		case domain.AssetTypeMemory:
			if b.ports.memory != nil {
				calls = append(calls, struct {
					t   domain.AssetType
					run func() ([]KnowledgeCandidate, *DegradedSource)
				}{t, func() ([]KnowledgeCandidate, *DegradedSource) {
					return runPort(portCtx, domain.AssetTypeMemory, func(c context.Context) ([]KnowledgeCandidate, error) {
						return b.ports.memory.Recall(c, ac, b.memRequest(q))
					})
				}})
			} else {
				calls = append(calls, unwiredDegraded(domain.AssetTypeMemory))
			}
		case domain.AssetTypeSkill:
			if b.ports.skill != nil {
				calls = append(calls, struct {
					t   domain.AssetType
					run func() ([]KnowledgeCandidate, *DegradedSource)
				}{t, func() ([]KnowledgeCandidate, *DegradedSource) {
					return runPort(portCtx, domain.AssetTypeSkill, func(c context.Context) ([]KnowledgeCandidate, error) {
						return b.ports.skill.Discover(c, ac, b.skillRequest(q))
					})
				}})
			} else {
				calls = append(calls, unwiredDegraded(domain.AssetTypeSkill))
			}
		}
	}

	results := make([]portResult, len(calls))
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i, c := range calls {
		i, c := i, c
		go func() {
			defer wg.Done()
			cands, deg := c.run()
			results[i] = portResult{cands: cands, deg: deg}
		}()
	}
	wg.Wait()

	var all []KnowledgeCandidate
	var degraded []DegradedSource
	for _, r := range results {
		if r.deg != nil {
			degraded = append(degraded, *r.deg)
		}
		all = append(all, r.cands...)
	}
	return all, degraded
}

// runPort runs one port call and maps a timeout/error to a DegradedSource
// (§7.3). A nil error returns the candidates + nil degradation. A ctx deadline
// → provider_timeout; any other error → provider_error. The candidates from a
// failed port are dropped (the partial result from a port that errored mid-way
// is not trusted — §7.1 "每端口独立失败不阻断其他端口").
func runPort(ctx context.Context, t domain.AssetType, fn func(context.Context) ([]KnowledgeCandidate, error)) ([]KnowledgeCandidate, *DegradedSource) {
	cands, err := fn(ctx)
	if err == nil {
		return cands, nil
	}
	// Distinguish timeout from other errors (§7.3 reason vocabulary).
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
		return nil, &DegradedSource{AssetType: t, Reason: DegradedProviderTimeout, Detail: redactErr(err)}
	}
	return nil, &DegradedSource{AssetType: t, Reason: DegradedProviderError, Detail: redactErr(err)}
}

// unwiredDegraded returns a DegradedSource for an unwired port (a routed type
// whose adapter was not injected). The reason is provider_error with a detail
// that names the unwired state (not a leak — the caller already knows the type
// was routed; this says the engine is not configured, §9.6 CodeGraph absent).
func unwiredDegraded(t domain.AssetType) struct {
	t   domain.AssetType
	run func() ([]KnowledgeCandidate, *DegradedSource)
} {
	return struct {
		t   domain.AssetType
		run func() ([]KnowledgeCandidate, *DegradedSource)
	}{t, func() ([]KnowledgeCandidate, *DegradedSource) {
		return nil, &DegradedSource{AssetType: t, Reason: DegradedProviderError, Detail: "port not wired"}
	}}
}

// postCheck is §7.1 step 5: the batch post-check authorization pass. It calls
// VisibleAssets with the merged candidate IDs; only the allowed subset
// survives. Returns the allowed set + the pre-check count (for the
// IsEmptyAuthorized three-state method, §7.3). Leak-safe (§8.2): an
// unauthorized candidate is dropped, never surfaced.
func (b *Broker) postCheck(ctx context.Context, workspaceID uuid.UUID, p Principal, cands []KnowledgeCandidate) (map[uuid.UUID]bool, int) {
	preCheckCount := len(cands)
	if len(cands) == 0 {
		return nil, 0
	}
	ids := make([]uuid.UUID, 0, len(cands))
	seen := make(map[uuid.UUID]bool, len(cands))
	for _, c := range cands {
		if !seen[c.AssetID] {
			seen[c.AssetID] = true
			ids = append(ids, c.AssetID)
		}
	}
	allowed, err := b.authz.VisibleAssets(ctx, authz.ListScope{
		WorkspaceID:   workspaceID,
		PrincipalType: p.SubjectType,
		PrincipalID:   p.PrincipalID,
		GroupIDs:      p.GroupIDs,
		ActingUserID:  p.ActingUserID,
		AgentID:       p.AgentID,
	}, ids)
	if err != nil {
		return nil, preCheckCount // authz fault → fail closed (empty allowed, §3.2)
	}
	set := make(map[uuid.UUID]bool, len(allowed))
	for _, id := range allowed {
		set[id] = true
	}
	return set, preCheckCount
}

// filterByAllowed drops candidates whose AssetID is not in the allowed set
// (§7.1 step 5 post-check). Preserves order. A nil allowed set (authz fault)
// drops everything — fail closed (§3.2).
func filterByAllowed(cands []KnowledgeCandidate, allowed map[uuid.UUID]bool) []KnowledgeCandidate {
	if len(cands) == 0 {
		return nil
	}
	if len(allowed) == 0 {
		return nil // fail closed: no allowed set → no candidates surface
	}
	out := make([]KnowledgeCandidate, 0, len(cands))
	for _, c := range cands {
		if allowed[c.AssetID] {
			out = append(out, c)
		}
	}
	return out
}

// countDeniedByType increments the §9.2 candidates_total{disposition=denied}
// and authz_denied_total counters for each candidate the post-check denied,
// grouped by asset type (the metrics are labeled by type so a spike on one
// type is distinguishable). A candidate counted here never reaches the
// response (leak-safe — the counter records the denial, not the asset id,
// §13.4). The action label is "use" (the per-asset use decision, §7.1 step 5).
func (b *Broker) countDeniedByType(m *Metrics, cands []KnowledgeCandidate, allowed map[uuid.UUID]bool, intent Intent) {
	counted := make(map[domain.AssetType]int, 4)
	for _, c := range cands {
		if allowed[c.AssetID] {
			continue
		}
		counted[c.AssetType]++
	}
	for t, n := range counted {
		for i := 0; i < n; i++ {
			m.IncCandidate(intent, t, dispositionDenied)
		}
		m.IncAuthzDenied(string(domain.ActionUse), t)
	}
}

// recordResponseMetrics emits the §9.2 response-level metrics after the
// pipeline completes: token spend by type (selected slice), candidate
// dispositions (selected vs truncated), degraded-source reasons, and the
// three-state empty counter. Called once per Execute; best-effort (the metrics
// seam is nil-safe via the Broker's installed Metrics).
func (b *Broker) recordResponseMetrics(m *Metrics, resp ContextResponse, intent Intent) {
	// tokens by type: the selected slice's estimated token spend (§9.2). Uses
	// the same estimateTokens the Budgeter uses so the metric reflects the
	// budgeted spend, not the raw snippet length.
	typeSpend := make(map[domain.AssetType]int, 4)
	for _, c := range resp.Candidates {
		typeSpend[c.AssetType] += estimateTokens(c.Snippet)
	}
	for t, tokens := range typeSpend {
		m.ObserveTokens(t, tokens)
	}

	// candidates_total{disposition=selected}: one per surviving candidate.
	for _, c := range resp.Candidates {
		m.IncCandidate(intent, c.AssetType, dispositionSelected)
	}
	// candidates_total{disposition=truncated}: the truncated asset ids (the
	// Budgeter cut them after scoring in). The type is not on the truncation
	// report (it carries only asset_ids), so attribute to the intent's primary
	// type when known; otherwise count under a neutral "unknown" bucket by
	// omitting the type label. The truncation report carries asset_ids as
	// strings, so we re-derive the type from the scored pool is not possible
	// here without the scored slice — record by count against the primary type.
	if n := len(resp.Truncation.TruncatedAssetIDs); n > 0 {
		primary := primaryTypeForIntent(intent)
		for i := 0; i < n; i++ {
			m.IncCandidate(intent, primary, dispositionTruncated)
		}
	}

	// degraded_total{reason}: one per degraded source (§7.3 / §9.2).
	for _, d := range resp.DegradedSources {
		m.IncDegraded(d.Reason)
	}

	// empty_total{state}: exactly one of the three states per response (§7.3).
	// IsProviderDown takes precedence (a fault is never "no knowledge").
	switch {
	case resp.IsProviderDown():
		m.IncEmpty(emptyProviderDown)
	case resp.IsEmptyAuthorized():
		m.IncEmpty(emptyAuthorized)
	case resp.IsEmptyNoResults():
		m.IncEmpty(emptyNoResults)
	}
}

// primaryTypeForIntent returns the §5.1 primary-basis type for an intent, used
// to attribute truncated candidates (whose type the TruncationReport does not
// carry) to a type bucket for the candidates_total{disposition=truncated}
// metric. This is an attribution heuristic for the metric label only — the
// truncation report itself carries the asset_ids for the Agent's continue-read.
func primaryTypeForIntent(intent Intent) domain.AssetType {
	switch intent {
	case IntentSpec:
		return domain.AssetTypeDocument
	case IntentRevision:
		return domain.AssetTypeCodebase
	case IntentRationale:
		return domain.AssetTypeDocument
	case IntentProcedure:
		return domain.AssetTypeSkill
	}
	return domain.AssetTypeDocument
}

// loadPolicy is §7.1 step 7 load: the current policy for the intent. Delegates
// to the PolicyLoader; returns the policy + its policy_version for the audit
// summary (step 10). The PolicyLoader falls back to the built-in default when
// the repo has no row (§5.3), so this only errors on a fatal wiring fault.
func (b *Broker) loadPolicy(ctx context.Context, workspaceID uuid.UUID, intent Intent) (AuthorityPolicy, int, error) {
	if b.policies == nil {
		// No loader → built-in default (the test path / a wiring that has not
		// yet plumbed the repo). policy_version=0 signals "default, not DB".
		p := NewAuthorityPolicy(intent, PolicyConfig{})
		if p == nil {
			return nil, 0, ErrBrokerNoPolicy
		}
		return p, 0, nil
	}
	p, v, err := b.policies(ctx, workspaceID, intent)
	if err != nil {
		return nil, 0, err
	}
	if p == nil {
		return nil, 0, ErrBrokerNoPolicy
	}
	return p, v, nil
}

// budgetFor builds the Budget for the query (§6.3). MaxTokens/MaxItems/Timeout
// come from the query when set, else the workspace default. TypeQuota is the
// default per-type cap intersected with the routed types (a type not in the
// routed set gets no quota — its candidates cannot score in anyway).
func (b *Broker) budgetFor(q KnowledgeQuery, types []domain.AssetType) Budget {
	d := b.defaults
	budget := Budget{
		MaxTokens: d.MaxTokens,
		MaxItems:  d.MaxItems,
		Timeout:   d.Timeout,
		TypeQuota: make(map[domain.AssetType]Quota, len(types)),
	}
	if q.MaxTokens > 0 {
		budget.MaxTokens = q.MaxTokens
	}
	if q.MaxItems > 0 {
		budget.MaxItems = q.MaxItems
	}
	for _, t := range types {
		if q, ok := d.TypeQuota[t]; ok {
			budget.TypeQuota[t] = q
		} else {
			// A routed type with no default quota: give it a small slice so it
			// is not silently dropped (§6.2 — the Budgeter drops a type with no
			// quota; here we ensure every routed type has one).
			budget.TypeQuota[t] = Quota{MaxItems: d.MaxItems, TokenShare: 0.10}
		}
	}
	return budget
}

// docRequest / codeRequest / memRequest / skillRequest translate the Broker's
// KnowledgeQuery into the per-type narrowed request (ports.go). The Broker
// fills these from the query + AuthzContext; each port reads its own keys from
// Filters. The AuthzContext.AllowedAssetIDs is the hard pre-filter the ports
// forward (§3.2) — it is already on ac, not re-passed here.

func (b *Broker) docRequest(q KnowledgeQuery) DocumentQueryRequest {
	req := DocumentQueryRequest{
		Query:       q.Query,
		WorkspaceID: q.WorkspaceID,
		MaxItems:    b.defaults.MaxItems,
	}
	if dirID := filterUUID(q.Filters, "directory_id"); dirID != nil {
		req.DirectoryID = dirID
	}
	if q.MaxItems > 0 {
		req.MaxItems = q.MaxItems
	}
	return req
}

func (b *Broker) codeRequest(q KnowledgeQuery) CodeQueryRequest {
	req := CodeQueryRequest{
		Query:    q.Query,
		MaxItems: b.defaults.MaxItems,
	}
	if id := filterUUID(q.Filters, "codebase_asset_id"); id != nil {
		req.AssetID = *id
	}
	if lang, ok := q.Filters["language"].(string); ok {
		req.Language = lang
	}
	if glob, ok := q.Filters["path_glob"].(string); ok {
		req.PathGlob = glob
	}
	if q.MaxItems > 0 {
		req.MaxItems = q.MaxItems
	}
	return req
}

func (b *Broker) memRequest(q KnowledgeQuery) MemoryQueryRequest {
	req := MemoryQueryRequest{
		Query:       q.Query,
		WorkspaceID: q.WorkspaceID,
		MaxItems:    b.defaults.MaxItems,
	}
	if oid := filterUUID(q.Filters, "owner_id"); oid != nil {
		req.OwnerID = oid
	}
	if mt, ok := q.Filters["memory_type"].(string); ok {
		req.MemoryType = &mt
	}
	if aid := filterUUID(q.Filters, "asset_id"); aid != nil {
		req.AssetID = aid
	}
	if q.MaxItems > 0 {
		req.MaxItems = q.MaxItems
	}
	return req
}

func (b *Broker) skillRequest(q KnowledgeQuery) SkillQueryRequest {
	// SkillQuery requires AgentID (§3.3); the Broker pulls it from the
	// principal's AgentID when the principal is an agent. A non-agent principal
	// gets uuid.Nil, and the SkillAdapter returns an error → degraded
	// (provider_error). This is correct: skill delivery is agent-bound (§6.2).
	var agentID uuid.UUID
	if q.AgentID != nil {
		agentID = *q.AgentID
	}
	req := SkillQueryRequest{
		Query:       q.Query,
		WorkspaceID: q.WorkspaceID,
		AgentID:     agentID,
		MaxItems:    b.defaults.MaxItems,
	}
	if ids := filterUUIDSlice(q.Filters, "skill_asset_ids"); len(ids) > 0 {
		req.AssetIDs = ids
	}
	if q.MaxItems > 0 {
		req.MaxItems = q.MaxItems
	}
	return req
}

// filterUUID extracts a uuid from the opaque Filters map (§3.1 Filters). Nil
// when absent or malformed (no leak — a malformed filter is ignored, §8.2).
func filterUUID(filters map[string]any, key string) *uuid.UUID {
	if filters == nil {
		return nil
	}
	v, ok := filters[key]
	if !ok {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

// isTimeoutErr reports whether err is a net/context timeout the port wrapped
// (a port may wrap its engine's deadline error). errgroup-style: we rely on
// errors.Is(ctx.DeadlineExceeded) for the per-port ctx; this catches wrapped
// variants the engine emits.
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	// Common timeout error sentinels across engines (net, context, generic).
	s := err.Error()
	return containsAny(s, "timeout", "deadline exceeded", "context deadline", "i/o timeout")
}

// redactErr returns a redacted, credential-free summary of the error for the
// DegradedSource.Detail (§9.3 — audit/detail never carries credentials or
// body). Keeps the top-level message; drops any wrapped cause chain that might
// carry an address/secret.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Trim to a short summary; the full error is logged server-side, not on
	// the response (§9.3 redacted summary).
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// containsAny reports whether s contains any of the substrings (case-insensitive
// for the timeout heuristic).
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 {
			continue
		}
		// case-insensitive contains without allocating a lowercased copy of s
		// for every sub: lowercase s once.
		if containsFold(s, sub) {
			return true
		}
	}
	return false
}

// containsFold is a case-insensitive substring check.
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// equalFold is a minimal ASCII case-insensitive equality (errors from engines
// are ASCII messages; this avoids pulling strings.EqualFold overhead per byte).
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// filterUUIDSlice extracts a []uuid from the opaque Filters map (§3.1 Filters).
// Nil/empty when absent or malformed (no leak — a malformed filter is ignored).
func filterUUIDSlice(filters map[string]any, key string) []uuid.UUID {
	if filters == nil {
		return nil
	}
	v, ok := filters[key]
	if !ok {
		return nil
	}
	switch vv := v.(type) {
	case []string:
		out := make([]uuid.UUID, 0, len(vv))
		for _, s := range vv {
			if id, err := uuid.Parse(s); err == nil {
				out = append(out, id)
			}
		}
		return out
	case []any:
		out := make([]uuid.UUID, 0, len(vv))
		for _, item := range vv {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if id, err := uuid.Parse(s); err == nil {
				out = append(out, id)
			}
		}
		return out
	}
	return nil
}
