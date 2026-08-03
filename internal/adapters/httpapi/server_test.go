package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestHTTPWorkflowCreatesApprovesExecutesAndVerifies(t *testing.T) {
	handler := newTestHandler(t)
	body := []byte(`{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
	}`)
	create := httptest.NewRequest(http.MethodPost, "/v1/incidents", bytes.NewReader(body))
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", createResponse.Code, createResponse.Body.String())
	}
	var created incidentSummaryDTO
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Incident.Status != "awaiting_approval" || created.Proposal == nil {
		t.Fatalf("created status/proposal = %s/%v", created.Incident.Status, created.Proposal)
	}
	approveBody, err := json.Marshal(approvalRequest{ActionID: created.Proposal.ActionID, ProposalHash: created.Proposal.ProposalHash})
	if err != nil {
		t.Fatal(err)
	}
	approve := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+created.Incident.ID+"/approve", bytes.NewReader(approveBody))
	approveResponse := httptest.NewRecorder()
	handler.ServeHTTP(approveResponse, approve)
	if approveResponse.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", approveResponse.Code, approveResponse.Body.String())
	}
	var approved incidentSummaryDTO
	if err := json.Unmarshal(approveResponse.Body.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	if approved.Incident.Status != "resolved" || approved.Execution == nil || approved.Verification == nil || !approved.Verification.Passed {
		t.Fatalf("approved summary = %+v", approved)
	}
	events := httptest.NewRequest(http.MethodGet, "/v1/incidents/"+created.Incident.ID+"/events", nil)
	eventsResponse := httptest.NewRecorder()
	handler.ServeHTTP(eventsResponse, events)
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("events status = %d body = %s", eventsResponse.Code, eventsResponse.Body.String())
	}
	var eventPayload struct {
		Events []eventDTO `json:"events"`
	}
	if err := json.Unmarshal(eventsResponse.Body.Bytes(), &eventPayload); err != nil {
		t.Fatal(err)
	}
	if len(eventPayload.Events) == 0 || eventPayload.Events[0].Sequence != 1 {
		t.Fatalf("events not ordered: %+v", eventPayload.Events)
	}
}

func TestStrictJSONRejectsUnknownFields(t *testing.T) {
	handler := newTestHandler(t)
	body := []byte(`{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api","extra":"no"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
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
	infrastructure.AfterExecute = func() {
		infrastructure.Metric.Value = 0.02
	}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	systemActor, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork:     memory.NewUnitOfWork(),
		Inspector:      infrastructure,
		Metrics:        infrastructure,
		Diagnoser:      diagnoser,
		Executor:       infrastructure,
		RollbackPolicy: policy,
		ApprovalPolicy: domain.ApprovalAuthorizer{},
		Clock:          clock,
		IDGenerator:    ids,
	}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: 5 * time.Second, SystemActor: systemActor})
	if err != nil {
		t.Fatal(err)
	}
	localActor, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(services, localActor, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server.Routes(nil)
}
