// Package observ exposes Prometheus metrics for the MCP module. Metrics are
// registered once on init; the HTTP transport exposes /metrics for scraping
// (design doc 07 §4).
package observ

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// MCPRequestsTotal counts MCP JSON-RPC requests by method and outcome.
	MCPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_requests_total",
			Help: "Total MCP JSON-RPC requests by method and outcome.",
		},
		[]string{"method", "outcome"},
	)

	// MCPForbiddenTotal counts RBAC-forbidden tool/resource calls. Spikes here
	// trigger alerts (design doc 06 §7.1).
	MCPForbiddenTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_forbidden_total",
			Help: "Total MCP calls denied by RBAC (forbidden).",
		},
		[]string{"tool"},
	)

	// MCPToolDuration observes tool execution latency in seconds.
	MCPToolDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mcp_tool_duration_seconds",
			Help:    "MCP tool execution duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"tool"},
	)

	// MCPRateLimitedTotal counts requests rejected by the per-token limiter.
	MCPRateLimitedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_rate_limited_total",
			Help: "Total MCP requests rejected by per-token rate limiting.",
		},
		[]string{"bucket"},
	)

	// MCPSessionsGauge tracks the number of active MCP sessions.
	MCPSessionsGauge = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "mcp_sessions_active",
			Help: "Number of active MCP sessions.",
		},
	)

	// Multi-format document parsing (design-docs/10 §9.3).
	RagParseDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "rag_parse_duration_seconds",
			Help:    "Document parse duration by source format and parser.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"format", "parser"},
	)
	RagParseTasks = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "rag_parse_tasks",
			Help: "Active parse tasks by status.",
		},
		[]string{"status"},
	)
	RagParseDeadLetterTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "rag_parse_dead_letter_total",
			Help: "Total parse events moved to the dead-letter stream.",
		},
	)
	MoraParserCallDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mora_parser_call_duration_seconds",
			Help:    "mora-parser sidecar call duration by route.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route"},
	)

	// --- Phase 6 Context Broker online quality metrics (design-docs/19 §9.2,
	// 12 §14.1 extension). Registered here (centralized promauto, same pattern
	// as the MCP/RAG metrics above); the Broker path emits them. Metrics are
	// per-type / per-intent / per-state — recall is NEVER aggregated into a
	// single number (§9.1 / §9.2: a weak type must not be masked by a strong
	// one's mean). The empty_total{state} label covers all three states
	// (provider_down / empty_authorized / empty_no_results) so the §7.3
	// three-state acceptance gate is observable (§0 D7/D8). ---

	// KnowledgeContextDuration observes each §7.1 pipeline phase latency in
	// seconds, labeled by route. route ∈ {intent, authz, parallel, dedup,
	// budget, citation}. The P95 of the end-to-end sum is the §14.3 SLO (≤ 2s).
	KnowledgeContextDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "knowledge_context_duration_seconds",
			Help: "Context Broker pipeline phase latency by route (intent/authz/parallel/dedup/budget/citation).",
			// Tuned to the 2s SLO (12 §14.3): sub-phase budgets plus headroom.
			Buckets: []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		},
		[]string{"route"},
	)

	// KnowledgeContextTokens observes the token budget consumed per request,
	// labeled by asset type (document/codebase/memory/skill). A histogram (not
	// a counter) so p50/p95 spend is visible; the budget guard itself lives in
	// the Budgeter (§6.2), this observes the realized spend.
	KnowledgeContextTokens = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "knowledge_context_tokens",
			Help: "Context budget token consumption per request by asset type.",
			Buckets: []float64{16, 64, 128, 256, 512, 1024, 2048, 4096, 8192},
		},
		[]string{"type"},
	)

	// KnowledgeContextCandidatesTotal counts candidates by intent, asset type,
	// and disposition (selected / truncated / denied). selected = survived
	// budget; truncated = scored but dropped by budget; denied = dropped by the
	// post-check authorization pass. A counter of candidate outcomes.
	KnowledgeContextCandidatesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_candidates_total",
			Help: "Context Broker candidate outcomes by intent, type, and disposition (selected/truncated/denied).",
		},
		[]string{"intent", "type", "disposition"},
	)

	// KnowledgeContextDegradedTotal counts degraded sources by reason
	// (provider_timeout / provider_error / qdrant_unavailable / fts_fallback /
	// rerank_unavailable, §7.3 vocabulary). Spikes here trigger alerts.
	KnowledgeContextDegradedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_degraded_total",
			Help: "Context Broker degraded sources by reason (§7.3 vocabulary).",
		},
		[]string{"reason"},
	)

	// KnowledgeContextEmptyTotal counts empty-result responses by the §7.3
	// three-state: provider_down / empty_authorized / empty_no_results. The
	// three-state distinguishability is the Phase 6 acceptance gate (§0 D7/D8,
	// §11.4) — this metric makes it observable so a fault is never confused
	// with "no knowledge".
	KnowledgeContextEmptyTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_context_empty_total",
			Help: "Context Broker empty-result responses by three-state (provider_down/empty_authorized/empty_no_results).",
		},
		[]string{"state"},
	)

	// KnowledgeContextPolicyVersion is a gauge of the currently applied
	// authority policy version (§5.3). 0 = built-in default (no DB row); >0 =
	// DB-sourced version. Tracks policy rollouts across the fleet.
	KnowledgeContextPolicyVersion = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "knowledge_context_policy_version",
			Help: "Currently applied Context Broker authority policy version (0 = built-in default).",
		},
	)

	// KnowledgeAuthzDeniedTotal counts authorization denials on the Broker path
	// by action and target type (reused from §14.1). action ∈ {read (workspace
	// membership gate), use (per-asset post-check)}; type ∈ {workspace,
	// document, codebase, memory, skill}. Leak-safe: a denial is counted but
	// never distinguished from "no knowledge" on the response (§8.2).
	KnowledgeAuthzDeniedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "knowledge_authz_denied_total",
			Help: "Context Broker authorization denials by action and target type.",
		},
		[]string{"action", "type"},
	)
)
