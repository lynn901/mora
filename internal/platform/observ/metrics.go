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
)
