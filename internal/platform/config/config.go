package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

const (
	defaultAllowedTargets          = "local/rollback-demo/demo-api/demo-api"
	investigationEvidenceCallCount = 2
	minimumRuleFallbackBudget      = time.Second
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
	InvestigatorURL       string
	A2ATaskTimeout        time.Duration
	A2ACancelTimeout      time.Duration
	A2APollInterval       time.Duration
	A2ALeaseTTL           time.Duration
	A2AWorkerID           string
	A2AMaxAttempts        int
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
	LLMProvider           LLMProvider
	LLMBaseURL            string
	LLMModel              string
	LLMAPIKey             string
	LLMTimeout            time.Duration
	LLMMaxOutputTokens    int
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
}

type ObservabilityMCPConfig struct {
	HTTPAddr       string
	PrometheusURL  string
	AllowedTargets []domain.DeploymentTarget
	RequestTimeout time.Duration
	SessionTimeout time.Duration
	MetricsWindow  time.Duration
	MaxSampleAge   time.Duration
}

func Load() (Config, error) {
	proposalTTL, err := durationEnv("PROPOSAL_TTL", 15*time.Minute)
	if err != nil {
		return Config{}, err
	}
	approvalTTL, err := durationEnv("APPROVAL_TTL", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	cleanupTimeout, err := durationEnv("CLEANUP_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	dbPingTimeout, err := durationEnv("DB_PING_TIMEOUT", 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	ruleWindow, err := durationEnv("RULE_CORRELATION_WINDOW", 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	confidence, err := floatEnv("RULE_MIN_CONFIDENCE", 0.8)
	if err != nil {
		return Config{}, err
	}
	maxConns, err := intEnv("DB_MAX_CONNS", 4)
	if err != nil {
		return Config{}, err
	}
	targets, err := targetsEnv("ALLOWED_TARGETS", defaultAllowedTargets)
	if err != nil {
		return Config{}, err
	}
	mcpConnectTimeout, err := durationEnv("MCP_CONNECT_TIMEOUT", 5*time.Second)
	if err != nil || mcpConnectTimeout > 30*time.Second {
		return Config{}, fmt.Errorf("MCP_CONNECT_TIMEOUT must be positive and no greater than 30s")
	}
	mcpCallTimeout, err := durationEnv("MCP_CALL_TIMEOUT", 10*time.Second)
	if err != nil || mcpCallTimeout > 30*time.Second {
		return Config{}, fmt.Errorf("MCP_CALL_TIMEOUT must be positive and no greater than 30s")
	}
	mcpWatchTimeout, err := durationEnv("MCP_WATCH_TIMEOUT", 90*time.Second)
	if err != nil || mcpWatchTimeout < time.Second || mcpWatchTimeout > 2*time.Minute {
		return Config{}, fmt.Errorf("MCP_WATCH_TIMEOUT must be between 1s and 2m")
	}
	metricsWindow, err := durationEnv("METRICS_WINDOW", 5*time.Minute)
	if err != nil || metricsWindow < 30*time.Second || metricsWindow > time.Hour {
		return Config{}, fmt.Errorf("METRICS_WINDOW must be between 30s and 1h")
	}
	a2aTaskTimeout, err := durationEnv("A2A_TASK_TIMEOUT", 45*time.Second)
	if err != nil || a2aTaskTimeout > 2*time.Minute {
		return Config{}, fmt.Errorf("A2A_TASK_TIMEOUT must be positive and no greater than 2m")
	}
	a2aCancelTimeout, err := durationEnv("A2A_CANCEL_TIMEOUT", 5*time.Second)
	if err != nil || a2aCancelTimeout > 30*time.Second {
		return Config{}, fmt.Errorf("A2A_CANCEL_TIMEOUT must be positive and no greater than 30s")
	}
	a2aPollInterval, err := durationEnv("A2A_POLL_INTERVAL", 250*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	a2aLeaseTTL, err := durationEnv("A2A_LEASE_TTL", a2aTaskTimeout+a2aCancelTimeout)
	if err != nil || a2aLeaseTTL < a2aTaskTimeout+a2aCancelTimeout {
		return Config{}, fmt.Errorf("A2A_LEASE_TTL must be at least A2A_TASK_TIMEOUT plus A2A_CANCEL_TIMEOUT")
	}
	a2aMaxAttempts, err := intEnv("A2A_MAX_ATTEMPTS", 3)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		HTTPAddr:              stringEnv("HTTP_ADDR", ":8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DBMaxConns:            int32(maxConns),
		DBPingTimeout:         dbPingTimeout,
		ProposalTTL:           proposalTTL,
		ApprovalTTL:           approvalTTL,
		CleanupTimeout:        cleanupTimeout,
		SystemActorID:         domain.ActorID(stringEnv("SYSTEM_ACTOR_ID", "rollback-guardian")),
		LocalActorID:          domain.ActorID(stringEnv("LOCAL_ACTOR_ID", "local-admin")),
		RuleMinimumConfidence: confidence,
		RuleCorrelationWindow: ruleWindow,
		AllowedTargets:        targets,
		KubernetesReadMCPURL:  stringEnv("KUBERNETES_READ_MCP_URL", "http://127.0.0.1:8091/mcp"),
		KubernetesWriteMCPURL: stringEnv("KUBERNETES_ROLLBACK_MCP_URL", "http://127.0.0.1:8092/mcp"),
		ObservabilityMCPURL:   stringEnv("OBSERVABILITY_MCP_URL", "http://127.0.0.1:8093/mcp"),
		MCPConnectTimeout:     mcpConnectTimeout,
		MCPCallTimeout:        mcpCallTimeout,
		MCPWatchTimeout:       mcpWatchTimeout,
		MetricsWindow:         metricsWindow,
		InvestigatorURL:       stringEnv("INVESTIGATOR_URL", "http://127.0.0.1:8094"),
		A2ATaskTimeout:        a2aTaskTimeout,
		A2ACancelTimeout:      a2aCancelTimeout,
		A2APollInterval:       a2aPollInterval,
		A2ALeaseTTL:           a2aLeaseTTL,
		A2AWorkerID:           stringEnv("A2A_WORKER_ID", "orchestrator-1"),
		A2AMaxAttempts:        a2aMaxAttempts,
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func LoadInvestigator() (InvestigatorConfig, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return InvestigatorConfig{}, fmt.Errorf("DATABASE_URL is required")
	}
	maxConns, err := intEnv("DB_MAX_CONNS", 4)
	if err != nil {
		return InvestigatorConfig{}, err
	}
	dbPingTimeout, err := durationEnv("DB_PING_TIMEOUT", 2*time.Second)
	if err != nil {
		return InvestigatorConfig{}, err
	}
	connectTimeout, err := durationEnv("MCP_CONNECT_TIMEOUT", 5*time.Second)
	if err != nil || connectTimeout > 30*time.Second {
		return InvestigatorConfig{}, fmt.Errorf("MCP_CONNECT_TIMEOUT must be positive and no greater than 30s")
	}
	callTimeout, err := durationEnv("MCP_CALL_TIMEOUT", 10*time.Second)
	if err != nil || callTimeout > 30*time.Second {
		return InvestigatorConfig{}, fmt.Errorf("MCP_CALL_TIMEOUT must be positive and no greater than 30s")
	}
	taskTimeout, err := durationEnv("A2A_TASK_TIMEOUT", 45*time.Second)
	if err != nil || taskTimeout > 2*time.Minute {
		return InvestigatorConfig{}, fmt.Errorf("A2A_TASK_TIMEOUT must be positive and no greater than 2m")
	}
	metricsWindow, err := durationEnv("METRICS_WINDOW", 5*time.Minute)
	if err != nil || metricsWindow < 30*time.Second || metricsWindow > time.Hour {
		return InvestigatorConfig{}, fmt.Errorf("METRICS_WINDOW must be between 30s and 1h")
	}
	ruleWindow, err := durationEnv("RULE_CORRELATION_WINDOW", 10*time.Minute)
	if err != nil {
		return InvestigatorConfig{}, err
	}
	strategy, err := diagnosisStrategyEnv()
	if err != nil {
		return InvestigatorConfig{}, err
	}
	provider := LLMProvider(stringEnv("LLM_PROVIDER", string(LLMProviderOpenAICompatible)))
	baseURL := strings.TrimSpace(os.Getenv("LLM_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("LLM_MODEL"))
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	llmTimeout := 15 * time.Second
	maxOutputTokens := 512
	if strategy != domain.StrategyRules {
		if provider != LLMProviderOpenAICompatible {
			return InvestigatorConfig{}, fmt.Errorf("LLM_PROVIDER must be %q", LLMProviderOpenAICompatible)
		}
		loopback, urlErr := validateLLMBaseURL(baseURL)
		if urlErr != nil {
			return InvestigatorConfig{}, urlErr
		}
		if model == "" {
			return InvestigatorConfig{}, fmt.Errorf("LLM_MODEL is required for model diagnosis")
		}
		if !loopback && apiKey == "" {
			return InvestigatorConfig{}, fmt.Errorf("LLM_API_KEY is required for non-loopback providers")
		}
		llmTimeout, err = durationEnv("LLM_TIMEOUT", llmTimeout)
		if err != nil || llmTimeout > 30*time.Second {
			return InvestigatorConfig{}, fmt.Errorf("LLM_TIMEOUT must be positive and no greater than 30s")
		}
		requiredTaskBudget := investigationEvidenceCallCount*callTimeout + llmTimeout + minimumRuleFallbackBudget
		if taskTimeout < requiredTaskBudget {
			return InvestigatorConfig{}, fmt.Errorf("A2A_TASK_TIMEOUT must be at least %s to cover two MCP calls, LLM reasoning, and rule fallback", requiredTaskBudget)
		}
		maxOutputTokens, err = intEnv("LLM_MAX_OUTPUT_TOKENS", maxOutputTokens)
		if err != nil || maxOutputTokens < 64 || maxOutputTokens > 2048 {
			return InvestigatorConfig{}, fmt.Errorf("LLM_MAX_OUTPUT_TOKENS must be between 64 and 2048")
		}
	}
	return InvestigatorConfig{
		HTTPAddr:              stringEnv("HTTP_ADDR", "127.0.0.1:8094"),
		PublicURL:             stringEnv("INVESTIGATOR_PUBLIC_URL", "http://127.0.0.1:8094"),
		DatabaseURL:           databaseURL,
		DBMaxConns:            int32(maxConns),
		DBPingTimeout:         dbPingTimeout,
		KubernetesReadMCPURL:  stringEnv("KUBERNETES_READ_MCP_URL", "http://127.0.0.1:8091/mcp"),
		ObservabilityMCPURL:   stringEnv("OBSERVABILITY_MCP_URL", "http://127.0.0.1:8093/mcp"),
		MCPConnectTimeout:     connectTimeout,
		MCPCallTimeout:        callTimeout,
		TaskTimeout:           taskTimeout,
		MetricsWindow:         metricsWindow,
		RuleCorrelationWindow: ruleWindow,
		DiagnosisStrategy:     strategy,
		LLMProvider:           provider,
		LLMBaseURL:            baseURL,
		LLMModel:              model,
		LLMAPIKey:             apiKey,
		LLMTimeout:            llmTimeout,
		LLMMaxOutputTokens:    maxOutputTokens,
	}, nil
}

func diagnosisStrategyEnv() (domain.DiagnosisStrategy, error) {
	switch stringEnv("DIAGNOSIS_STRATEGY", "rule_based") {
	case "rule_based":
		return domain.StrategyRules, nil
	case "llm_based":
		return domain.StrategyLLM, nil
	case "hybrid":
		return domain.StrategyHybrid, nil
	default:
		return 0, fmt.Errorf("DIAGNOSIS_STRATEGY must be rule_based, llm_based, or hybrid")
	}
}

func validateLLMBaseURL(value string) (bool, error) {
	if value == "" {
		return false, fmt.Errorf("LLM_BASE_URL is required for model diagnosis")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
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

func LoadKubernetesMCP() (KubernetesMCPConfig, error) {
	targets, err := targetsEnv("ALLOWED_TARGETS", defaultAllowedTargets)
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	requestTimeout, err := durationEnv("MCP_REQUEST_TIMEOUT", 2*time.Minute)
	if err != nil || requestTimeout > 2*time.Minute {
		return KubernetesMCPConfig{}, fmt.Errorf("MCP_REQUEST_TIMEOUT must be positive and no greater than 2m")
	}
	sessionTimeout, err := durationEnv("MCP_SESSION_TIMEOUT", 5*time.Minute)
	if err != nil {
		return KubernetesMCPConfig{}, err
	}
	mode := stringEnv("KUBERNETES_MCP_MODE", "readonly")
	if mode != "readonly" && mode != "rollback" {
		return KubernetesMCPConfig{}, fmt.Errorf("KUBERNETES_MCP_MODE must be readonly or rollback")
	}
	clusterID := stringEnv("CLUSTER_ID", "local")
	for _, target := range targets {
		if target.Cluster() != clusterID {
			return KubernetesMCPConfig{}, fmt.Errorf("ALLOWED_TARGETS must use CLUSTER_ID %q", clusterID)
		}
	}
	defaultAddr := ":8091"
	databaseURL := ""
	var dbMaxConns int32
	var dbPingTimeout time.Duration
	if mode == "rollback" {
		defaultAddr = ":8092"
		databaseURL = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		if databaseURL == "" {
			return KubernetesMCPConfig{}, fmt.Errorf("DATABASE_URL is required in rollback mode")
		}
		maxConns, maxConnsErr := intEnv("DB_MAX_CONNS", 4)
		if maxConnsErr != nil {
			return KubernetesMCPConfig{}, maxConnsErr
		}
		dbMaxConns = int32(maxConns)
		dbPingTimeout, err = durationEnv("DB_PING_TIMEOUT", 2*time.Second)
		if err != nil {
			return KubernetesMCPConfig{}, err
		}
	}
	return KubernetesMCPConfig{HTTPAddr: stringEnv("HTTP_ADDR", defaultAddr), Mode: mode, ClusterID: clusterID, Kubeconfig: strings.TrimSpace(os.Getenv("KUBECONFIG")), DatabaseURL: databaseURL, DBMaxConns: dbMaxConns, DBPingTimeout: dbPingTimeout, AllowedTargets: targets, RequestTimeout: requestTimeout, SessionTimeout: sessionTimeout}, nil
}

func LoadObservabilityMCP() (ObservabilityMCPConfig, error) {
	targets, err := targetsEnv("ALLOWED_TARGETS", defaultAllowedTargets)
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	requestTimeout, err := durationEnv("PROMETHEUS_TIMEOUT", 5*time.Second)
	if err != nil || requestTimeout > 30*time.Second {
		return ObservabilityMCPConfig{}, fmt.Errorf("PROMETHEUS_TIMEOUT must be positive and no greater than 30s")
	}
	sessionTimeout, err := durationEnv("MCP_SESSION_TIMEOUT", 5*time.Minute)
	if err != nil {
		return ObservabilityMCPConfig{}, err
	}
	window, err := durationEnv("METRICS_WINDOW", 5*time.Minute)
	if err != nil || window < 30*time.Second || window > time.Hour {
		return ObservabilityMCPConfig{}, fmt.Errorf("METRICS_WINDOW must be between 30s and 1h")
	}
	maxAge, err := durationEnv("METRICS_MAX_SAMPLE_AGE", time.Minute)
	if err != nil || maxAge > 15*time.Minute {
		return ObservabilityMCPConfig{}, fmt.Errorf("METRICS_MAX_SAMPLE_AGE must be positive and no greater than 15m")
	}
	return ObservabilityMCPConfig{HTTPAddr: stringEnv("HTTP_ADDR", ":8093"), PrometheusURL: stringEnv("PROMETHEUS_URL", "http://127.0.0.1:9090"), AllowedTargets: targets, RequestTimeout: requestTimeout, SessionTimeout: sessionTimeout, MetricsWindow: window, MaxSampleAge: maxAge}, nil
}

func stringEnv(name string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return duration, nil
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func floatEnv(name string, fallback float64) (float64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a float", name)
	}
	return parsed, nil
}

func targetsEnv(name string, fallback string) ([]domain.DeploymentTarget, error) {
	value := stringEnv(name, fallback)
	parts := strings.Split(value, ",")
	targets := make([]domain.DeploymentTarget, 0, len(parts))
	for _, part := range parts {
		fields := strings.Split(strings.TrimSpace(part), "/")
		if len(fields) != 4 {
			return nil, fmt.Errorf("%s target %q must be cluster/namespace/deployment/service", name, part)
		}
		target, err := domain.NewDeploymentTarget(fields[0], fields[1], fields[2], fields[3])
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		targets = append(targets, target)
	}
	return targets, nil
}
