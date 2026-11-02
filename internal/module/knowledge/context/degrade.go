// Package context — partial response + degraded sources (design-docs/19 §7.3,
// D8).
//
// A typed query port can fail independently: a codegraph sidecar times out, a
// Qdrant shard is down, a reranker model is unavailable. The Broker does NOT
// treat a port failure as "no knowledge" — it records a DegradedSource and
// returns a partial response so the caller can distinguish three states
// (§7.3 / §11.5, the Phase 6 acceptance gate "三状态可区分"):
//
//   - HasResults          — real results were returned.
//   - IsEmptyAuthorized   — candidates existed but every one was filtered out
//                            by the post-check authorization pass (the caller
//                            had no use on any returned asset).
//   - IsEmptyNoResults    — every port returned empty AND no port failed: the
//                            knowledge base genuinely has nothing for this
//                            query.
//   - IsProviderDown      — at least one port failed AND no candidate survived:
//                            the empty result is a provider fault, not "no
//                            knowledge". The caller must NOT conclude the asset
//                            does not exist.
//
// The distinction is the difference between "I am not allowed to see this" and
// "the knowledge base is broken right now" — collapsing them is a leak
// (§11.4 "不得把失败解释为「没有知识」"). The ContextResponse carries the
// DegradedSources slice and the three-state methods so the handler can map to
// the §11.5 error semantics without re-deriving the state.
package context

import (
	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// DegradedReason is why a typed port did not contribute candidates (§7.3).
// The values align with the §7.3 reason vocabulary so the
// knowledge_context_degraded_total{reason} metric labels are stable.
type DegradedReason string

const (
	// DegradedProviderTimeout: the port call exceeded the shared deadline
	// (§7.1 step 4 common deadline = min(q.Timeout, 2s)). CodeGraph timeouts
	// do NOT block document results (§7.1 / §9.6).
	DegradedProviderTimeout DegradedReason = "provider_timeout"
	// DegradedProviderError: the port returned a non-timeout error (engine
	// fault, parse failure, unauthorized-engine blow-up). The candidate set
	// from this port is dropped; other ports are unaffected (§7.1).
	DegradedProviderError DegradedReason = "provider_error"
	// DegradedQdrantUnavailable: the vector store was unreachable; the
	// Document/Memory port fell back to FTS-only (§15). Fallback is NOT a
	// hard failure — the port still returns candidates — but the Broker
	// records the degradation so the caller knows ranking is BM25-only.
	DegradedQdrantUnavailable DegradedReason = "qdrant_unavailable"
	// DegradedFTSFallback: the port served FTS results instead of hybrid
	// (vector+BM25). Recorded alongside DegradedQdrantUnavailable when the
	// fallback succeeded, or standalone when the reranker path dropped to FTS.
	DegradedFTSFallback DegradedReason = "fts_fallback"
	// DegradedRerankUnavailable: the reranker model was unavailable; the port
	// kept the RRF-fused ordering without re-ranking (§7.3 "Reranker 不可用 →
	// 保留融合排序，不阻断"). Not a hard failure — candidates still return.
	DegradedRerankUnavailable DegradedReason = "rerank_unavailable"
)

// DegradedSource is one typed port that did not contribute its full result
// (§7.3). Detail is a redacted summary (no body, no credentials, §9.3 audit).
type DegradedSource struct {
	AssetType domain.AssetType `json:"asset_type"`
	Reason    DegradedReason   `json:"reason"`
	Detail    string           `json:"detail,omitempty"`
}

// ContextResponse is the Broker's delivery shape (§7.3 / §11.3). It carries
// the selected candidates, the degraded sources, the truncation report, the
// resolved intent + policy_version + authz_revision + decision_id, and the
// three-state methods the handler maps to §11.5 error semantics.
//
// Field names align with the §11.3 response example so the JSON shape is the
// design-doc shape (candidates / degraded_sources / truncation / intent /
// policy_version / authz_revision / decision_id).
type ContextResponse struct {
	Candidates       []KnowledgeCandidate `json:"candidates"`
	DegradedSources  []DegradedSource     `json:"degraded_sources,omitempty"`
	Truncation       TruncationReport     `json:"truncation,omitempty"`
	Intent           Intent               `json:"intent"`
	PolicyVersion    int                  `json:"policy_version"`
	AuthzRevision    int64                `json:"authz_revision"`
	DecisionID       *uuid.UUID           `json:"decision_id,omitempty"`

	// preCheckCandidateCount is the number of candidates that survived the
	// fetch + dedup but BEFORE the post-check authorization pass. It lets
	// IsEmptyAuthorized distinguish "candidates existed but authz denied them
	// all" from "no candidates were ever fetched" (IsEmptyNoResults). Set by
	// the Broker; not serialized (internal state for the three-state methods).
	preCheckCandidateCount int
}

// HasResults reports whether real results survived the pipeline (§7.3). True
// when at least one candidate made it through dedup + post-check + budget.
func (r ContextResponse) HasResults() bool { return len(r.Candidates) > 0 }

// IsEmptyAuthorized reports the "authorization filtered to empty" state
// (§7.3): candidates existed (preCheckCandidateCount > 0) but the post-check
// denied every one, leaving zero selected. This is NOT "no knowledge" — the
// assets exist, the caller just has no use on any of them. Leak-safe: the
// handler surfaces this as an empty result, never a 403 (§11.5).
func (r ContextResponse) IsEmptyAuthorized() bool {
	return len(r.Candidates) == 0 && r.preCheckCandidateCount > 0 && len(r.DegradedSources) == 0
}

// IsEmptyNoResults reports the "genuinely no knowledge" state (§7.3): every
// port returned empty, no port failed, no candidates were ever fetched. This
// is the only state where telling the caller "nothing exists" is accurate.
func (r ContextResponse) IsEmptyNoResults() bool {
	return len(r.Candidates) == 0 && r.preCheckCandidateCount == 0 && len(r.DegradedSources) == 0
}

// IsProviderDown reports the "provider fault" state (§7.3): at least one port
// failed (DegradedSources non-empty) AND no candidate survived. The caller
// must NOT conclude the asset does not exist — the empty result is a provider
// fault, not "no knowledge" (§7.3 / §11.4).
func (r ContextResponse) IsProviderDown() bool {
	return len(r.Candidates) == 0 && len(r.DegradedSources) > 0
}

// NewAuthorizedEmptyResponse constructs the "authorization filtered to empty"
// state (§7.3 / §11.5): candidates existed (preCheckCount > 0) but the post-
// check denied every one, leaving zero selected. This is the shape the Broker
// produces at step 5 when authz filters the fetched pool to nothing — leak-safe
// (surfaced to the caller as an empty result, never a 403).
//
// preCheckCandidateCount is unexported on ContextResponse, so callers outside
// this package (notably the eval runner's fake broker, §9.1) cannot construct
// the authorized-empty shape by struct literal. This constructor is the only
// way to build it from outside; it keeps the field write inside the package
// that owns the three-state invariant.
func NewAuthorizedEmptyResponse(intent Intent, preCheckCount int) ContextResponse {
	return ContextResponse{
		Intent:                 intent,
		preCheckCandidateCount: preCheckCount,
	}
}
