package a2aadapter

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestRESTGatewayRunsTypedInvestigationTask(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "payments", "api", "api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("error_rate", 0.2, 0.05, "ratio", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.NewIncident("incident-1", target, symptom, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := incident.StartInvestigation(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "rev-2", PreviousStableRevision: "rev-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-2 * time.Minute)}
	deploymentEvidence, err := domain.NewDeploymentEvidence("evidence-1", incident.ID(), deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	metricEvidence, err := domain.NewMetricEvidence("evidence-2", incident.ID(), domain.MetricEvidence{Name: "error_rate", Value: 0.2, Threshold: 0.05, Unit: "ratio", ObservedAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, err := domain.NewInvestigationConclusion("conclusion-1", incident.ID(), domain.StrategyRules, "new revision is unhealthy", 0.98, []domain.EvidenceID{"evidence-1", "evidence-2"}, domain.RecommendRollback, now)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	investigator := investigatorFunc(func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
		calls.Add(1)
		return ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{deploymentEvidence, metricEvidence}, Conclusion: conclusion}, nil
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicURL := "http://" + listener.Addr().String()
	handler, err := NewServerHandler(investigator, nil, nil, ServerConfig{PublicURL: publicURL, TaskTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }() //nolint:errcheck // shutdown closes the listener
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	gateway, err := NewGateway(context.Background(), publicURL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gateway.Close(); err != nil {
			t.Error(err)
		}
	}()
	ref, err := gateway.Submit(context.Background(), ports.InvestigationSubmission{Incident: incident, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", MessageID: "message-1", TraceID: "trace-1", RequestHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	var stages []ports.InvestigationStage
	result, err := gateway.Watch(context.Background(), ref, func(stage ports.InvestigationStage) error { stages = append(stages, stage); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion.ID() != conclusion.ID() {
		t.Fatalf("conclusion = %q", result.Conclusion.ID())
	}
	if calls.Load() != 1 {
		t.Fatalf("investigator calls = %d", calls.Load())
	}
	if len(stages) > 0 && stages[len(stages)-1] != ports.InvestigationStageFinalizing {
		t.Fatalf("last progress stage = %v", stages)
	}
}

func TestDecodeRequestRejectsUnknownFields(t *testing.T) {
	_, err := decodeRequest(map[string]any{"schemaVersion": SchemaVersion, "incident": map[string]any{}, "unexpected": true})
	if err == nil {
		t.Fatal("expected strict decoding error")
	}
}

func TestAgentCardDeclaresOnlyTypedRESTInvestigation(t *testing.T) {
	card := AgentCard("http://127.0.0.1:8094")
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != "HTTP+JSON" || card.SupportedInterfaces[0].URL != "http://127.0.0.1:8094/a2a" {
		t.Fatalf("interfaces = %#v", card.SupportedInterfaces)
	}
	if !card.Capabilities.Streaming || len(card.Capabilities.Extensions) != 1 || card.Capabilities.Extensions[0].URI != ExtensionURI || !card.Capabilities.Extensions[0].Required {
		t.Fatalf("capabilities = %#v", card.Capabilities)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "investigate_rollout" {
		t.Fatalf("skills = %#v", card.Skills)
	}
}

type investigatorFunc func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error)

func (f investigatorFunc) Investigate(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	return f(ctx, request)
}
