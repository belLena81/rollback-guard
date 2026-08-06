package phase1_test

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestCompleteRollbackLifecycle(t *testing.T) {
	tests := []struct {
		name      string
		factory   diagnosisFactory
		effective domain.DiagnosisStrategy
		requested domain.DiagnosisStrategy
		fallback  domain.DiagnosisFallbackReason
	}{
		{name: "rules", factory: ruleDiagnosisFactory, effective: domain.StrategyRules, requested: domain.StrategyRules},
		{name: "llm", factory: modelDiagnosisFactory(domain.StrategyLLM, nil, false), effective: domain.StrategyLLM, requested: domain.StrategyLLM},
		{name: "hybrid", factory: modelDiagnosisFactory(domain.StrategyHybrid, nil, false), effective: domain.StrategyHybrid, requested: domain.StrategyHybrid},
		{name: "llm invalid output fallback", factory: modelDiagnosisFactory(domain.StrategyLLM, nil, true), effective: domain.StrategyRules, requested: domain.StrategyLLM, fallback: domain.FallbackInvalidOutput},
		{name: "hybrid provider fallback", factory: modelDiagnosisFactory(domain.StrategyHybrid, ports.ErrReasonerUnavailable, false), effective: domain.StrategyRules, requested: domain.StrategyHybrid, fallback: domain.FallbackProviderError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runCompleteRollbackLifecycle(t, test.factory, test.effective, test.requested, test.fallback)
		})
	}
}

func runCompleteRollbackLifecycle(t *testing.T, factory diagnosisFactory, effective domain.DiagnosisStrategy, requested domain.DiagnosisStrategy, fallback domain.DiagnosisFallbackReason) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", 0.35, 0.10, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	infrastructure := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target:                 target,
			CurrentRevision:        "revision-2",
			PreviousStableRevision: "revision-1",
			DesiredReplicas:        3,
			ReadyReplicas:          1,
			AvailableReplicas:      1,
			Healthy:                false,
			RolloutAt:              now.Add(-time.Minute),
		},
		Metric: domain.MetricEvidence{
			Name:       "http_error_rate",
			Value:      0.35,
			Threshold:  0.10,
			Unit:       "ratio",
			ObservedAt: now,
		},
		Now: clock.Now,
	}
	diagnoser := factory(t, ids)
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	systemActor, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork: memory.NewUnitOfWork(), Inspector: infrastructure, Metrics: infrastructure,
		Diagnoser: diagnoser, Executor: infrastructure, RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: clock, IDGenerator: ids,
	}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: 5 * time.Second, SystemActor: systemActor})
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}

	incident, err := services.Incidents.CreateIncident(ctx, application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: human})
	if err != nil {
		t.Fatal(err)
	}
	incident, err = services.Investigations.Investigate(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusAwaitingApproval {
		t.Fatalf("Investigate() status = %s", incident.Status())
	}
	summary, err := services.Queries.GetIncidentSummary(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Conclusion == nil || summary.Conclusion.Strategy() != effective || summary.Conclusion.Provenance().RequestedStrategy != requested || summary.Conclusion.Provenance().FallbackReason != fallback {
		t.Fatalf("diagnosis path = %#v, want effective/requested/fallback %s/%s/%s", summary.Conclusion, effective, requested, fallback)
	}
	proposal, err := services.Queries.GetProposal(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.Approvals.Approve(ctx, application.ApprovalCommand{IncidentID: incident.ID(), Actor: human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := services.Executions.Execute(ctx, incident.ID(), "execute-1")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := services.Executions.Execute(ctx, incident.ID(), "execute-1")
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID() != execution.ID() || infrastructure.ExecutionCount() != 1 {
		t.Fatalf("idempotent retry = %q with %d calls, want %q with 1 call", retry.ID(), infrastructure.ExecutionCount(), execution.ID())
	}
	infrastructure.SetRecoveryMetric(0.02)
	verification, err := services.Verifications.Verify(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Passed() {
		t.Fatal("verification did not pass")
	}
	incident, err = services.Queries.GetIncident(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusResolved {
		t.Fatalf("final status = %s, want resolved", incident.Status())
	}

	events, err := services.Queries.ListEvents(ctx, incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []domain.EventKind{
		domain.EventIncidentCreated,
		domain.EventStatusChanged,
		domain.EventEvidenceCollected,
		domain.EventEvidenceCollected,
		domain.EventDiagnosisCompleted,
		domain.EventPolicyEvaluated,
		domain.EventProposalCreated,
		domain.EventStatusChanged,
		domain.EventApprovalRecorded,
		domain.EventStatusChanged,
		domain.EventStatusChanged,
		domain.EventExecutionCompleted,
		domain.EventStatusChanged,
		domain.EventVerificationCompleted,
		domain.EventStatusChanged,
	}
	if len(events) != len(wantKinds) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantKinds))
	}
	for index, event := range events {
		if event.Sequence() != uint64(index+1) || event.Kind() != wantKinds[index] {
			t.Fatalf("event[%d] = sequence %d kind %s, want %d/%s", index, event.Sequence(), event.Kind(), index+1, wantKinds[index])
		}
	}
}

type diagnosisFactory func(*testing.T, ports.IDGenerator) ports.Diagnoser

func ruleDiagnosisFactory(t *testing.T, ids ports.IDGenerator) ports.Diagnoser {
	t.Helper()
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return diagnoser
}

func modelDiagnosisFactory(strategy domain.DiagnosisStrategy, reasonerErr error, invalid bool) diagnosisFactory {
	return func(t *testing.T, ids ports.IDGenerator) ports.Diagnoser {
		t.Helper()
		rules := ruleDiagnosisFactory(t, ids)
		reasoner := reasonerFunc(func(_ context.Context, request ports.ReasoningRequest) (ports.ReasoningResult, error) {
			if reasonerErr != nil {
				return ports.ReasoningResult{}, reasonerErr
			}
			candidate := ports.ReasoningCandidate{
				Hypothesis: "the rollout is correlated with the service regression", Confidence: .95,
				Recommendation: domain.RecommendRollback.String(),
			}
			for _, evidence := range request.Evidence {
				candidate.EvidenceIDs = append(candidate.EvidenceIDs, evidence.ID)
			}
			if invalid {
				candidate.Recommendation = "unsafe_action"
			}
			return ports.ReasoningResult{Candidate: candidate, Model: "test-model", Usage: ports.TokenUsage{InputTokens: 30, OutputTokens: 10}}, nil
		})
		config := application.ModelDiagnoserConfig{Model: "test-model", PromptVersion: application.DiagnosisPromptVersion, Timeout: time.Second}
		var diagnoser ports.Diagnoser
		var err error
		switch strategy {
		case domain.StrategyLLM:
			diagnoser, err = application.NewLLMDiagnoser(reasoner, rules, ids, config)
		case domain.StrategyHybrid:
			diagnoser, err = application.NewHybridDiagnoser(reasoner, rules, ids, config)
		default:
			t.Fatalf("unsupported test strategy %s", strategy)
		}
		if err != nil {
			t.Fatal(err)
		}
		return diagnoser
	}
}

type reasonerFunc func(context.Context, ports.ReasoningRequest) (ports.ReasoningResult, error)

func (f reasonerFunc) Reason(ctx context.Context, request ports.ReasoningRequest) (ports.ReasoningResult, error) {
	return f(ctx, request)
}
