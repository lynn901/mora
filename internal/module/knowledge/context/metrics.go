// Package context — Phase 6 online quality metrics emission (design-docs/19
// §9.2, 12 §14.1 extension).
//
// The seven §9.2 Prometheus metrics are REGISTERED centrally in
// internal/platform/observ/metrics.go (promauto, same pattern as the MCP/RAG
// metrics). This file defines the Broker-facing emission seam: a
// MetricsEmitter interface the Broker calls at the natural §7.1 phase
// boundaries. The default is a no-op (nil-safe) so the Broker unit tests —
// which construct the Broker without WithMetrics — run unchanged and
// metric-free; the wiring layer (cmd/mora-api) installs the real emitter via
// WithMetrics so production emits against the registered collectors.
//
// Scope (YS-225): instrumentation only. The emitter calls do NOT alter the
// ten-step pipeline's control flow, order, or return values — they observe
// durations/counts alongside the existing audit path (§9.3). Recall is NEVER
// aggregated into a single number on this surface (§9.1/§9.2): candidates and
// tokens are labeled by type; the empty_total metric carries the §7.3
// three-state label so a fault is observable as distinct from "no knowledge"
// (§0 D7/D8, §11.4).
package context

import (
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/observ"
)

// MetricsPhase labels a §7.1 pipeline phase for the duration histogram. The
// values are the §9.2 route vocabulary: intent / authz / parallel / dedup /
// budget / citation. The P95 of the sum is the §14.3 end-to-end SLO (≤ 2s).
type MetricsPhase string

const (
	MetricsPhaseIntent    MetricsPhase = "intent"
	MetricsPhaseAuthz     MetricsPhase = "authz"
	MetricsPhaseParallel  MetricsPhase = "parallel"
	MetricsPhaseDedup     MetricsPhase = "dedup"
	MetricsPhaseBudget    MetricsPhase = "budget"
	MetricsPhaseCitation  MetricsPhase = "citation"
)

// MetricsDisposition labels a candidate outcome (§9.2). selected = survived
// the budget; truncated = scored in but dropped by the budget;
// denied = dropped by the §7.1 step-5 post-check authorization pass.
type MetricsDisposition string

const (
	MetricsDispSelected  MetricsDisposition = "selected"
	MetricsDispTruncated MetricsDisposition = "truncated"
	MetricsDispDenied    MetricsDisposition = "denied"
)

// MetricsEmptyState labels the §7.3 three-state for the empty_total metric.
// The three-state distinguishability is the Phase 6 acceptance gate (§0 D7/D8,
// §11.4): a provider fault must never be reported as "no knowledge".
type MetricsEmptyState string

const (
	MetricsEmptyProviderDown     MetricsEmptyState = "provider_down"
	MetricsEmptyAuthorized       MetricsEmptyState = "empty_authorized"
	MetricsEmptyNoResults        MetricsEmptyState = "empty_no_results"
)

// MetricsEmitter is the Broker-facing emission seam for the §9.2 metrics.
// Implementations MUST be safe for concurrent use (the Broker fans out ports
// concurrently; emission may interleave). A nil emitter is no-op — the Broker
// guards every call so the test path (no WithMetrics) runs metric-free.
//
// The methods are the seven §9.2 metrics:
//   - ObservePhaseDuration → knowledge_context_duration_seconds{route}
//   - ObserveTokens        → knowledge_context_tokens{type}
//   - IncCandidates        → knowledge_context_candidates_total{intent,type,disposition}
//   - IncDegraded          → knowledge_context_degraded_total{reason}
//   - IncEmptyState        → knowledge_context_empty_total{state}
//   - SetPolicyVersion     → knowledge_context_policy_version (gauge)
//   - IncAuthzDenied       → knowledge_authz_denied_total{action,type}
type MetricsEmitter interface {
	ObservePhaseDuration(phase MetricsPhase, d time.Duration)
	ObserveTokens(assetType domain.AssetType, tokens int)
	IncCandidates(intent Intent, assetType domain.AssetType, disposition MetricsDisposition, n int)
	IncDegraded(reason DegradedReason)
	IncEmptyState(state MetricsEmptyState)
	SetPolicyVersion(version int)
	IncAuthzDenied(action domain.Action, targetType domain.TargetType)
}

// promMetricsEmitter is the production MetricsEmitter: it forwards to the
// centrally-registered observ collectors. It is stateless; one shared instance
// is fine because prometheus collectors are themselves concurrency-safe.
type promMetricsEmitter struct{}

// NewPromMetricsEmitter returns a MetricsEmitter backed by the
// internal/platform/observ collectors registered in metrics.go. The wiring
// layer (cmd/mora-api) installs this via WithMetrics.
func NewPromMetricsEmitter() MetricsEmitter { return promMetricsEmitter{} }

func (promMetricsEmitter) ObservePhaseDuration(phase MetricsPhase, d time.Duration) {
	observ.KnowledgeContextDuration.WithLabelValues(string(phase)).Observe(d.Seconds())
}

func (promMetricsEmitter) ObserveTokens(assetType domain.AssetType, tokens int) {
	observ.KnowledgeContextTokens.WithLabelValues(string(assetType)).Observe(float64(tokens))
}

func (promMetricsEmitter) IncCandidates(intent Intent, assetType domain.AssetType, disposition MetricsDisposition, n int) {
	if n <= 0 {
		return
	}
	observ.KnowledgeContextCandidatesTotal.WithLabelValues(string(intent), string(assetType), string(disposition)).
		Add(float64(n))
}

func (promMetricsEmitter) IncDegraded(reason DegradedReason) {
	observ.KnowledgeContextDegradedTotal.WithLabelValues(string(reason)).Inc()
}

func (promMetricsEmitter) IncEmptyState(state MetricsEmptyState) {
	observ.KnowledgeContextEmptyTotal.WithLabelValues(string(state)).Inc()
}

func (promMetricsEmitter) SetPolicyVersion(version int) {
	observ.KnowledgeContextPolicyVersion.Set(float64(version))
}

func (promMetricsEmitter) IncAuthzDenied(action domain.Action, targetType domain.TargetType) {
	observ.KnowledgeAuthzDeniedTotal.WithLabelValues(string(action), string(targetType)).Inc()
}

// noopMetricsEmitter is the no-op used when no emitter is wired. It is the
// test path: the Broker unit tests construct without WithMetrics, so they run
// metric-free and do not depend on the prometheus registry. Kept unexported;
// the Broker treats a nil emitter as this no-op (emitIf guards).
type noopMetricsEmitter struct{}

func (noopMetricsEmitter) ObservePhaseDuration(MetricsPhase, time.Duration) {}
func (noopMetricsEmitter) ObserveTokens(domain.AssetType, int)              {}
func (noopMetricsEmitter) IncCandidates(Intent, domain.AssetType, MetricsDisposition, int) {}
func (noopMetricsEmitter) IncDegraded(DegradedReason)                       {}
func (noopMetricsEmitter) IncEmptyState(MetricsEmptyState)                  {}
func (noopMetricsEmitter) SetPolicyVersion(int)                             {}
func (noopMetricsEmitter) IncAuthzDenied(domain.Action, domain.TargetType)  {}
