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
)
