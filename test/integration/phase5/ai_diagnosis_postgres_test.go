//go:build integration

package phase5

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	postgresadapter "github.com/belLena81/rollback-guard/internal/adapters/postgres"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestAIDiagnosisProvenanceRoundTripsInPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := postgrescontainer.Run(ctx, "postgres:16-alpine", postgrescontainer.WithDatabase("rollback_guard"), postgrescontainer.WithUsername("rollback_guard"), postgrescontainer.WithPassword("rollback_guard"), postgrescontainer.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, container)
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, name := range []string{"000001_initial_schema.up.sql", "000002_rollback_mutation_ledger.up.sql", "000003_a2a_investigation.up.sql", "000004_ai_diagnosis.up.sql"} {
		data, readErr := os.ReadFile(filepath.Join("../../../migrations", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}
	uow, err := postgresadapter.NewUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		effective  domain.DiagnosisStrategy
		provenance domain.DiagnosisProvenance
	}{
		{name: "rules", effective: domain.StrategyRules, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyRules}},
		{name: "llm", effective: domain.StrategyLLM, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyLLM, Model: "test-model", PromptVersion: "rollback-diagnosis-v1", InputTokens: 40, OutputTokens: 12}},
		{name: "hybrid fallback", effective: domain.StrategyRules, provenance: domain.DiagnosisProvenance{RequestedStrategy: domain.StrategyHybrid, Model: "test-model", PromptVersion: "rollback-diagnosis-v1", FallbackReason: domain.FallbackProviderTimeout}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 8, 6, 12, index, 0, 0, time.UTC)
			target, targetErr := domain.NewDeploymentTarget("local", "rollback-demo", fmt.Sprintf("demo-api-%d", index), "demo-api")
			if targetErr != nil {
				t.Fatal(targetErr)
			}
			symptom, symptomErr := domain.NewSymptom("http_error_rate", .35, .1, "ratio", now)
			if symptomErr != nil {
				t.Fatal(symptomErr)
			}
			incident, incidentErr := domain.NewIncident(domain.IncidentID(fmt.Sprintf("incident-%d", index)), target, symptom, now)
			if incidentErr != nil {
				t.Fatal(incidentErr)
			}
			deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)}
			evidence, evidenceErr := domain.NewDeploymentEvidence(domain.EvidenceID(fmt.Sprintf("evidence-%d", index)), incident.ID(), deployment, now)
			if evidenceErr != nil {
				t.Fatal(evidenceErr)
			}
			conclusion, conclusionErr := domain.NewInvestigationConclusionWithProvenance(domain.ConclusionID(fmt.Sprintf("conclusion-%d", index)), incident.ID(), test.effective, "rollout regression", .9, []domain.EvidenceID{evidence.ID()}, domain.RecommendRollback, test.provenance, now)
			if conclusionErr != nil {
				t.Fatal(conclusionErr)
			}
			if txErr := uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
				if createErr := repositories.Incidents().Create(incident); createErr != nil {
					return createErr
				}
				if addErr := repositories.Evidence().Add(evidence); addErr != nil {
					return addErr
				}
				return repositories.Conclusions().Save(conclusion)
			}); txErr != nil {
				t.Fatal(txErr)
			}
			if txErr := uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
				stored, loadErr := repositories.Conclusions().Get(incident.ID())
				if loadErr != nil {
					return loadErr
				}
				if stored.Snapshot().Strategy != conclusion.Snapshot().Strategy || stored.Provenance() != conclusion.Provenance() {
					t.Fatalf("stored conclusion = %#v, want %#v", stored.Snapshot(), conclusion.Snapshot())
				}
				return nil
			}); txErr != nil {
				t.Fatal(txErr)
			}
		})
	}
}
