// Package context — Prometheus metrics for the Context Broker (design-docs/19
// §9.2, doc 12 §14.1). These are the runtime SLO/quality instruments that
// complement the offline eval case set (§9.1): the case set locks release
// thresholds; these metrics observe live behavior.
//
// Metrics are registered once via promauto (same precedent as
// platform/observ/metrics.go). The HTTP /metrics endpoint (exposed by the
// observ package) scrapes them. The Broker instruments Execute (steps 1-10)
// and the handler instruments the authz-denied path.
//
// The seven metrics (§9.2 table):
//
//	knowledge_context_duration_seconds{route}   — Histogram, Broker stage timing
//	  (routes: intent / authz / parallel / dedup / budget / citation / total).
//	knowledge_context_tokens{type}              — Histogram, token spend by type.
//	knowledge_context_candidates_total{intent,type,disposition}
//	                                            — Counter, candidate
//	  selected/truncated/denied counts by intent+type+disposition.
//	knowledge_context_degraded_total{reason}    — Counter, degradation reason.
//	knowledge_context_empty_total{state}        — Counter, three-state counts
//	  (provider_down / empty_authorized / empty_no_results).
//	knowledge_context_policy_version            — Gauge, current policy version.
//	knowledge_authz_denied_total{action,type}   — Counter, authz denials
//	  (reuses §14.1; labeled by action + asset type).
//
// Label values are stable strings (the DegradedReason / TruncationReason /
// Intent enums stringize to these). A nil metrics set is never constructed —
// the package-level vars are the production instruments; a Broker test injects
// a no-op set via the Metrics field (so unit tests do not depend on the global
// registry).
package context

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/lynn901/mora/internal/domain"
)

// Stage route labels for knowledge_context_duration_seconds (§9.2). Each is one
// timed segment of the §7.1 ten-step pipeline; "total" is the end-to-end
// Execute duration.
const (
	stageIntent   = "intent"   // §7.1 step 3 (IntentRouter.Route)
	stageAuthz    = "authz"    // §7.1 steps 1-2 + step 5 (Authorize + post-check)
	stageParallel = "parallel" // §7.1 step 4 (typed-port fan-out)
	stageDedup    = "dedup"    // §7.1 step 6 (DedupAndKeepConflicts)
	stageBudget   = "budget"   // §7.1 step 8 (Budgeter.Select)
	stageCitation = "citation" // §7.1 step 9 (CitationBuilder.Build)
	stageTotal    = "total"    // end-to-end Execute
)

// EmptyState labels for knowledge_context_empty_total (§7.3 three states).
const (
	emptyProviderDown      = "provider_down"
	emptyAuthorized        = "empty_authorized"
	emptyNoResults         = "empty_no_results"
)

// CandidateDisposition labels for knowledge_context_candidates_total (§9.2).
const (
	dispositionSelected  = "selected"  // survived the budget
	dispositionTruncated = "truncated" // scored in but cut by the budget
	dispositionDenied    = "denied"    // dropped by the authz post-check
)

var (
	// ContextDurationSeconds observes each Broker stage's duration in seconds
	// (§9.2). The "total" route is the end-to-end Execute latency (the P95
	// gate ≤ 2s, §14.3 SLO, is read off this label).
	ContextDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "knowledge_context_duration_seconds",
			Help:    "Context Broker stage duration in seconds, by pipeline route.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14), // 1ms → ~8.2s
		},
		[]string{"route"},
	)

	// ContextTokens observes the token budget consumed per asset type (§9.2).
	// The Budgeter reports the selected-slice token spend; the histogram lets
	// SRE see the per-type spend distribution (a type routinely hitting its
	// TokenShare cap is a tuning signal).
	ContextTokens = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "knowledge_context_tokens",
			Help:    "Context budget token spend per asset type.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 14), // 1 → ~8192 tokens
		},
		[]string{"type"},
	)

	// ContextCandidatesTotal counts candidates by intent + type + disposition
	// (§9.2). disposition ∈ {selected, truncated, denied}: selected survived
	// the budget, truncated scored in but were cut, denied were dropped by the
	// authz post-check. The truncated/denied counts are the budget + leak
	// signals (§12 gates).
	ContextCandidatesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_candidates_total",
			Help: "Context Broker candidates by intent, asset type, and disposition (selected/truncated/denied).",
		},
		[]string{"intent", "type", "disposition"},
	)

	// ContextDegradedTotal counts port degradations by reason (§9.2 / §7.3).
	// The reason values are the DegradedReason enum strings
	// (provider_timeout/provider_error/qdrant_unavailable/fts_fallback/
	// rerank_unavailable). A spike in provider_timeout is the CodeGraph-sidecar
	// SLO signal (§9.6).
	ContextDegradedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_degraded_total",
			Help: "Context Broker typed-port degradations by reason.",
		},
		[]string{"reason"},
	)

	// ContextEmptyTotal counts the three empty states (§7.3 / §9.2). The three
	// labels are mutually exclusive per response; the counts let SRE
	// distinguish "provider broken" from "authz locked the caller out" from
	// "genuinely no knowledge" — the §11.4 leak-prevention invariant.
	ContextEmptyTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_empty_total",
			Help: "Context Broker empty-response states (provider_down/empty_authorized/empty_no_results).",
		},
		[]string{"state"},
	)

	// ContextPolicyVersion is the current authority policy version per
	// (workspace, intent) (§9.2 / §5.3). Set on each Execute that loads a
	// policy; policy_version=0 is the built-in default (no DB row). A step
	// change signals a PM governance update (§5.3).
	ContextPolicyVersion = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "knowledge_context_policy_version",
			Help: "Current authority policy version by workspace and intent (0 = built-in default).",
		},
		[]string{"workspace_id", "intent"},
	)

	// AuthzDeniedTotal counts authorization denials the Broker observed (§9.2,
	// reusing §14.1's authz-denied metric). Labeled by action + asset type so a
	// spike on one type (e.g. memory) is distinguishable. Incremented at the
	// post-check (step 5) for each candidate the VisibleAssets pass denied, and
	// at the workspace gate (step 2) when a non-member is denied. Leak-safe:
	// the counter records the DENIAL, never the asset id (§13.4 no body / no
	// existence leak).
	AuthzDeniedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_authz_denied_total",
			Help: "Authorization denials observed by the Context Broker, by action and asset type.",
		},
		[]string{"action", "type"},
	)
)

// Metrics is the instrumentation seam a Broker uses to record the §9.2 metrics.
// The production implementation is the package-level promauto instruments above
// (defaultMetrics); a unit test injects a no-op (NilMetrics) so the broker test
// does not depend on the global Prometheus registry. The Broker holds a
// *Metrics; nil → no instrumentation (the test path that predates metrics).
//
// The methods take the primitive values (not the full structs) so the call
// sites stay readable and the seam is mockable without constructing whole
// ContextResponse objects.
type Metrics struct {
	// ObserveStage records one pipeline stage's duration (§9.2 duration).
	ObserveStage func(route string, d time.Duration)
	// ObserveTokens records the token spend for one asset type (§9.2 tokens).
	ObserveTokens func(assetType domain.AssetType, tokens int)
	// IncCandidate records one candidate's disposition (§9.2 candidates_total).
	IncCandidate func(intent Intent, assetType domain.AssetType, disposition string)
	// IncDegraded records one degraded source (§9.2 degraded_total).
	IncDegraded func(reason DegradedReason)
	// IncEmpty records one empty-response state (§9.2 empty_total).
	IncEmpty func(state string)
	// SetPolicyVersion records the loaded policy version (§9.2 policy_version).
	SetPolicyVersion func(workspaceID string, intent Intent, version int)
	// IncAuthzDenied records one authz denial (§9.2 authz_denied_total).
	IncAuthzDenied func(action string, assetType domain.AssetType)
}

// DefaultMetrics returns the production Metrics wired to the package-level
// promauto instruments. The Broker installs it via WithMetrics unless a test
// injects NilMetrics.
func DefaultMetrics() *Metrics { return defaultMetrics() }

// defaultMetrics wires the Metrics seam to the package-level prometheus
// instruments. Returned by DefaultMetrics(); the Broker installs it unless a
// test injects NilMetrics.
func defaultMetrics() *Metrics {
	return &Metrics{
		ObserveStage: func(route string, d time.Duration) {
			ContextDurationSeconds.WithLabelValues(route).Observe(d.Seconds())
		},
		ObserveTokens: func(t domain.AssetType, tokens int) {
			ContextTokens.WithLabelValues(string(t)).Observe(float64(tokens))
		},
		IncCandidate: func(intent Intent, t domain.AssetType, disposition string) {
			ContextCandidatesTotal.WithLabelValues(string(intent), string(t), disposition).Inc()
		},
		IncDegraded: func(reason DegradedReason) {
			ContextDegradedTotal.WithLabelValues(string(reason)).Inc()
		},
		IncEmpty: func(state string) {
			ContextEmptyTotal.WithLabelValues(state).Inc()
		},
		SetPolicyVersion: func(wsID string, intent Intent, version int) {
			ContextPolicyVersion.WithLabelValues(wsID, string(intent)).Set(float64(version))
		},
		IncAuthzDenied: func(action string, t domain.AssetType) {
			AuthzDeniedTotal.WithLabelValues(action, string(t)).Inc()
		},
	}
}

// NilMetrics is a no-op Metrics for the Broker unit-test path (so broker tests
// do not touch the global Prometheus registry). Every method is a no-op.
var NilMetrics = &Metrics{
	ObserveStage:     func(string, time.Duration) {},
	ObserveTokens:    func(domain.AssetType, int) {},
	IncCandidate:     func(Intent, domain.AssetType, string) {},
	IncDegraded:      func(DegradedReason) {},
	IncEmpty:         func(string) {},
	SetPolicyVersion: func(string, Intent, int) {},
	IncAuthzDenied:   func(string, domain.AssetType) {},
}
