package phase1_test

import (
	"context"
	"errors"
	"fmt"
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

func TestInvestigationDeclinesWhenPolicyFails(t *testing.T) {
	h := newHarness(t, nil)
	h.infrastructure.SetHealthy(true)
	incident, err := h.services.Investigations.Investigate(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != domain.StatusDeclined {
		t.Fatalf("status = %s, want declined", incident.Status())
	}
	_, err = h.services.Queries.GetProposal(context.Background(), incident.ID())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProposal() error = %v, want not found", err)
	}
	err = h.uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		evidence, listErr := repositories.Evidence().List(incident.ID())
		if listErr != nil {
			return listErr
		}
		if len(evidence) != 2 {
			return fmt.Errorf("evidence count = %d, want 2", len(evidence))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInvalidEvidenceReferenceFailsIncident(t *testing.T) {
	h := newHarness(t, invalidReferenceDiagnoser{})
	_, err := h.services.Investigations.Investigate(context.Background(), h.incident.ID())
	if !errors.Is(err, domain.ErrInvalidEvidenceReference) {
		t.Fatalf("Investigate() error = %v, want invalid evidence reference", err)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
	assertLastEventKind(t, h, domain.EventIncidentFailed)
}

func TestForeignIncidentConclusionFailsBeforePolicyOrPersistence(t *testing.T) {
	h := newHarness(t, foreignIncidentDiagnoser{})
	_, err := h.services.Investigations.Investigate(context.Background(), h.incident.ID())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Investigate() error = %v, want validation error", err)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
	assertLastEventKind(t, h, domain.EventIncidentFailed)

	err = h.uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		if _, getErr := repositories.Conclusions().Get("foreign-incident"); !errors.Is(getErr, domain.ErrNotFound) {
			t.Fatalf("foreign conclusion lookup error = %v, want not found", getErr)
		}
		if _, getErr := repositories.Proposals().GetByIncident(h.incident.ID()); !errors.Is(getErr, domain.ErrNotFound) {
			t.Fatalf("proposal lookup error = %v, want not found", getErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCanceledInvestigationStillFailsIncidentAtomically(t *testing.T) {
	tests := []struct {
		name      string
		overrides func(context.CancelFunc) harnessOverrides
	}{
		{name: "deployment inspection", overrides: func(cancel context.CancelFunc) harnessOverrides {
			return harnessOverrides{inspector: cancelingInspector{cancel: cancel}}
		}},
		{name: "metric observation", overrides: func(cancel context.CancelFunc) harnessOverrides {
			return harnessOverrides{metrics: cancelingMetrics{cancel: cancel}}
		}},
		{name: "diagnosis", overrides: func(cancel context.CancelFunc) harnessOverrides {
			return harnessOverrides{diagnoser: cancelingDiagnoser{cancel: cancel}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			h := newHarnessWithOverrides(t, test.overrides(cancel))
			_, err := h.services.Investigations.Investigate(ctx, h.incident.ID())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Investigate() error = %v, want context canceled", err)
			}
			assertIncidentStatus(t, h, domain.StatusFailed)
			assertLastEventKind(t, h, domain.EventIncidentFailed)
		})
	}
}

func TestApprovalRejectsMismatchedHashAndAgent(t *testing.T) {
	h := awaitingApprovalHarness(t)
	proposal := mustServiceProposal(t, h)
	_, err := h.services.Approvals.Approve(context.Background(), application.ApprovalCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: "wrong"})
	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("Approve(mismatch) error = %v", err)
	}
	agent, actorErr := domain.NewActor("investigator", domain.ActorAgent)
	if actorErr != nil {
		t.Fatal(actorErr)
	}
	_, err = h.services.Approvals.Approve(context.Background(), application.ApprovalCommand{IncidentID: h.incident.ID(), Actor: agent, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()})
	if !errors.Is(err, domain.ErrNonHumanApproval) {
		t.Fatalf("Approve(agent) error = %v", err)
	}
	assertIncidentStatus(t, h, domain.StatusAwaitingApproval)
}

func TestApprovalDecisionRetriesAreIdempotentAndConflictsAreRejected(t *testing.T) {
	h := awaitingApprovalHarness(t)
	proposal := mustServiceProposal(t, h)
	command := application.ApprovalCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()}
	first, err := h.services.Approvals.Approve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := h.services.Queries.ListEvents(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	retry, err := h.services.Approvals.Approve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID() != first.ID() {
		t.Fatalf("retry approval = %q, want %q", retry.ID(), first.ID())
	}
	eventsAfter, err := h.services.Queries.ListEvents(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("retry appended %d events", len(eventsAfter)-len(eventsBefore))
	}
	_, err = h.services.Approvals.Reject(context.Background(), application.RejectionCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash(), Reason: "unsafe"})
	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("conflicting rejection error = %v, want mismatch", err)
	}
}

func TestProposalAndApprovalExpireAtExactBoundaries(t *testing.T) {
	t.Run("proposal", func(t *testing.T) {
		h := awaitingApprovalHarness(t)
		proposal := mustServiceProposal(t, h)
		h.clock.Advance(15 * time.Minute)
		_, err := h.services.Approvals.Approve(context.Background(), application.ApprovalCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()})
		if !errors.Is(err, domain.ErrExpired) {
			t.Fatalf("Approve() error = %v, want expired", err)
		}
		assertIncidentStatus(t, h, domain.StatusExpired)
	})
	t.Run("approval", func(t *testing.T) {
		h := approvedHarness(t)
		h.clock.Advance(10 * time.Minute)
		_, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute")
		if !errors.Is(err, domain.ErrExpired) {
			t.Fatalf("Execute() error = %v, want expired", err)
		}
		assertIncidentStatus(t, h, domain.StatusExpired)
	})
}

func TestExecutionFailsOnRevisionDriftAndExecutorFailure(t *testing.T) {
	t.Run("revision drift", func(t *testing.T) {
		h := approvedHarness(t)
		h.infrastructure.SetCurrentRevision("revision-3")
		_, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute")
		if !errors.Is(err, domain.ErrApprovalMismatch) {
			t.Fatalf("Execute() error = %v, want approval mismatch", err)
		}
		if h.infrastructure.ExecutionCount() != 0 {
			t.Fatal("executor was called after revision drift")
		}
		assertIncidentStatus(t, h, domain.StatusFailed)
	})
	t.Run("executor failure", func(t *testing.T) {
		h := approvedHarness(t)
		h.infrastructure.ExecuteErr = errors.New("rollback unavailable")
		execution, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute")
		if err == nil || execution.Outcome() != domain.ExecutionFailed {
			t.Fatalf("Execute() outcome/error = %s/%v", execution.Outcome(), err)
		}
		assertIncidentStatus(t, h, domain.StatusFailed)
	})
}

func TestExecutionRetryRecoversAfterResultPersistenceIsCanceled(t *testing.T) {
	h := approvedHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.infrastructure.AfterExecute = cancel

	_, err := h.services.Executions.Execute(ctx, h.incident.ID(), "execute-pending")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Execute() error = %v, want context canceled", err)
	}
	assertIncidentStatus(t, h, domain.StatusExecuting)
	if h.infrastructure.ExecutionCount() != 1 {
		t.Fatalf("execution count = %d, want 1", h.infrastructure.ExecutionCount())
	}

	h.infrastructure.AfterExecute = nil
	h.clock.Advance(20 * time.Minute)
	execution, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute-pending")
	if err != nil {
		t.Fatal(err)
	}
	assertIncidentStatus(t, h, domain.StatusVerifying)
	if h.infrastructure.ExecutionCount() != 1 {
		t.Fatalf("execution count after retry = %d, want 1", h.infrastructure.ExecutionCount())
	}
	retry, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute-pending")
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID() != execution.ID() || h.infrastructure.ExecutionCount() != 1 {
		t.Fatalf("completed retry = %q with %d calls, want %q with 1 call", retry.ID(), h.infrastructure.ExecutionCount(), execution.ID())
	}
	err = h.uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		record, getErr := repositories.Idempotency().Get("execute-pending")
		if getErr != nil {
			return getErr
		}
		if record.State() != domain.IdempotencyCompleted || record.ResultID() != execution.ID() {
			return fmt.Errorf("attempt state/result = %v/%q", record.State(), record.ResultID())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewServicesRejectsIncompleteComposition(t *testing.T) {
	if _, err := application.NewServices(application.Dependencies{}, application.WorkflowConfig{}); err == nil {
		t.Fatal("NewServices() error = nil, want validation")
	}
}

func TestCancelAndRejectAreAtomicTerminalCommands(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		h := newHarness(t, nil)
		incident, err := h.services.Incidents.Cancel(context.Background(), application.CancelIncidentCommand{IncidentID: h.incident.ID(), Actor: h.human, Reason: "operator canceled"})
		if err != nil {
			t.Fatal(err)
		}
		if incident.Status() != domain.StatusCanceled {
			t.Fatalf("status = %s", incident.Status())
		}
		assertLastEventKind(t, h, domain.EventIncidentCanceled)
	})
	t.Run("reject", func(t *testing.T) {
		h := awaitingApprovalHarness(t)
		proposal := mustServiceProposal(t, h)
		approval, err := h.services.Approvals.Reject(context.Background(), application.RejectionCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash(), Reason: "risk too high"})
		if err != nil {
			t.Fatal(err)
		}
		if approval.Decision() != domain.ApprovalRejected {
			t.Fatalf("decision = %s", approval.Decision())
		}
		assertIncidentStatus(t, h, domain.StatusRejected)
	})
}

func TestFailedRecoveryTerminatesIncident(t *testing.T) {
	h := approvedHarness(t)
	_, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute")
	if err != nil {
		t.Fatal(err)
	}
	verification, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err == nil || verification.Passed() {
		t.Fatalf("Verify() passed/error = %v/%v", verification.Passed(), err)
	}
	assertIncidentStatus(t, h, domain.StatusFailed)
	err = h.uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
		stored, getErr := repositories.Verifications().GetByIncident(h.incident.ID())
		if getErr != nil {
			return getErr
		}
		if stored.ExecutionID() != verification.ExecutionID() {
			return fmt.Errorf("stored verification execution = %q", stored.ExecutionID())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerificationAdapterFailureLeavesIncidentRetryable(t *testing.T) {
	h := approvedHarness(t)
	if _, err := h.services.Executions.Execute(context.Background(), h.incident.ID(), "execute"); err != nil {
		t.Fatal(err)
	}
	h.infrastructure.MetricErr = errors.New("metrics unavailable")
	_, err := h.services.Verifications.Verify(context.Background(), h.incident.ID())
	if err == nil {
		t.Fatal("Verify() error = nil, want metrics failure")
	}
	assertIncidentStatus(t, h, domain.StatusVerifying)
}

type phaseHarness struct {
	services       application.Services
	infrastructure *fake.Infrastructure
	uow            *memory.UnitOfWork
	clock          *testkit.Clock
	incident       domain.Incident
	human          domain.Actor
}

type harnessOverrides struct {
	inspector ports.DeploymentInspector
	metrics   ports.MetricsPort
	diagnoser ports.Diagnoser
	executor  ports.RollbackExecutor
}

func newHarness(t *testing.T, diagnoserOverride ports.Diagnoser) phaseHarness {
	return newHarnessWithOverrides(t, harnessOverrides{diagnoser: diagnoserOverride})
}

func newHarnessWithOverrides(t *testing.T, overrides harnessOverrides) phaseHarness {
	t.Helper()
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
		Deployment: domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, Healthy: false, RolloutAt: now.Add(-time.Minute)},
		Metric:     domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
		Now:        clock.Now,
	}
	selectedInspector := overrides.inspector
	if selectedInspector == nil {
		selectedInspector = infrastructure
	}
	selectedMetrics := overrides.metrics
	if selectedMetrics == nil {
		selectedMetrics = infrastructure
	}
	selectedDiagnoser := overrides.diagnoser
	if selectedDiagnoser == nil {
		selectedDiagnoser, err = diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
	}
	selectedExecutor := overrides.executor
	if selectedExecutor == nil {
		selectedExecutor = infrastructure
	}
	uow := memory.NewUnitOfWork()
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	systemActor, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork: uow, Inspector: selectedInspector, Metrics: selectedMetrics, Diagnoser: selectedDiagnoser,
		Executor: selectedExecutor, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: clock, IDGenerator: ids,
	}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: 5 * time.Second, SystemActor: systemActor})
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := services.Incidents.CreateIncident(context.Background(), application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: human})
	if err != nil {
		t.Fatal(err)
	}
	return phaseHarness{services: services, infrastructure: infrastructure, uow: uow, clock: clock, incident: incident, human: human}
}

func awaitingApprovalHarness(t *testing.T) phaseHarness {
	t.Helper()
	h := newHarness(t, nil)
	incident, err := h.services.Investigations.Investigate(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	h.incident = incident
	return h
}

func approvedHarness(t *testing.T) phaseHarness {
	t.Helper()
	h := awaitingApprovalHarness(t)
	proposal := mustServiceProposal(t, h)
	_, err := h.services.Approvals.Approve(context.Background(), application.ApprovalCommand{IncidentID: h.incident.ID(), Actor: h.human, ActionID: proposal.ActionID(), ProposalHash: proposal.Hash()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustServiceProposal(t *testing.T, h phaseHarness) domain.RollbackProposal {
	t.Helper()
	proposal, err := h.services.Queries.GetProposal(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

func assertIncidentStatus(t *testing.T, h phaseHarness, want domain.Status) {
	t.Helper()
	incident, err := h.services.Queries.GetIncident(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if incident.Status() != want {
		t.Fatalf("incident status = %s, want %s", incident.Status(), want)
	}
}

func assertLastEventKind(t *testing.T, h phaseHarness, want domain.EventKind) {
	t.Helper()
	events, err := h.services.Queries.ListEvents(context.Background(), h.incident.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Kind() != want {
		t.Fatalf("last event = %v, want %s", events, want)
	}
}

type invalidReferenceDiagnoser struct{}

func (invalidReferenceDiagnoser) Diagnose(_ context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	conclusion, err := domain.NewInvestigationConclusion("invalid-conclusion", input.Incident.ID(), domain.StrategyRules, "invalid reference", 0.20, []domain.EvidenceID{"missing"}, domain.RecommendNoAction, input.Now)
	if err != nil {
		return domain.InvestigationConclusion{}, fmt.Errorf("create invalid-reference conclusion: %w", err)
	}
	return conclusion, nil
}

type foreignIncidentDiagnoser struct{}

func (foreignIncidentDiagnoser) Diagnose(_ context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return domain.NewInvestigationConclusion("foreign-conclusion", "foreign-incident", domain.StrategyRules, "foreign conclusion", 0.95, []domain.EvidenceID{input.Evidence[0].ID(), input.Evidence[1].ID()}, domain.RecommendRollback, input.Now)
}

type cancelingInspector struct {
	cancel context.CancelFunc
}

func (c cancelingInspector) InspectDeployment(ctx context.Context, _ domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	c.cancel()
	return domain.DeploymentEvidence{}, ctx.Err()
}

type cancelingMetrics struct {
	cancel context.CancelFunc
}

func (c cancelingMetrics) Observe(ctx context.Context, _ domain.DeploymentTarget, _ domain.Symptom) (domain.MetricEvidence, error) {
	c.cancel()
	return domain.MetricEvidence{}, ctx.Err()
}

type cancelingDiagnoser struct {
	cancel context.CancelFunc
}

func (c cancelingDiagnoser) Diagnose(ctx context.Context, _ ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	c.cancel()
	return domain.InvestigationConclusion{}, ctx.Err()
}
