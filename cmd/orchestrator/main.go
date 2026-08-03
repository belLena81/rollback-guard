package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	"github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("orchestrator exited", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	poolConfig.MaxConns = cfg.DBMaxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return err
	}
	defer pool.Close()
	uow, err := postgres.NewUnitOfWork(pool)
	if err != nil {
		return err
	}
	clock := runtime.Clock{}
	ids := &runtime.IDGenerator{}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: cfg.RuleCorrelationWindow})
	if err != nil {
		return err
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{
		MinimumConfidence: cfg.RuleMinimumConfidence,
		CorrelationWindow: cfg.RuleCorrelationWindow,
		AllowedTargets:    cfg.AllowedTargets,
	})
	if err != nil {
		return err
	}
	systemActor, err := domain.NewActor(cfg.SystemActorID, domain.ActorSystem)
	if err != nil {
		return err
	}
	localActor, err := domain.NewActor(cfg.LocalActorID, domain.ActorHuman)
	if err != nil {
		return err
	}
	target := cfg.AllowedTargets[0]
	infra := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target:                 target,
			CurrentRevision:        cfg.FakeInfrastructure.CurrentRevision,
			PreviousStableRevision: cfg.FakeInfrastructure.PreviousStableRevision,
			DesiredReplicas:        cfg.FakeInfrastructure.DesiredReplicas,
			ReadyReplicas:          cfg.FakeInfrastructure.ReadyReplicas,
			AvailableReplicas:      cfg.FakeInfrastructure.AvailableReplicas,
			Healthy:                cfg.FakeInfrastructure.Healthy,
			RolloutAt:              clock.Now().Add(-time.Minute),
		},
		Metric: domain.MetricEvidence{
			Name:       "http_error_rate",
			Value:      cfg.FakeInfrastructure.MetricValue,
			Threshold:  0.10,
			Unit:       "ratio",
			ObservedAt: clock.Now(),
		},
		Now: clock.Now,
	}
	infra.AfterExecute = func() {
		infra.Metric.Value = cfg.FakeInfrastructure.RecoveryMetricValue
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork:     uow,
		Inspector:      infra,
		Metrics:        infra,
		Diagnoser:      diagnoser,
		Executor:       infra,
		RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{},
		Clock:          clock,
		IDGenerator:    ids,
	}, application.WorkflowConfig{
		ProposalTTL:    cfg.ProposalTTL,
		ApprovalTTL:    cfg.ApprovalTTL,
		CleanupTimeout: cfg.CleanupTimeout,
		SystemActor:    systemActor,
	})
	if err != nil {
		return err
	}
	api, err := httpapi.NewServer(services, localActor, logger)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Routes(func(ctx context.Context) error { return pool.Ping(ctx) }),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("orchestrator listening", "addr", cfg.HTTPAddr)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
