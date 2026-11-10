package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// The approve route returns the use case's outcome unchanged: a conflict gets
// the fixed conflict body, never the domain error text.
func TestHTTPApproveConflictReturnsFixedMessage(t *testing.T) {
	handler := newTestHandler(t)
	created := createHTTPIncident(t, handler)

	response := postApproval(t, handler, created, "sha256-not-the-proposal")

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeErrorBody(t, response)
	if body.Error != "conflict" || body.Message != messageApprovalMismatch || body.RequestID != "req-approve" {
		t.Fatalf("body = %+v", body)
	}
}

// A rollback whose outcome is unknown is not reported as a recorded failure:
// the request fails with 503 rollback_outcome_unknown and Retry-After, and
// the incident stays executing, so a retry of the same approval can
// reconcile it.
func TestHTTPApproveUnknownRollbackOutcomeLeavesIncidentExecuting(t *testing.T) {
	var infra *fake.Infrastructure
	handler := newTestHandlerWithInfrastructure(t, func(infrastructure *fake.Infrastructure) {
		infrastructure.ExecuteCallErr = ports.ErrRollbackOutcomeUnknown
		infra = infrastructure
	})
	created := createHTTPIncident(t, handler)

	response := postApproval(t, handler, created, created.Proposal.ProposalHash)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != rollbackOutcomeRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, rollbackOutcomeRetryAfter)
	}
	if body := decodeErrorBody(t, response); body.Error != "rollback_outcome_unknown" || body.Message != messageRollbackOutcomeUnknown || body.RequestID != "req-approve" {
		t.Fatalf("body = %+v", body)
	}
	assertSummaryStatus(t, handler, created.Incident.ID, "executing", false)

	// Retrying the same approval once the executor answers reconciles the
	// pending attempt; it is not refused as a duplicate approval.
	infra.ExecuteCallErr = nil
	retry := postApproval(t, handler, created, created.Proposal.ProposalHash)
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d body = %s", retry.Code, retry.Body.String())
	}
	if infra.ExecutionCount() != 1 {
		t.Fatalf("executions = %d, want exactly one mutation", infra.ExecutionCount())
	}
}

func assertSummaryStatus(t *testing.T, handler http.Handler, id, wantStatus string, wantExecution bool) {
	t.Helper()
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/incidents/"+id, nil))
	var summary incidentSummaryDTO
	if err := json.Unmarshal(get.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status != wantStatus || (summary.Execution != nil) != wantExecution {
		t.Fatalf("summary = %+v, want status %s with execution recorded = %v", summary, wantStatus, wantExecution)
	}
}

func TestHTTPInvalidJSONCarriesRequestID(t *testing.T) {
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/incident-1/approve", bytes.NewReader([]byte(`{"actionId":`)))
	request.Header.Set("X-Request-Id", "req-json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if body := decodeErrorBody(t, response); body.Error != "invalid_json" || body.RequestID != "req-json" {
		t.Fatalf("body = %+v", body)
	}
}

func postApproval(t *testing.T, handler http.Handler, created incidentSummaryDTO, proposalHash string) *httptest.ResponseRecorder {
	t.Helper()
	if created.Proposal == nil {
		t.Fatal("created incident has no proposal")
	}
	body, err := json.Marshal(approvalRequest{ActionID: created.Proposal.ActionID, ProposalHash: proposalHash})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/"+created.Incident.ID+"/approve", bytes.NewReader(body))
	request.Header.Set("X-Request-Id", "req-approve")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
