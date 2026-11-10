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
// the request fails with the fixed internal_error body and the incident stays
// executing, so a retry of the same approval can reconcile it.
func TestHTTPApproveUnknownRollbackOutcomeLeavesIncidentExecuting(t *testing.T) {
	handler := newTestHandlerWithInfrastructure(t, func(infrastructure *fake.Infrastructure) {
		infrastructure.ExecuteCallErr = ports.ErrRollbackOutcomeUnknown
	})
	created := createHTTPIncident(t, handler)

	response := postApproval(t, handler, created, created.Proposal.ProposalHash)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if body := decodeErrorBody(t, response); body.Error != "internal_error" || body.Message != messageInternal || body.RequestID != "req-approve" {
		t.Fatalf("body = %+v", body)
	}
	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/incidents/"+created.Incident.ID, nil))
	var summary incidentSummaryDTO
	if err := json.Unmarshal(get.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Incident.Status != "executing" || summary.Execution != nil {
		t.Fatalf("summary = %+v, want executing with no recorded execution", summary)
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
