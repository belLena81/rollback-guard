package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

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
	FakeInfrastructure    FakeInfrastructureConfig
}

type FakeInfrastructureConfig struct {
	CurrentRevision        domain.Revision
	PreviousStableRevision domain.Revision
	DesiredReplicas        int
	ReadyReplicas          int
	AvailableReplicas      int
	Healthy                bool
	MetricValue            float64
	RecoveryMetricValue    float64
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
	targets, err := targetsEnv("ALLOWED_TARGETS", "local/demo/demo-api/demo-api")
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
		FakeInfrastructure: FakeInfrastructureConfig{
			CurrentRevision:        domain.Revision(stringEnv("FAKE_CURRENT_REVISION", "revision-2")),
			PreviousStableRevision: domain.Revision(stringEnv("FAKE_PREVIOUS_STABLE_REVISION", "revision-1")),
			DesiredReplicas:        mustInt("FAKE_DESIRED_REPLICAS", 3),
			ReadyReplicas:          mustInt("FAKE_READY_REPLICAS", 1),
			AvailableReplicas:      mustInt("FAKE_AVAILABLE_REPLICAS", 1),
			Healthy:                mustBool("FAKE_HEALTHY", false),
			MetricValue:            mustFloat("FAKE_METRIC_VALUE", 0.35),
			RecoveryMetricValue:    mustFloat("FAKE_RECOVERY_METRIC_VALUE", 0.02),
		},
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
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

func mustInt(name string, fallback int) int {
	value, err := intEnv(name, fallback)
	if err != nil {
		return fallback
	}
	return value
}

func mustFloat(name string, fallback float64) float64 {
	value, err := floatEnv(name, fallback)
	if err != nil {
		return fallback
	}
	return value
}

func mustBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
