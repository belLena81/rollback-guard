package a2aadapter

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// Approval is never requested through the investigation task (docs/adr.md,
// ADR-01). The A2A states that pause a task for its caller are therefore a
// protocol violation, never a state to wait in.
var interruptedStates = []a2a.TaskState{a2a.TaskStateInputRequired, a2a.TaskStateAuthRequired}

// TestTaskStatesAreClassified lists every A2A task state, so a state added
// by an SDK upgrade fails here until it is classified on purpose.
func TestTaskStatesAreClassified(t *testing.T) {
	tests := []struct {
		state a2a.TaskState
		want  string // "running", "failed", "canceled" or "violation"
	}{
		{a2a.TaskStateUnspecified, "running"},
		{a2a.TaskStateSubmitted, "running"},
		{a2a.TaskStateWorking, "running"},
		{a2a.TaskStateCompleted, "running"},
		{a2a.TaskStateFailed, "failed"},
		{a2a.TaskStateRejected, "failed"},
		{a2a.TaskStateCanceled, "canceled"},
		{a2a.TaskStateInputRequired, "violation"},
		{a2a.TaskStateAuthRequired, "violation"},
	}
	for _, test := range tests {
		t.Run(test.state.String(), func(t *testing.T) {
			err := failureOfState(test.state)
			got := "failed"
			switch {
			case err == nil:
				got = "running"
			case errors.Is(err, ports.ErrInvestigationProtocolViolation):
				got = "violation"
			case errors.Is(err, context.Canceled):
				got = "canceled"
			}
			if got != test.want {
				t.Fatalf("failureOfState(%s) = %v (%s), want %s", test.state, err, got, test.want)
			}
		})
	}
}

// TestWatchRejectsInterruptedStates covers each way an interrupted state can
// reach the gateway: a status update on the stream, a task snapshot on the
// stream, and the GetTask read that Watch makes first and after a stream
// ends or breaks.
func TestWatchRejectsInterruptedStates(t *testing.T) {
	for _, state := range interruptedStates {
		t.Run(state.String(), func(t *testing.T) {
			watch := &taskWatch{ref: domain.InvestigationTaskRef{TaskID: "task-1", ContextID: "context-1"}}
			status := &a2a.TaskStatusUpdateEvent{TaskID: "task-1", ContextID: "context-1", Status: a2a.TaskStatus{State: state}}
			if _, err := watch.observe(context.Background(), status); !errors.Is(err, ports.ErrInvestigationProtocolViolation) {
				t.Fatalf("status update error = %v, want protocol violation", err)
			}
			snapshot := &a2a.Task{ID: "task-1", ContextID: "context-1", Status: a2a.TaskStatus{State: state}}
			if _, err := watch.observe(context.Background(), snapshot); !errors.Is(err, ports.ErrInvestigationProtocolViolation) {
				t.Fatalf("task snapshot error = %v, want protocol violation", err)
			}
			if _, err := resultFromTask(snapshot); !errors.Is(err, ports.ErrInvestigationProtocolViolation) {
				t.Fatalf("GetTask result error = %v, want protocol violation", err)
			}
			if !stopsWatch(state) {
				t.Fatalf("Watch would keep waiting on %s", state)
			}
		})
	}
}

// TestGatewayWatchFailsFastWhenInvestigatorAsksForInput runs the real REST
// gateway against an A2A server whose agent pauses the task for its caller.
// Watch must fail with a protocol violation well before the task timeout,
// instead of waiting for input that the orchestrator never gives.
func TestGatewayWatchFailsFastWhenInvestigatorAsksForInput(t *testing.T) {
	for _, state := range interruptedStates {
		t.Run(state.String(), func(t *testing.T) {
			gateway := serveInterruptingAgent(t, state)
			incident := interruptTestIncident(t)
			ref, err := gateway.Submit(context.Background(), ports.InvestigationSubmission{
				Incident: incident, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1",
				MessageID: "message-1", TraceID: "trace-1", RequestHash: application.InvestigationRequestHash(incident),
			})
			if err != nil {
				t.Fatal(err)
			}
			const taskTimeout = 5 * time.Second
			ctx, cancel := context.WithTimeout(context.Background(), taskTimeout)
			defer cancel()
			started := time.Now()
			_, err = gateway.Watch(ctx, ref, nil)
			if !errors.Is(err, ports.ErrInvestigationProtocolViolation) {
				t.Fatalf("watch error = %v, want protocol violation", err)
			}
			if elapsed := time.Since(started); elapsed > taskTimeout/2 {
				t.Fatalf("watch took %s, want a fast failure", elapsed)
			}
		})
	}
}

// TestInvestigatorEmitsOnlyLifecycleStates drives the investigator's executor
// through every outcome and checks that it never pauses a task for its
// caller: the investigator has nothing to ask the orchestrator.
func TestInvestigatorEmitsOnlyLifecycleStates(t *testing.T) {
	allowed := map[a2a.TaskState]bool{
		a2a.TaskStateSubmitted: true, a2a.TaskStateWorking: true, a2a.TaskStateCompleted: true,
		a2a.TaskStateFailed: true, a2a.TaskStateCanceled: true,
	}
	incident := interruptTestIncident(t)
	valid := interruptTestResult(t, incident)
	succeeding := investigatorFunc(func(_ context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
		for _, stage := range []domain.InvestigationStage{domain.InvestigationStageDeployment, domain.InvestigationStageMetrics, domain.InvestigationStageDiagnosis, domain.InvestigationStageFinalizing} {
			if err := request.Progress(stage); err != nil {
				return ports.InvestigationResult{}, err
			}
		}
		return valid, nil
	})
	failing := investigatorFunc(func(context.Context, ports.InvestigationRequest) (ports.InvestigationResult, error) {
		return ports.InvestigationResult{}, errors.New("evidence collection failed")
	})
	hash := application.InvestigationRequestHash(incident)
	tests := []struct {
		name         string
		investigator ports.Investigator
		requestHash  string
		badPayload   bool
		cancel       bool
		wantFinal    a2a.TaskState
	}{
		{name: "success with progress", investigator: succeeding, requestHash: hash, wantFinal: a2a.TaskStateCompleted},
		{name: "investigator error", investigator: failing, requestHash: hash, wantFinal: a2a.TaskStateFailed},
		{name: "request hash mismatch", investigator: succeeding, requestHash: "other-hash", wantFinal: a2a.TaskStateFailed},
		{name: "invalid request", investigator: succeeding, requestHash: hash, badPayload: true, wantFinal: a2a.TaskStateFailed},
		{name: "cancel", investigator: succeeding, requestHash: hash, cancel: true, wantFinal: a2a.TaskStateCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &investigationExecutor{investigator: test.investigator, timeout: time.Second, logger: slog.Default(), tracer: ports.NoopTracer{}}
			execCtx := interruptTestExecContext(t, incident, test.requestHash, test.badPayload)
			events := executor.Execute(context.Background(), execCtx)
			if test.cancel {
				events = executor.Cancel(context.Background(), execCtx)
			}
			states := emittedStates(t, events)
			if len(states) == 0 || states[len(states)-1] != test.wantFinal {
				t.Fatalf("states = %v, want final %s", states, test.wantFinal)
			}
			for _, state := range states {
				if !allowed[state] {
					t.Fatalf("investigator emitted %s (states %v)", state, states)
				}
			}
		})
	}
}

func emittedStates(t *testing.T, events iter.Seq2[a2a.Event, error]) []a2a.TaskState {
	t.Helper()
	var states []a2a.TaskState
	for event, err := range events {
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.(type) {
		case *a2a.Task:
			states = append(states, value.Status.State)
		case *a2a.TaskStatusUpdateEvent:
			states = append(states, value.Status.State)
		}
	}
	return states
}

// interruptingAgent is a stand-in investigator that pauses every task for its
// caller, which the real investigator never does.
type interruptingAgent struct{ state a2a.TaskState }

func (a interruptingAgent) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a.state, nil), nil)
	}
}

func (interruptingAgent) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func serveInterruptingAgent(t *testing.T, state a2a.TaskState) *Gateway {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	publicURL := "http://" + listener.Addr().String()
	handler := a2asrv.NewHandler(interruptingAgent{state: state}, a2asrv.WithTaskStore(taskstore.NewInMemory(nil)))
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(AgentCard(publicURL)))
	mux.Handle("/a2a/", http.StripPrefix("/a2a", a2asrv.NewRESTHandler(handler)))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }() //nolint:errcheck // shutdown closes the listener
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	gateway, err := NewGateway(context.Background(), publicURL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gateway.Close(); err != nil {
			t.Error(err)
		}
	})
	return gateway
}

func interruptTestIncident(t *testing.T) domain.Incident {
	t.Helper()
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
	return incident
}

func interruptTestResult(t *testing.T, incident domain.Incident) ports.InvestigationResult {
	t.Helper()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	deployment := domain.DeploymentEvidence{Target: incident.Target(), CurrentRevision: "rev-2", PreviousStableRevision: "rev-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-2 * time.Minute)}
	evidence, err := domain.NewDeploymentEvidence("evidence-1", incident.ID(), deployment, now)
	if err != nil {
		t.Fatal(err)
	}
	conclusion, err := domain.NewInvestigationConclusion("conclusion-1", incident.ID(), domain.StrategyRules, "new revision is unhealthy", 0.98, []domain.EvidenceID{"evidence-1"}, domain.RecommendRollback, now)
	if err != nil {
		t.Fatal(err)
	}
	return ports.InvestigationResult{Deployment: deployment, Evidence: []domain.Evidence{evidence}, Conclusion: conclusion}
}

func interruptTestExecContext(t *testing.T, incident domain.Incident, requestHash string, badPayload bool) *a2asrv.ExecutorContext {
	t.Helper()
	part := a2a.NewDataPart(encodeRequest(incident))
	if badPayload {
		part = a2a.NewDataPart(map[string]any{"schemaVersion": SchemaVersion, "unexpected": true})
	}
	part.MediaType = JSONMediaType
	message := a2a.NewMessage(a2a.MessageRoleUser, part)
	correlation, err := dataMap(correlationDTO{SchemaVersion: SchemaVersion, OperationID: "operation-1", IdempotencyKey: "investigation:incident-1", TraceID: "trace-1", RequestHash: requestHash})
	if err != nil {
		t.Fatal(err)
	}
	return &a2asrv.ExecutorContext{Message: message, TaskID: "task-states", ContextID: "context-states", Metadata: map[string]any{ExtensionURI: correlation}}
}
