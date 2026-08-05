package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
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
	retry := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+created.Incident.ID+"/approve", bytes.NewReader(approveBody))
	retryResponse := httptest.NewRecorder()
	handler.ServeHTTP(retryResponse, retry)
	if retryResponse.Code != http.StatusOK {
		t.Fatalf("retry approve status = %d body = %s", retryResponse.Code, retryResponse.Body.String())
	}
	var retried incidentSummaryDTO
	if err := json.Unmarshal(retryResponse.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Incident.Status != "resolved" || retried.Execution == nil || retried.Verification == nil || !retried.Verification.Passed {
		t.Fatalf("retried summary = %+v", retried)
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

func TestHTTPCreateReturnsAcceptedQueuedInvestigation(t *testing.T) {
	server := newTestServer(t, nil)
	actor, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := httpserver.NewLocalAuthMiddleware(actor)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Routes(nil, 0, auth)
	body := []byte(`{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create status = %d body = %s", response.Code, response.Body.String())
	}
	var created incidentSummaryDTO
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Incident.Status != "investigating" || created.Investigation == nil || created.Investigation.State != "queued" || created.Proposal != nil {
		t.Fatalf("created summary = %+v", created)
	}
	if response.Header().Get("Location") != "/v1/incidents/"+created.Incident.ID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
}

func TestHTTPApproveReturnsCommittedExecutionFailureSummary(t *testing.T) {
	handler := newTestHandlerWithInfrastructure(t, func(infrastructure *fake.Infrastructure) {
		infrastructure.ExecuteErr = errors.New("mcp rollback failed")
	})
	created := createHTTPIncident(t, handler)
	approved := approveHTTPProposal(t, handler, created)
	if approved.Incident.Status != "failed" || approved.Execution == nil || approved.Execution.Outcome != "failed" {
		t.Fatalf("approval summary = %+v", approved)
	}
	if approved.Verification != nil {
		t.Fatalf("verification = %+v, want nil", approved.Verification)
	}
}

func TestHTTPApproveReturnsCommittedVerificationFailureSummary(t *testing.T) {
	handler := newTestHandlerWithInfrastructure(t, func(infrastructure *fake.Infrastructure) {
		infrastructure.AfterExecute = nil
	})
	created := createHTTPIncident(t, handler)
	approved := approveHTTPProposal(t, handler, created)
	if approved.Incident.Status != "failed" || approved.Execution == nil || approved.Execution.Outcome != "succeeded" {
		t.Fatalf("approval summary = %+v", approved)
	}
	if approved.Verification == nil || approved.Verification.Passed {
		t.Fatalf("verification = %+v, want failed verification", approved.Verification)
	}
}

func TestOpenAPIRejectionRequestIsStrictSingleObject(t *testing.T) {
	data, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	rejectionStart := strings.Index(schema, "    RejectionRequest:\n")
	if rejectionStart < 0 {
		t.Fatal("RejectionRequest schema not found")
	}
	cancelStart := strings.Index(schema[rejectionStart:], "    CancelRequest:\n")
	if cancelStart < 0 {
		t.Fatal("CancelRequest schema not found after RejectionRequest")
	}
	rejectionSchema := schema[rejectionStart : rejectionStart+cancelStart]
	if strings.Contains(rejectionSchema, "allOf:") {
		t.Fatalf("RejectionRequest must not use allOf with additionalProperties: false:\n%s", rejectionSchema)
	}
	for _, required := range []string{
		"required: [actionId, proposalHash, reason]",
		"additionalProperties: false",
		"actionId: { type: string }",
		"proposalHash: { type: string }",
		"reason: { type: string }",
	} {
		if !strings.Contains(rejectionSchema, required) {
			t.Fatalf("RejectionRequest missing %q:\n%s", required, rejectionSchema)
		}
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

func TestListEventsReturnsNotFoundForMissingIncident(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/v1/incidents/missing/events", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"not_found"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestMutatingRouteRequiresRequestActor(t *testing.T) {
	handler := newUnauthenticatedTestHandler(t)
	body := []byte(`{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"error":"unauthenticated"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestReadinessUsesConfiguredTimeout(t *testing.T) {
	timeout := 25 * time.Millisecond
	server := newTestServer(t, nil)
	var observed time.Duration
	handler := server.Routes(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("readiness context has no deadline")
		}
		observed = time.Until(deadline)
		return nil
	}, timeout, nil)
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if observed <= 0 || observed > timeout {
		t.Fatalf("readiness timeout = %s, want within configured %s", observed, timeout)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWithInfrastructure(t, nil)
}

func newTestHandlerWithInfrastructure(t *testing.T, configure func(*fake.Infrastructure)) http.Handler {
	t.Helper()
	server := newTestServer(t, configure)
	localActor, err := domain.NewActor("local-admin", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := httpserver.NewLocalAuthMiddleware(localActor)
	if err != nil {
		t.Fatal(err)
	}
	return synchronousInvestigationHandler{next: server.Routes(nil, 0, auth), server: server}
}

type synchronousInvestigationHandler struct {
	next   http.Handler
	server *Server
}

func (h synchronousInvestigationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/incidents" {
		h.next.ServeHTTP(w, r)
		return
	}
	recorder := httptest.NewRecorder()
	h.next.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusAccepted {
		copyRecordedResponse(w, recorder)
		return
	}
	var accepted incidentSummaryDTO
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil {
		copyRecordedResponse(w, recorder)
		return
	}
	if _, err := h.server.services.Investigations.Investigate(r.Context(), domain.IncidentID(accepted.Incident.ID)); err != nil {
		copyRecordedResponse(w, recorder)
		return
	}
	summary, err := h.server.services.Queries.GetIncidentSummary(r.Context(), domain.IncidentID(accepted.Incident.ID))
	if err != nil {
		copyRecordedResponse(w, recorder)
		return
	}
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	writeJSON(w, http.StatusCreated, summaryResponse(summary))
}
func copyRecordedResponse(w http.ResponseWriter, recorder *httptest.ResponseRecorder) {
	for key, values := range recorder.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(recorder.Code)
	if _, err := w.Write(recorder.Body.Bytes()); err != nil {
		return
	}
}

func newUnauthenticatedTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestServer(t, nil).Routes(nil, 0, nil)
}

func newTestServer(t *testing.T, configure func(*fake.Infrastructure)) *Server {
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
	if configure != nil {
		configure(infrastructure)
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
	server, err := NewServer(services, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func createHTTPIncident(t *testing.T, handler http.Handler) incidentSummaryDTO {
	t.Helper()
	body := []byte(`{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
	}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", response.Code, response.Body.String())
	}
	var created incidentSummaryDTO
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created
}

func approveHTTPProposal(t *testing.T, handler http.Handler, created incidentSummaryDTO) incidentSummaryDTO {
	t.Helper()
	if created.Proposal == nil {
		t.Fatal("created incident has no proposal")
	}
	body, err := json.Marshal(approvalRequest{ActionID: created.Proposal.ActionID, ProposalHash: created.Proposal.ProposalHash})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+created.Incident.ID+"/approve", bytes.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("approve status = %d body = %s", response.Code, response.Body.String())
	}
	var approved incidentSummaryDTO
	if err := json.Unmarshal(response.Body.Bytes(), &approved); err != nil {
		t.Fatal(err)
	}
	return approved
}
