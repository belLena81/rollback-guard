package a2aadapter

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestRESTGatewayRunsTypedInvestigationTask(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "payments", "api", "api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.2, 0.05, "ratio", now.Add(-time.Minute))
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
	requestHash := application.InvestigationRequestHash(incident)
	encoded, err := dataMap(encodeRequest(incident))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decodedHash := application.InvestigationRequestHash(decoded.Incident); decodedHash != requestHash {
		t.Fatalf("wire request hash = %q, want %q", decodedHash, requestHash)
	}
	ref, err := gateway.Submit(context.Background(), ports.InvestigationSubmission{Incident: incident, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", MessageID: "message-1", TraceID: "trace-1", RequestHash: requestHash})
	if err != nil {
		t.Fatal(err)
	}
	var stages []domain.InvestigationStage
	result, err := gateway.Watch(context.Background(), ref, func(stage domain.InvestigationStage) error { stages = append(stages, stage); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Conclusion.ID() != conclusion.ID() {
		t.Fatalf("conclusion = %q", result.Conclusion.ID())
	}
	if calls.Load() != 1 {
		t.Fatalf("investigator calls = %d", calls.Load())
	}
	if len(stages) > 0 && stages[len(stages)-1] != domain.InvestigationStageFinalizing {
		t.Fatalf("last progress stage = %v", stages)
	}
	changedSymptom, err := domain.NewSymptom("http_error_rate", 0.3, 0.05, "ratio", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	changedIncident, err := domain.NewIncident("incident-1", target, changedSymptom, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := changedIncident.StartInvestigation(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRequestHashMismatchCode(t, investigator, changedIncident, requestHash)
	ref, err = gateway.Submit(context.Background(), ports.InvestigationSubmission{Incident: changedIncident, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", MessageID: "message-2", TraceID: "trace-1", RequestHash: requestHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Watch(context.Background(), ref, nil); err == nil {
		t.Fatal("expected mismatched request hash to fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("investigator calls after mismatch = %d, want 1", calls.Load())
	}
}

func assertRequestHashMismatchCode(t *testing.T, investigator ports.Investigator, incident domain.Incident, requestHash string) {
	t.Helper()
	part := a2a.NewDataPart(encodeRequest(incident))
	part.MediaType = JSONMediaType
	message := a2a.NewMessage(a2a.MessageRoleUser, part)
	correlation, err := dataMap(correlationDTO{SchemaVersion: SchemaVersion, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", TraceID: "trace-1", RequestHash: requestHash})
	if err != nil {
		t.Fatal(err)
	}
	execCtx := &a2asrv.ExecutorContext{Message: message, TaskID: "task-mismatch", ContextID: "context-mismatch", Metadata: map[string]any{ExtensionURI: correlation}}
	executor := &investigationExecutor{investigator: investigator, timeout: time.Second, logger: slog.Default()}
	failureCode := ""
	for event, eventErr := range executor.Execute(context.Background(), execCtx) {
		if eventErr != nil {
			t.Fatal(eventErr)
		}
		status, ok := event.(*a2a.TaskStatusUpdateEvent)
		if !ok || status.Status.State != a2a.TaskStateFailed || status.Status.Message == nil {
			continue
		}
		var failure struct {
			SchemaVersion string `json:"schemaVersion"`
			Code          string `json:"code"`
		}
		if err := strictDecode(status.Status.Message.Parts[0].Data(), &failure); err != nil {
			t.Fatal(err)
		}
		failureCode = failure.Code
	}
	if failureCode != "request_hash_mismatch" {
		t.Fatalf("failure code = %q, want request_hash_mismatch", failureCode)
	}
}

func TestDecodeRequestRejectsUnknownFields(t *testing.T) {
	_, err := decodeRequest(map[string]any{"schemaVersion": SchemaVersion, "incident": map[string]any{}, "unexpected": true})
	if err == nil {
		t.Fatal("expected strict decoding error")
	}
}

func TestResultWirePreservesModelProvenance(t *testing.T) {
	result := modelWireResult(t)
	wire := encodeResult(result.Conclusion.IncidentID(), result)
	decoded, err := decodeResult(wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Conclusion.Strategy() != domain.StrategyHybrid || decoded.Conclusion.Provenance() != result.Conclusion.Provenance() {
		t.Fatalf("decoded conclusion = %#v", decoded.Conclusion.Snapshot())
	}
}

func TestResultWireAcceptsLegacyRuleConclusionWithoutProvenance(t *testing.T) {
	result := modelWireResult(t)
	rules, err := domain.NewInvestigationConclusion(
		"legacy-conclusion", result.Conclusion.IncidentID(), domain.StrategyRules, "legacy rules", .9,
		result.Conclusion.EvidenceIDs(), domain.RecommendRollback, result.Conclusion.CreatedAt(),
	)
	if err != nil {
		t.Fatal(err)
	}
	result.Conclusion = rules
	wire := encodeResult(rules.IncidentID(), result)
	if wire.Conclusion.Provenance != nil {
		t.Fatalf("legacy-compatible rule result contains provenance: %#v", wire.Conclusion.Provenance)
	}
	decoded, err := decodeResult(wire)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Conclusion.Provenance().RequestedStrategy != domain.StrategyRules {
		t.Fatalf("legacy provenance = %#v", decoded.Conclusion.Provenance())
	}
}

func TestResultWireRejectsUnknownRecommendation(t *testing.T) {
	tests := []struct {
		name           string
		recommendation string
		legacy         bool
		wantErr        bool
	}{
		{name: "rollback", recommendation: domain.RecommendRollback.String()},
		{name: "no action", recommendation: domain.RecommendNoAction.String()},
		{name: "unknown value", recommendation: "restart_pods", wantErr: true},
		{name: "empty value", recommendation: "", wantErr: true},
		{name: "wrong case", recommendation: "Rollback", wantErr: true},
		{name: "zero value spelling", recommendation: domain.Recommendation(0).String(), wantErr: true},
		{name: "legacy result with unknown value", recommendation: "restart_pods", legacy: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := modelWireResult(t)
			wire := encodeResult(result.Conclusion.IncidentID(), result)
			if test.legacy {
				wire.Conclusion.Strategy = domain.StrategyRules.String()
				wire.Conclusion.Provenance = nil
			}
			wire.Conclusion.Recommendation = test.recommendation

			decoded, err := decodeResult(wire)
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeResult() with recommendation %q error = %v, wantErr %t", test.recommendation, err, test.wantErr)
			}
			var validation *domain.ValidationError
			if test.wantErr && (!errors.As(err, &validation) || validation.Field != "conclusion.recommendation") {
				t.Fatalf("decodeResult() with recommendation %q error = %v, want a conclusion.recommendation validation error", test.recommendation, err)
			}
			if err == nil && decoded.Conclusion.Recommendation().String() != test.recommendation {
				t.Fatalf("decoded recommendation = %s, want %s", decoded.Conclusion.Recommendation(), test.recommendation)
			}
		})
	}
}

func TestResultFromArtifactsRequiresExactlyOneResultArtifact(t *testing.T) {
	valid := validResultArtifact(t)
	unexpected := &a2a.Artifact{Name: "debug-log", Parts: a2a.ContentParts{a2a.NewTextPart("ignored")}}
	duplicate := validResultArtifact(t)

	tests := []struct {
		name      string
		artifacts []*a2a.Artifact
		wantErr   bool
	}{
		{name: "valid single result", artifacts: []*a2a.Artifact{valid}},
		{name: "extra artifact", artifacts: []*a2a.Artifact{valid, unexpected}, wantErr: true},
		{name: "duplicate result artifact", artifacts: []*a2a.Artifact{valid, duplicate}, wantErr: true},
		{name: "only unexpected artifact", artifacts: []*a2a.Artifact{unexpected}, wantErr: true},
		{name: "nil artifact", artifacts: []*a2a.Artifact{nil}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := resultFromArtifacts(test.artifacts)
			if test.wantErr && err == nil {
				t.Fatal("expected result artifact validation error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("result artifact validation error = %v", err)
			}
		})
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

func validResultArtifact(t *testing.T) *a2a.Artifact {
	t.Helper()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "payments", "api", "api")
	if err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "rev-2", PreviousStableRevision: "rev-1", DesiredReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3, Healthy: true, RolloutAt: now.Add(-2 * time.Minute)}
	evidence, err := domain.NewDeploymentEvidence("evidence-1", "incident-1", deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, err := domain.NewInvestigationConclusion("conclusion-1", "incident-1", domain.StrategyRules, "new revision is unhealthy", 0.98, []domain.EvidenceID{"evidence-1"}, domain.RecommendRollback, now)
	if err != nil {
		t.Fatal(err)
	}
	part := a2a.NewDataPart(encodeResult("incident-1", ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{evidence}, Conclusion: conclusion}))
	part.MediaType = JSONMediaType
	return &a2a.Artifact{Name: ResultArtifactName, Parts: a2a.ContentParts{part}}
}

func modelWireResult(t *testing.T) ports.InvestigationResult {
	t.Helper()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)}
	evidence, err := domain.NewDeploymentEvidence("evidence", "incident", deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	provenance := domain.DiagnosisProvenance{
		RequestedStrategy: domain.StrategyHybrid, Model: "test-model", PromptVersion: "rollback-diagnosis-v1",
		InputTokens: 30, OutputTokens: 10,
	}
	conclusion, err := domain.NewInvestigationConclusionWithProvenance(
		"conclusion", "incident", domain.StrategyHybrid, "rollout regression", .92,
		[]domain.EvidenceID{evidence.ID()}, domain.RecommendRollback, provenance, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{evidence}, Conclusion: conclusion}
}
