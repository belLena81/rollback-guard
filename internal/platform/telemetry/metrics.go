package telemetry

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
)

const namespace = "rollback_guardian"

// Every label below takes values from a closed set (route patterns, tool
// catalog names, enum strings). Identifiers such as incident IDs, keys,
// hashes, and free text are never labels.
type metrics struct {
	httpRequests          *prometheus.CounterVec
	httpDuration          *prometheus.HistogramVec
	incidentsCreated      prometheus.Counter
	incidentsTerminal     *prometheus.CounterVec
	approvalDecisions     *prometheus.CounterVec
	dispatches            *prometheus.CounterVec
	dispatchDuration      *prometheus.HistogramVec
	a2aCalls              *prometheus.CounterVec
	a2aDuration           *prometheus.HistogramVec
	mcpCalls              *prometheus.CounterVec
	mcpCallDuration       *prometheus.HistogramVec
	mcpTools              *prometheus.CounterVec
	mcpToolDuration       *prometheus.HistogramVec
	diagnoses             *prometheus.CounterVec
	rollbackExecutions    *prometheus.CounterVec
	recoveryVerifications *prometheus.CounterVec
	retries               *prometheus.CounterVec
	breakerTransitions    *prometheus.CounterVec
}

var (
	latencyBuckets       = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
	investigationBuckets = []float64{0.5, 1, 2.5, 5, 10, 20, 30, 45, 60, 90, 120, 300}
)

func newMetrics(registerer prometheus.Registerer) (*metrics, error) {
	m := &metrics{
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_server_requests_total",
			Help: "HTTP requests served, by method, route pattern, and status code."}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "http_server_request_duration_seconds",
			Help: "HTTP request handling time, by method, route pattern, and status code.", Buckets: latencyBuckets}, []string{"method", "route", "code"}),
		incidentsCreated: prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "incidents_total",
			Help: "Incidents created (committed)."}),
		incidentsTerminal: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "incidents_terminal_total",
			Help: "Incidents that reached a terminal status (committed), by status."}, []string{"status"}),
		approvalDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "approval_decisions_total",
			Help: "Human rollback decisions recorded (committed), by decision."}, []string{"decision"}),
		dispatches: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "investigation_dispatches_total",
			Help: "Investigation dispatches that reached a terminal state, by outcome and failure reason."}, []string{"outcome", "reason"}),
		dispatchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "investigation_duration_seconds",
			Help: "Time from investigation dispatch creation to its terminal state, by outcome.", Buckets: investigationBuckets}, []string{"outcome"}),
		a2aCalls: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "a2a_calls_total",
			Help: "A2A calls to the Investigator agent, by operation and outcome."}, []string{"operation", "outcome"}),
		a2aDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "a2a_call_duration_seconds",
			Help: "A2A call time, by operation and outcome.", Buckets: latencyBuckets}, []string{"operation", "outcome"}),
		mcpCalls: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "mcp_calls_total",
			Help: "MCP tool calls made by this process, by server, tool, and outcome."}, []string{"server", "tool", "outcome"}),
		mcpCallDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "mcp_call_duration_seconds",
			Help: "MCP tool call time seen by the caller, by server, tool, and outcome.", Buckets: latencyBuckets}, []string{"server", "tool", "outcome"}),
		mcpTools: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "mcp_tool_requests_total",
			Help: "MCP tool calls handled by this server, by server, tool, and outcome."}, []string{"server", "tool", "outcome"}),
		mcpToolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "mcp_tool_request_duration_seconds",
			Help: "MCP tool handling time on the server, by server, tool, and outcome.", Buckets: latencyBuckets}, []string{"server", "tool", "outcome"}),
		diagnoses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "diagnoses_total",
			Help:      "Diagnoses produced, by requested strategy, effective strategy, and fallback reason.",
		}, []string{"requested_strategy", "effective_strategy", "fallback_reason"}),
		rollbackExecutions: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "rollback_executions_total",
			Help: "Rollback executions completed (committed), by outcome."}, []string{"outcome"}),
		recoveryVerifications: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "recovery_verifications_total",
			Help: "Recovery verifications completed (committed), by result."}, []string{"result"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "retries_total",
			Help: "Retries of calls to external dependencies, by dependency and outcome (retried, exhausted)."}, []string{"dependency", "outcome"}),
		breakerTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "circuit_breaker_transitions_total",
			Help: "Circuit breaker state changes, by dependency and the state entered (open, half_open, closed)."}, []string{"dependency", "state"}),
	}
	for _, collector := range []prometheus.Collector{
		m.httpRequests, m.httpDuration, m.incidentsCreated, m.incidentsTerminal, m.approvalDecisions,
		m.dispatches, m.dispatchDuration, m.a2aCalls, m.a2aDuration, m.mcpCalls, m.mcpCallDuration,
		m.mcpTools, m.mcpToolDuration, m.diagnoses, m.rollbackExecutions, m.recoveryVerifications,
		m.retries, m.breakerTransitions,
	} {
		if err := registerer.Register(collector); err != nil {
			return nil, fmt.Errorf("register metrics collector: %w", err)
		}
	}
	return m, nil
}
