//go:build integration

package phase4

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestA2ARESTTasksAndIdempotencySurviveInPostgres(t *testing.T) {
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
	for _, name := range []string{"000001_initial_schema.up.sql", "000002_rollback_mutation_ledger.up.sql", "000003_a2a_investigation.up.sql"} {
		data, readErr := os.ReadFile(filepath.Join("../../../migrations", name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := pool.Exec(ctx, string(data)); execErr != nil {
			t.Fatalf("apply %s: %v", name, execErr)
		}
	}

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target, _ := domain.NewDeploymentTarget("local", "demo", "api", "api")
	symptom, _ := domain.NewSymptom("error_rate", 0.2, 0.05, "ratio", now.Add(-time.Minute))
	incident, _ := domain.NewIncident("incident-1", target, symptom, now)
	_ = incident.StartInvestigation(now.Add(time.Second))
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "rev-2", PreviousStableRevision: "rev-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)}
	evidence, _ := domain.NewDeploymentEvidence("evidence-1", incident.ID(), deployment, now)
	conclusion, _ := domain.NewInvestigationConclusion("conclusion-1", incident.ID(), domain.StrategyRules, "new revision unhealthy", .99, []domain.EvidenceID{"evidence-1"}, domain.RecommendRollback, now)
	var calls atomic.Int32
	runner := investigatorFunc(func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
		calls.Add(1)
		return ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{evidence}, Conclusion: conclusion}, nil
	})
	idempotent, err := a2aadapter.NewIdempotentInvestigator(pool, runner)
	if err != nil {
		t.Fatal(err)
	}
	store, err := a2aadapter.NewPostgresTaskStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicURL := "http://" + listener.Addr().String()
	handler, err := a2aadapter.NewServerHandler(idempotent, store, nil, a2aadapter.ServerConfig{PublicURL: publicURL, TaskTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Shutdown(context.Background())
	gateway, err := a2aadapter.NewGateway(ctx, publicURL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	requestHash := application.InvestigationRequestHash(incident)
	for _, messageID := range []string{"message-1", "message-2"} {
		ref, submitErr := gateway.Submit(ctx, ports.InvestigationSubmission{Incident: incident, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", MessageID: messageID, TraceID: "trace-1", RequestHash: requestHash})
		if submitErr != nil {
			t.Fatal(submitErr)
		}
		result, watchErr := gateway.Watch(ctx, ref, nil)
		if watchErr != nil {
			t.Fatal(watchErr)
		}
		if result.Conclusion.ID() != conclusion.ID() {
			t.Fatalf("conclusion = %q", result.Conclusion.ID())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("investigator calls = %d, want 1", calls.Load())
	}
	var taskCount, requestCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM a2a_tasks`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM a2a_investigation_requests`).Scan(&requestCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 2 || requestCount != 1 {
		t.Fatalf("task/request rows = %d/%d", taskCount, requestCount)
	}

	requestCtx, cancelRequest := context.WithCancel(ctx)
	var canceledCalls atomic.Int32
	cancelingRunner := investigatorFunc(func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
		canceledCalls.Add(1)
		cancelRequest()
		return ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{evidence}, Conclusion: conclusion}, nil
	})
	cancelingInvestigator, err := a2aadapter.NewIdempotentInvestigator(pool, cancelingRunner)
	if err != nil {
		t.Fatal(err)
	}
	directRequest := ports.InvestigationRequest{Incident: incident, IdempotencyKey: "investigation:incident-canceled-context", RequestHash: requestHash, TaskID: "task-canceled-context-1"}
	if _, err := cancelingInvestigator.Investigate(requestCtx, directRequest); err != nil {
		t.Fatalf("complete after caller cancellation: %v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state::text FROM a2a_investigation_requests WHERE idempotency_key=$1`, directRequest.IdempotencyKey).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "completed" {
		t.Fatalf("idempotency state = %q, want completed", state)
	}
	directRequest.TaskID = "task-canceled-context-2"
	if _, err := cancelingInvestigator.Investigate(ctx, directRequest); err != nil {
		t.Fatalf("read cached result: %v", err)
	}
	if canceledCalls.Load() != 1 {
		t.Fatalf("investigator calls after cached retry = %d, want 1", canceledCalls.Load())
	}
}

type investigatorFunc func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error)

func (f investigatorFunc) Investigate(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	return f(ctx, request)
}
