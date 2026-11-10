package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

type LLMProvider string

const LLMProviderOpenAICompatible LLMProvider = "openai_compatible"

type Config struct {
	HTTPAddr              string
	DatabaseURL           string
	DBMaxConns            int32
	DBPingTimeout         time.Duration
	ProposalTTL           time.Duration
	ApprovalTTL           time.Duration
	CleanupTimeout        time.Duration
	SystemActorID         domain.ActorID
	LocalActorID          domain.ActorID
	RuleMinimumConfidence float64
	RuleCorrelationWindow time.Duration
	AllowedTargets        []domain.DeploymentTarget
	KubernetesReadMCPURL  string
	KubernetesWriteMCPURL string
	ObservabilityMCPURL   string
	MCPConnectTimeout     time.Duration
	MCPCallTimeout        time.Duration
	MCPWatchTimeout       time.Duration
	MetricsWindow         time.Duration
	RecoveryTimeout       time.Duration
	RecoveryPollInterval  time.Duration
	InvestigatorURL       string
	A2AConnectTimeout     time.Duration
	A2ATaskTimeout        time.Duration
	A2ACancelTimeout      time.Duration
	A2APollInterval       time.Duration
	A2ALeaseTTL           time.Duration
	A2AWorkerID           string
	A2AMaxAttempts        int
	Telemetry             TelemetryConfig
	Resilience            ResilienceConfig
}

type InvestigatorConfig struct {
	HTTPAddr              string
	PublicURL             string
	DatabaseURL           string
	DBMaxConns            int32
	DBPingTimeout         time.Duration
	KubernetesReadMCPURL  string
	ObservabilityMCPURL   string
	MCPConnectTimeout     time.Duration
	MCPCallTimeout        time.Duration
	TaskTimeout           time.Duration
	MetricsWindow         time.Duration
	RuleCorrelationWindow time.Duration
	DiagnosisStrategy     domain.DiagnosisStrategy
	LLMBaseURL            string
	LLMModel              string
	LLMAPIKey             string
	LLMTimeout            time.Duration
	LLMMaxOutputTokens    int
	Telemetry             TelemetryConfig
	Resilience            ResilienceConfig
}

type KubernetesMCPConfig struct {
	HTTPAddr       string
	Mode           string
	ClusterID      string
	Kubeconfig     string
	DatabaseURL    string
	DBMaxConns     int32
	DBPingTimeout  time.Duration
	AllowedTargets []domain.DeploymentTarget
	RequestTimeout time.Duration
	SessionTimeout time.Duration
	Telemetry      TelemetryConfig
	Resilience     ResilienceConfig
}

type ObservabilityMCPConfig struct {
	HTTPAddr       string
	PrometheusURL  string
	AllowedTargets []domain.DeploymentTarget
	RequestTimeout time.Duration
	SessionTimeout time.Duration
	MetricsWindow  time.Duration
	MaxSampleAge   time.Duration
	Telemetry      TelemetryConfig
	Resilience     ResilienceConfig
}

// Load reads the orchestrator configuration.
func Load() (Config, error) {
	workflow, err := loadWorkflow()
	if err != nil {
		return Config{}, err
	}
	db, err := loadDB("")
	if err != nil {
		return Config{}, err
	}
	targets, err := loadTargets()
	if err != nil {
		return Config{}, err
	}
	mcp, err := loadMCPClient()
	if err != nil {
		return Config{}, err
	}
	mcpWatchTimeout, err := boundedDurationEnv("MCP_WATCH_TIMEOUT", defaultMCPWatchTimeout, domain.MinRolloutWatch, domain.MaxRolloutWatch)
	if err != nil {
		return Config{}, err
	}
	metricsWindow, err := loadMetricsWindow()
	if err != nil {
		return Config{}, err
	}
	recovery, err := loadRecoveryWait(metricsWindow)
	if err != nil {
		return Config{}, err
	}
	a2a, err := loadA2AClient(mcp.connectTimeout)
	if err != nil {
		return Config{}, err
	}
	telemetry, err := telemetryEnv("orchestrator")
	if err != nil {
		return Config{}, err
	}
	resilience, err := resilienceEnv()
	if err != nil {
		return Config{}, err
	}
	return Config{
		HTTPAddr:              stringEnv("HTTP_ADDR", DefaultOrchestratorAddr),
		DatabaseURL:           db.url,
		DBMaxConns:            db.maxConns,
		DBPingTimeout:         db.pingTimeout,
		ProposalTTL:           workflow.proposalTTL,
		ApprovalTTL:           workflow.approvalTTL,
		CleanupTimeout:        workflow.cleanupTimeout,
		SystemActorID:         domain.ActorID(stringEnv("SYSTEM_ACTOR_ID", defaultSystemActorID)),
		LocalActorID:          domain.ActorID(stringEnv("LOCAL_ACTOR_ID", defaultLocalActorID)),
		RuleMinimumConfidence: workflow.ruleMinimumConfidence,
		RuleCorrelationWindow: workflow.ruleCorrelationWindow,
		AllowedTargets:        targets,
		KubernetesReadMCPURL:  mcp.kubernetesReadURL,
		KubernetesWriteMCPURL: stringEnv("KUBERNETES_ROLLBACK_MCP_URL", defaultKubernetesRollbackMCPURL),
		ObservabilityMCPURL:   mcp.observabilityURL,
		MCPConnectTimeout:     mcp.connectTimeout,
		MCPCallTimeout:        mcp.callTimeout,
		MCPWatchTimeout:       mcpWatchTimeout,
		MetricsWindow:         metricsWindow,
		RecoveryTimeout:       recovery.timeout,
		RecoveryPollInterval:  recovery.pollInterval,
		InvestigatorURL:       stringEnv("INVESTIGATOR_URL", defaultInvestigatorURL),
		A2AConnectTimeout:     a2a.connectTimeout,
		A2ATaskTimeout:        a2a.taskTimeout,
		A2ACancelTimeout:      a2a.cancelTimeout,
		A2APollInterval:       a2a.pollInterval,
		A2ALeaseTTL:           a2a.leaseTTL,
		A2AWorkerID:           stringEnv("A2A_WORKER_ID", defaultA2AWorkerID),
		A2AMaxAttempts:        a2a.maxAttempts,
		Telemetry:             telemetry,
		Resilience:            resilience,
	}, nil
}

// LoadInvestigator reads the investigator configuration.
func LoadInvestigator() (InvestigatorConfig, error) {
	db, err := loadDB("")
	if err != nil {
		return InvestigatorConfig{}, err
	}
	mcp, err := loadMCPClient()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	taskTimeout, err := loadA2ATaskTimeout()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	metricsWindow, err := loadMetricsWindow()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	ruleWindow, err := durationEnv("RULE_CORRELATION_WINDOW", defaultRuleCorrelationWindow)
	if err != nil {
		return InvestigatorConfig{}, err
	}
	strategy, err := diagnosisStrategyEnv()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	telemetry, err := telemetryEnv("investigator")
	if err != nil {
		return InvestigatorConfig{}, err
	}
	resilience, err := resilienceEnv()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	llm, err := loadLLM(strategy, mcp.callTimeout, taskTimeout)
	if err != nil {
		return InvestigatorConfig{}, err
	}
	return InvestigatorConfig{
		HTTPAddr:              stringEnv("HTTP_ADDR", DefaultInvestigatorAddr),
		PublicURL:             stringEnv("INVESTIGATOR_PUBLIC_URL", defaultInvestigatorURL),
		DatabaseURL:           db.url,
		DBMaxConns:            db.maxConns,
		DBPingTimeout:         db.pingTimeout,
		KubernetesReadMCPURL:  mcp.kubernetesReadURL,
		ObservabilityMCPURL:   mcp.observabilityURL,
		MCPConnectTimeout:     mcp.connectTimeout,
		MCPCallTimeout:        mcp.callTimeout,
		TaskTimeout:           taskTimeout,
		MetricsWindow:         metricsWindow,
		RuleCorrelationWindow: ruleWindow,
		DiagnosisStrategy:     strategy,
		LLMBaseURL:            llm.baseURL,
		LLMModel:              llm.model,
		LLMAPIKey:             llm.apiKey,
		LLMTimeout:            llm.timeout,
		LLMMaxOutputTokens:    llm.maxOutputTokens,
		Telemetry:             telemetry,
		Resilience:            resilience,
	}, nil
}

// LoadKubernetesMCP reads the Kubernetes MCP configuration for either mode.
func LoadKubernetesMCP() (KubernetesMCPConfig, error) {
	targets, err := loadTargets()
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	requestTimeout, err := boundedDurationEnv("MCP_REQUEST_TIMEOUT", defaultMCPRequestTimeout, 0, maxMCPRequestTimeout)
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	sessionTimeout, err := durationEnv("MCP_SESSION_TIMEOUT", defaultMCPSessionTimeout)
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	mode := stringEnv("KUBERNETES_MCP_MODE", KubernetesMCPModeReadonly)
	if mode != KubernetesMCPModeReadonly && mode != KubernetesMCPModeRollback {
		return KubernetesMCPConfig{}, fmt.Errorf("KUBERNETES_MCP_MODE must be readonly or rollback")
	}
	clusterID := stringEnv("CLUSTER_ID", defaultClusterID)
	for _, target := range targets {
		if target.Cluster() != clusterID {
			return KubernetesMCPConfig{}, fmt.Errorf("ALLOWED_TARGETS must use CLUSTER_ID %q", clusterID)
		}
	}
	// Only rollback mode connects to PostgreSQL.
	defaultAddr := DefaultKubernetesReadMCPAddr
	var db dbSettings
	if mode == KubernetesMCPModeRollback {
		defaultAddr = DefaultKubernetesRollbackMCPAddr
		db, err = loadDB(" in rollback mode")
		if err != nil {
			return KubernetesMCPConfig{}, err
		}
	}
	telemetry, err := telemetryEnv("kubernetes-mcp-" + mode)
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	resilience, err := resilienceEnv()
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	return KubernetesMCPConfig{
		HTTPAddr:       stringEnv("HTTP_ADDR", defaultAddr),
		Mode:           mode,
		ClusterID:      clusterID,
		Kubeconfig:     strings.TrimSpace(os.Getenv("KUBECONFIG")),
		DatabaseURL:    db.url,
		DBMaxConns:     db.maxConns,
		DBPingTimeout:  db.pingTimeout,
		AllowedTargets: targets,
		RequestTimeout: requestTimeout,
		SessionTimeout: sessionTimeout,
		Telemetry:      telemetry,
		Resilience:     resilience,
	}, nil
}

// LoadObservabilityMCP reads the observability MCP configuration.
func LoadObservabilityMCP() (ObservabilityMCPConfig, error) {
	targets, err := loadTargets()
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	requestTimeout, err := boundedDurationEnv("PROMETHEUS_TIMEOUT", defaultPrometheusTimeout, 0, MaxPrometheusTimeout)
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	sessionTimeout, err := durationEnv("MCP_SESSION_TIMEOUT", defaultMCPSessionTimeout)
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	window, err := loadMetricsWindow()
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	maxAge, err := boundedDurationEnv("METRICS_MAX_SAMPLE_AGE", defaultMaxSampleAge, 0, maxMaxSampleAge)
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	telemetry, err := telemetryEnv("observability-mcp")
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	resilience, err := resilienceEnv()
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	return ObservabilityMCPConfig{
		HTTPAddr:       stringEnv("HTTP_ADDR", DefaultObservabilityMCPAddr),
		PrometheusURL:  stringEnv("PROMETHEUS_URL", defaultPrometheusURL),
		AllowedTargets: targets,
		RequestTimeout: requestTimeout,
		SessionTimeout: sessionTimeout,
		MetricsWindow:  window,
		MaxSampleAge:   maxAge,
		Telemetry:      telemetry,
		Resilience:     resilience,
	}, nil
}

type dbSettings struct {
	url         string
	maxConns    int32
	pingTimeout time.Duration
}

// loadDB reads the PostgreSQL settings. requiredFor is appended to the
// "DATABASE_URL is required" error, for example " in rollback mode".
func loadDB(requiredFor string) (dbSettings, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return dbSettings{}, fmt.Errorf("DATABASE_URL is required%s", requiredFor)
	}
	maxConns, err := intEnv("DB_MAX_CONNS", defaultDBMaxConns)
	if err != nil {
		return dbSettings{}, err
	}
	pingTimeout, err := durationEnv("DB_PING_TIMEOUT", defaultDBPingTimeout)
	if err != nil {
		return dbSettings{}, err
	}
	return dbSettings{url: databaseURL, maxConns: int32(maxConns), pingTimeout: pingTimeout}, nil
}

type mcpClientSettings struct {
	kubernetesReadURL string
	observabilityURL  string
	connectTimeout    time.Duration
	callTimeout       time.Duration
}

// loadMCPClient reads the settings shared by both MCP clients (orchestrator
// and investigator).
func loadMCPClient() (mcpClientSettings, error) {
	connectTimeout, err := boundedDurationEnv("MCP_CONNECT_TIMEOUT", defaultMCPConnectTimeout, 0, MaxConnectTimeout)
	if err != nil {
		return mcpClientSettings{}, err
	}
	callTimeout, err := boundedDurationEnv("MCP_CALL_TIMEOUT", defaultMCPCallTimeout, 0, MaxMCPCallTimeout)
	if err != nil {
		return mcpClientSettings{}, err
	}
	return mcpClientSettings{
		kubernetesReadURL: stringEnv("KUBERNETES_READ_MCP_URL", defaultKubernetesReadMCPURL),
		observabilityURL:  stringEnv("OBSERVABILITY_MCP_URL", defaultObservabilityMCPURL),
		connectTimeout:    connectTimeout,
		callTimeout:       callTimeout,
	}, nil
}

func loadTargets() ([]domain.DeploymentTarget, error) {
	return targetsEnv("ALLOWED_TARGETS", defaultAllowedTargets)
}

func loadMetricsWindow() (time.Duration, error) {
	return boundedDurationEnv("METRICS_WINDOW", defaultMetricsWindow, domain.MinMetricsWindow, domain.MaxMetricsWindow)
}

func loadA2ATaskTimeout() (time.Duration, error) {
	return boundedDurationEnv("A2A_TASK_TIMEOUT", defaultA2ATaskTimeout, 0, MaxA2ATaskTimeout)
}

type recoveryWaitSettings struct {
	timeout      time.Duration
	pollInterval time.Duration
}

// loadRecoveryWait reads how long recovery verification may keep checking.
// The default is one metrics window plus a margin, capped at
// MaxRecoveryTimeout. Both values must be positive.
func loadRecoveryWait(metricsWindow time.Duration) (recoveryWaitSettings, error) {
	timeout, err := boundedDurationEnv("RECOVERY_TIMEOUT", min(metricsWindow+defaultRecoveryMargin, MaxRecoveryTimeout), 0, MaxRecoveryTimeout)
	if err != nil {
		return recoveryWaitSettings{}, err
	}
	interval, err := boundedDurationEnv("RECOVERY_POLL_INTERVAL", defaultRecoveryPollInterval, minRecoveryPollInterval, maxRecoveryPollInterval)
	if err != nil {
		return recoveryWaitSettings{}, err
	}
	return recoveryWaitSettings{timeout: timeout, pollInterval: interval}, nil
}

type workflowSettings struct {
	proposalTTL           time.Duration
	approvalTTL           time.Duration
	cleanupTimeout        time.Duration
	ruleCorrelationWindow time.Duration
	ruleMinimumConfidence float64
}

func loadWorkflow() (workflowSettings, error) {
	proposalTTL, err := durationEnv("PROPOSAL_TTL", defaultProposalTTL)
	if err != nil {
		return workflowSettings{}, err
	}
	approvalTTL, err := durationEnv("APPROVAL_TTL", defaultApprovalTTL)
	if err != nil {
		return workflowSettings{}, err
	}
	cleanupTimeout, err := durationEnv("CLEANUP_TIMEOUT", defaultCleanupTimeout)
	if err != nil {
		return workflowSettings{}, err
	}
	ruleWindow, err := durationEnv("RULE_CORRELATION_WINDOW", defaultRuleCorrelationWindow)
	if err != nil {
		return workflowSettings{}, err
	}
	confidence, err := floatEnv("RULE_MIN_CONFIDENCE", defaultRuleMinConfidence)
	if err != nil {
		return workflowSettings{}, err
	}
	return workflowSettings{proposalTTL: proposalTTL, approvalTTL: approvalTTL, cleanupTimeout: cleanupTimeout, ruleCorrelationWindow: ruleWindow, ruleMinimumConfidence: confidence}, nil
}

type a2aClientSettings struct {
	connectTimeout time.Duration
	taskTimeout    time.Duration
	cancelTimeout  time.Duration
	pollInterval   time.Duration
	leaseTTL       time.Duration
	maxAttempts    int
}

// loadA2AClient reads the orchestrator's A2A settings. A2A_CONNECT_TIMEOUT
// used to be MCP_CONNECT_TIMEOUT, so that value stays its default.
func loadA2AClient(mcpConnectTimeout time.Duration) (a2aClientSettings, error) {
	connectTimeout, err := boundedDurationEnv("A2A_CONNECT_TIMEOUT", mcpConnectTimeout, 0, MaxConnectTimeout)
	if err != nil {
		return a2aClientSettings{}, err
	}
	taskTimeout, err := loadA2ATaskTimeout()
	if err != nil {
		return a2aClientSettings{}, err
	}
	cancelTimeout, err := boundedDurationEnv("A2A_CANCEL_TIMEOUT", defaultA2ACancelTimeout, 0, MaxA2ACancelTimeout)
	if err != nil {
		return a2aClientSettings{}, err
	}
	pollInterval, err := durationEnv("A2A_POLL_INTERVAL", defaultA2APollInterval)
	if err != nil {
		return a2aClientSettings{}, err
	}
	// The lease has to outlive the task and its cancellation cleanup.
	minimumLease := taskTimeout + cancelTimeout
	leaseTTL, err := durationEnv("A2A_LEASE_TTL", minimumLease)
	if err != nil {
		return a2aClientSettings{}, err
	}
	if leaseTTL < minimumLease {
		return a2aClientSettings{}, fmt.Errorf("A2A_LEASE_TTL must be at least A2A_TASK_TIMEOUT plus A2A_CANCEL_TIMEOUT (%s), got %s", formatDuration(minimumLease), formatDuration(leaseTTL))
	}
	maxAttempts, err := intEnv("A2A_MAX_ATTEMPTS", defaultA2AMaxAttempts)
	if err != nil {
		return a2aClientSettings{}, err
	}
	return a2aClientSettings{connectTimeout: connectTimeout, taskTimeout: taskTimeout, cancelTimeout: cancelTimeout, pollInterval: pollInterval, leaseTTL: leaseTTL, maxAttempts: maxAttempts}, nil
}

type llmSettings struct {
	provider        LLMProvider
	baseURL         string
	model           string
	apiKey          string
	timeout         time.Duration
	maxOutputTokens int
}

// loadLLM reads the model settings. They are validated only when the
// strategy uses a model, so a rule-based investigator ignores stale or
// invalid LLM_* values.
func loadLLM(strategy domain.DiagnosisStrategy, mcpCallTimeout, taskTimeout time.Duration) (llmSettings, error) {
	settings := llmSettings{
		provider:        LLMProvider(stringEnv("LLM_PROVIDER", string(LLMProviderOpenAICompatible))),
		baseURL:         strings.TrimSpace(os.Getenv("LLM_BASE_URL")),
		model:           strings.TrimSpace(os.Getenv("LLM_MODEL")),
		apiKey:          strings.TrimSpace(os.Getenv("LLM_API_KEY")),
		timeout:         defaultLLMTimeout,
		maxOutputTokens: defaultLLMMaxOutputTokens,
	}
	if strategy == domain.StrategyRules {
		return settings, nil
	}
	if settings.provider != LLMProviderOpenAICompatible {
		return llmSettings{}, fmt.Errorf("LLM_PROVIDER must be %q", LLMProviderOpenAICompatible)
	}
	loopback, err := validateLLMBaseURL(settings.baseURL)
	if err != nil {
		return llmSettings{}, err
	}
	if settings.model == "" {
		return llmSettings{}, fmt.Errorf("LLM_MODEL is required for model diagnosis")
	}
	if !loopback && settings.apiKey == "" {
		return llmSettings{}, fmt.Errorf("LLM_API_KEY is required for non-loopback providers")
	}
	settings.timeout, err = boundedDurationEnv("LLM_TIMEOUT", defaultLLMTimeout, 0, maxLLMTimeout)
	if err != nil {
		return llmSettings{}, err
	}
	requiredTaskBudget := investigationEvidenceCallCount*mcpCallTimeout + settings.timeout + minimumRuleFallbackBudget
	if taskTimeout < requiredTaskBudget {
		return llmSettings{}, fmt.Errorf("A2A_TASK_TIMEOUT must be at least %s to cover two MCP calls, LLM reasoning, and rule fallback", requiredTaskBudget)
	}
	settings.maxOutputTokens, err = boundedIntEnv("LLM_MAX_OUTPUT_TOKENS", defaultLLMMaxOutputTokens, minLLMMaxOutputTokens, maxLLMMaxOutputTokens)
	if err != nil {
		return llmSettings{}, err
	}
	return settings, nil
}

func diagnosisStrategyEnv() (domain.DiagnosisStrategy, error) {
	// The operator-facing spellings differ from the canonical domain names
	// (which are what is stored and sent on the wire) on purpose; see
	// docs/phase5-ai-diagnosis.md.
	canonical := map[string]string{
		"rule_based": domain.StrategyRules.String(),
		"llm_based":  domain.StrategyLLM.String(),
		"hybrid":     domain.StrategyHybrid.String(),
	}
	name, ok := canonical[stringEnv("DIAGNOSIS_STRATEGY", "rule_based")]
	if !ok {
		return 0, fmt.Errorf("DIAGNOSIS_STRATEGY must be rule_based, llm_based, or hybrid")
	}
	return domain.ParseDiagnosisStrategy(name)
}

func validateLLMBaseURL(value string) (bool, error) {
	if value == "" {
		return false, fmt.Errorf("LLM_BASE_URL is required for model diagnosis")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		// The value may carry credentials, so neither it nor the parse
		// error is reported.
		return false, fmt.Errorf("LLM_BASE_URL must be an absolute URL without credentials, query, or fragment")
	}
	host := parsed.Hostname()
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback) {
		return false, fmt.Errorf("LLM_BASE_URL must use HTTPS unless it targets loopback")
	}
	return loopback, nil
}
