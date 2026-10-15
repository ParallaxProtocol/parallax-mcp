// Package metrics holds the Prometheus collectors. They are registered on a
// private registry served only when --metrics-addr is set, so a deployment
// without it pays nothing but a few counter increments.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Registry holds every collector below.
	Registry = prometheus.NewRegistry()

	toolCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "parallax_mcp_tool_calls_total",
		Help: "Tool calls by tool and outcome (ok, error, partial).",
	}, []string{"tool", "outcome"})

	toolDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parallax_mcp_tool_duration_seconds",
		Help:    "Tool call latency.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"tool"})

	backendRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "parallax_mcp_backend_requests_total",
		Help: "Requests to backends (rpc, explorer, status) by outcome (ok, error, retry, cache_hit, denied).",
	}, []string{"backend", "outcome"})

	backendDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "parallax_mcp_backend_duration_seconds",
		Help:    "Backend request latency, retries included.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 15},
	}, []string{"backend"})

	rateLimited = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "parallax_mcp_http_rate_limited_total",
		Help: "HTTP requests refused by the per-client rate limit.",
	})
)

func init() {
	Registry.MustRegister(toolCalls, toolDuration, backendRequests, backendDuration, rateLimited,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

// ToolCall records one tool call.
func ToolCall(tool, outcome string, d time.Duration) {
	toolCalls.WithLabelValues(tool, outcome).Inc()
	toolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// Backend records one backend request outcome.
func Backend(backend, outcome string) { backendRequests.WithLabelValues(backend, outcome).Inc() }

// BackendLatency records how long a backend request took.
func BackendLatency(backend string, d time.Duration) {
	backendDuration.WithLabelValues(backend).Observe(d.Seconds())
}

// RateLimited records a refused HTTP request.
func RateLimited() { rateLimited.Inc() }

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}
