// degrade.go — DegradedSource / ContextResponse 结构（§7.3）+ 三状态方法（D8）。
//
// D8 (§0 / 附录 A #17): the three empty-result states are DISTINGUISHABLE —
// Provider down / empty-after-authorization / genuinely-no-results must NOT
// collapse into a single "no knowledge" (12 §11.4). The four state methods
// below are the acceptance-gate carrier (§12 验收门禁 "Provider 故障/授权过滤
// 后为空/真实无结果三状态可区分"); the metrics layer mirrors them as
// knowledge_context_empty_total{state=provider_down|empty_authorized|
// empty_no_results} (§9.2).
//
// Degradation behavior (§7.3): a per-port failure does NOT block other ports —
// the failed port lands in DegradedSources. Qdrant unavailable → document/
// memory degrade to FTS (§9.6 / §15). Reranker unavailable → keep RRF fusion,
// do not block. All type engines failing returns a structured DegradedSources,
// never an explanation dressed as "no knowledge" (§9.6).

package contextbroker

import (
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// DegradedSource records a type-query port that failed or degraded (§7.3).
// Reason is the machine-readable failure category; Detail is a redacted summary
// — never body content or credentials (§7.3 / §13.4 audit).
type DegradedSource struct {
	AssetType domain.AssetType
	Reason    string // provider_timeout | provider_error | qdrant_unavailable | fts_fallback | rerank_unavailable
	Detail    string // redacted summary; no body / credentials
}

// ContextResponse is the Broker's output (§7.3). The four state methods below
// make the three empty-result states + the provider-down state distinguishable
// (D8 acceptance gate). Candidates is the budgeted, deduped, conflict-preserving
// final set; DegradedSources lists per-port failures; Truncation records what
// the Budgeter dropped + why; Intent/PolicyVersion/AuthzRevision/DecisionID
// are the audit anchors recorded in the context.query audit summary (§9.3).
type ContextResponse struct {
	Candidates       []KnowledgeCandidate
	DegradedSources  []DegradedSource
	Truncation       TruncationReport
	Intent           Intent
	PolicyVersion    int
	AuthzRevision    int64
	DecisionID       *uuid.UUID
	// authzVisibleRemoved is set by the broker when the authz post-check
	// (VisibleAssets) narrowed a non-empty pre-authz candidate set to empty —
	// the IsEmptyAuthorized signal. Unexported so it cannot be set from outside
	// the package; the broker sets it during step 5 (§7.1).
	authzVisibleRemoved bool
}

// HasResults reports real results (§7.3): at least one candidate survived
// budgeting. Distinguished from the empty states so a caller does not mistake
// a degraded-but-populated response for empty.
func (r ContextResponse) HasResults() bool { return len(r.Candidates) > 0 }

// IsEmptyAuthorized reports "authorized-filter-empty" (§7.3): candidates were
// produced by the type ports BUT the authz post-check (VisibleAssets) removed
// every one. Existence of the underlying assets is never leaked — this is
// indistinguishable to the caller from a genuine no-results, but the broker /
// metrics layer distinguishes them (D8 / §12 gate).
//
// Detection: there are no surviving candidates AND no provider failures (no
// DegradedSources) AND the authz post-check did drop something. The broker
// sets hadCandidatesPreAuthz via the authzVisibleRemoved flag below when the
// post-check narrowed a non-empty pre-authz set to empty.
func (r ContextResponse) IsEmptyAuthorized() bool {
	return len(r.Candidates) == 0 &&
		len(r.DegradedSources) == 0 &&
		r.authzVisibleRemoved
}

// IsEmptyNoResults reports "genuinely no results" (§7.3): all type ports
// returned empty AND no provider failed. This is the true "no knowledge" state
// — distinct from a provider outage (IsProviderDown) and from an
// authorization-filtered-to-empty set (IsEmptyAuthorized), per D8.
func (r ContextResponse) IsEmptyNoResults() bool {
	return len(r.Candidates) == 0 &&
		len(r.DegradedSources) == 0 &&
		!r.authzVisibleRemoved
}

// IsProviderDown reports "provider failure" (§7.3): at least one type port
// failed (DegradedSources non-empty) AND no candidate survived. Distinct from
// a partial-success response (which has both candidates + degraded sources) —
// here the provider failure left the result empty, so the caller sees a
// structured degradation, not "no knowledge" (§9.6).
func (r ContextResponse) IsProviderDown() bool {
	return len(r.Candidates) == 0 && len(r.DegradedSources) > 0
}
