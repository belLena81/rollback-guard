package config

import "time"

// Listen addresses. Every service binds loopback by default (only
// the orchestrator authenticates, and only with AUTH_MODE=github); a deployment that must be reachable from another network
// namespace sets HTTP_ADDR explicitly.
const (
	DefaultOrchestratorAddr          = "127.0.0.1:8080"
	DefaultKubernetesReadMCPAddr     = "127.0.0.1:8091"
	DefaultKubernetesRollbackMCPAddr = "127.0.0.1:8092"
	DefaultObservabilityMCPAddr      = "127.0.0.1:8093"
	DefaultInvestigatorAddr          = "127.0.0.1:8094"
)

// Peer URLs follow from the default listen addresses.
const (
	mcpPath                         = "/mcp"
	defaultKubernetesReadMCPURL     = "http://" + DefaultKubernetesReadMCPAddr + mcpPath
	defaultKubernetesRollbackMCPURL = "http://" + DefaultKubernetesRollbackMCPAddr + mcpPath
	defaultObservabilityMCPURL      = "http://" + DefaultObservabilityMCPAddr + mcpPath
	defaultInvestigatorURL          = "http://" + DefaultInvestigatorAddr
	defaultPrometheusURL            = "http://127.0.0.1:9090"
)

// Targets and identities.
const (
	defaultAllowedTargets = "local/rollback-demo/demo-api/demo-api"
	targetFieldCount      = 4 // cluster/namespace/deployment/service
	defaultClusterID      = "local"
	defaultSystemActorID  = "rollback-guardian"
	defaultLocalActorID   = "local-admin"
	defaultA2AWorkerID    = "orchestrator-1"
)

// Kubernetes MCP modes.
const (
	KubernetesMCPModeReadonly = "readonly"
	KubernetesMCPModeRollback = "rollback"
)

// Database.
const (
	defaultDBMaxConns = 4
	// maxDBMaxConns bounds DB_MAX_CONNS so the value always fits pgx's int32.
	maxDBMaxConns        = 1000
	defaultDBPingTimeout = 2 * time.Second
)

// Workflow and policy.
const (
	defaultProposalTTL           = 15 * time.Minute
	defaultApprovalTTL           = 10 * time.Minute
	defaultCleanupTimeout        = 5 * time.Second
	defaultRuleCorrelationWindow = 10 * time.Minute
	defaultRuleMinConfidence     = 0.8
	defaultMetricsWindow         = 5 * time.Minute
)

// Recovery verification wait (RECOVERY_TIMEOUT, RECOVERY_POLL_INTERVAL).
// Right after a rollback the metrics window still holds the errors from
// before it, so by default verification keeps checking for one metrics window
// plus a margin. The cap keeps an approve request (rollout watch of at most
// domain.MaxRolloutWatch, its MCP calls, and this wait) inside
// httpserver.WriteTimeout (pinned by its tests).
const (
	defaultRecoveryMargin       = 30 * time.Second
	MaxRecoveryTimeout          = 7 * time.Minute
	defaultRecoveryPollInterval = 5 * time.Second
	minRecoveryPollInterval     = time.Second
	maxRecoveryPollInterval     = time.Minute
)

// MCP client and server. The metrics window and rollout watch bounds live in
// internal/domain because the adapters validate against them too.
const (
	defaultMCPConnectTimeout = 5 * time.Second
	MaxConnectTimeout        = 30 * time.Second
	defaultMCPCallTimeout    = 10 * time.Second
	MaxMCPCallTimeout        = 30 * time.Second
	defaultMCPWatchTimeout   = 90 * time.Second
	defaultMCPSessionTimeout = 5 * time.Minute
	// The Kubernetes API request timeout has to cover one full rollout watch.
	defaultMCPRequestTimeout = 2 * time.Minute
	maxMCPRequestTimeout     = 2 * time.Minute
)

// A2A.
const (
	defaultA2ATaskTimeout   = 45 * time.Second
	MaxA2ATaskTimeout       = 2 * time.Minute
	defaultA2ACancelTimeout = 5 * time.Second
	MaxA2ACancelTimeout     = 30 * time.Second
	defaultA2APollInterval  = 250 * time.Millisecond
	defaultA2AMaxAttempts   = 3
)

// Observability MCP.
const (
	defaultPrometheusTimeout = 5 * time.Second
	MaxPrometheusTimeout     = 30 * time.Second
	defaultMaxSampleAge      = time.Minute
	maxMaxSampleAge          = 15 * time.Minute
)

// LLM diagnosis.
const (
	defaultLLMTimeout         = 15 * time.Second
	maxLLMTimeout             = 30 * time.Second
	defaultLLMMaxOutputTokens = 512
	minLLMMaxOutputTokens     = 64
	maxLLMMaxOutputTokens     = 2048
	// An investigation makes this many evidence calls before reasoning.
	investigationEvidenceCallCount = 2
	minimumRuleFallbackBudget      = time.Second
)

// Telemetry.
const (
	defaultTelemetryShutdownTimeout = 5 * time.Second
	maxTelemetryShutdownTimeout     = 30 * time.Second
	defaultTraceSampleRatio         = 1.0
	defaultLogLevel                 = "info"
)
