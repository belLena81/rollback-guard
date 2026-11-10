package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestWriteErrorNeverEchoesWrappedErrorText(t *testing.T) {
	const secret = "postgres://guardian:hunter2@db:5432/rollback?sslmode=disable: connection refused"
	leak := errors.New(secret)
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{name: "illegal transition", err: errors.Join(&domain.IllegalTransitionError{From: domain.StatusResolved, To: domain.StatusExecuting}, leak), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messageIllegalTransition},
		{name: "version conflict", err: fmt.Errorf("start rollback execution: %w", errors.Join(&domain.VersionConflictError{Expected: 3, Actual: 4}, leak)), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messageVersionConflict},
		{name: "idempotency conflict", err: errors.Join(&domain.IdempotencyConflictError{Key: "approval:approval-1"}, leak), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messageIdempotency},
		{name: "approval mismatch with failed cleanup", err: errors.Join(&domain.ApprovalMismatchError{Reason: "deployment identity or current revision changed"}, leak), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messageApprovalMismatch},
		{name: "expired with failed cleanup", err: errors.Join(&domain.ExpiryError{Resource: "approval"}, leak), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messageExpired},
		{name: "policy denied", err: errors.Join(&domain.PolicyDenialError{Rule: "policy_decision_binding"}, leak), wantStatus: http.StatusConflict, wantCode: "conflict", wantMessage: messagePolicyDenied},
		{name: "non-human approver", err: errors.Join(&domain.NonHumanApprovalError{Kind: domain.ActorAgent}, leak), wantStatus: http.StatusForbidden, wantCode: "forbidden", wantMessage: messageForbidden},
		{name: "validation keeps only the typed message", err: fmt.Errorf("record approval decision: %w", errors.Join(&domain.ValidationError{Field: "approval", Reason: "required value is empty"}, leak)), wantStatus: http.StatusBadRequest, wantCode: "validation_failed", wantMessage: "approval: required value is empty"},
		{name: "not found keeps only the typed message", err: errors.Join(&domain.NotFoundError{Resource: "incident", ID: "incident-9"}, leak), wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: `incident "incident-9" not found`},
		{name: "bare validation sentinel", err: fmt.Errorf("decode %s: %w", secret, domain.ErrValidation), wantStatus: http.StatusBadRequest, wantCode: "validation_failed", wantMessage: messageValidation},
		{name: "bare not-found sentinel", err: fmt.Errorf("lookup %s: %w", secret, domain.ErrNotFound), wantStatus: http.StatusNotFound, wantCode: "not_found", wantMessage: messageNotFound},
		{name: "internal", err: leak, wantStatus: http.StatusInternalServerError, wantCode: "internal_error", wantMessage: messageInternal},
		{name: "unknown rollback outcome", err: fmt.Errorf("execute approved rollback: %w", errors.Join(ports.ErrRollbackOutcomeUnknown, leak)), wantStatus: http.StatusServiceUnavailable, wantCode: "rollback_outcome_unknown", wantMessage: messageRollbackOutcomeUnknown},
		// The outcome-unknown classification wins over any domain error
		// joined into the same chain: the client must retry, not give up.
		{name: "unknown rollback outcome joined with a conflict", err: errors.Join(ports.ErrRollbackOutcomeUnknown, &domain.VersionConflictError{Expected: 1, Actual: 2}), wantStatus: http.StatusServiceUnavailable, wantCode: "rollback_outcome_unknown", wantMessage: messageRollbackOutcomeUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			server := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			recorder := httptest.NewRecorder()

			server.writeError(recorder, requestWithID(t, "req-42"), test.err)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
			body := decodeErrorBody(t, recorder)
			if body.Error != test.wantCode || body.Message != test.wantMessage || body.RequestID != "req-42" {
				t.Fatalf("body = %+v, want %s/%q/req-42", body, test.wantCode, test.wantMessage)
			}
			for _, output := range []string{recorder.Body.String(), logs.String()} {
				if strings.Contains(output, "hunter2") || strings.Contains(output, "postgres://") {
					t.Fatalf("error text leaked: %s", output)
				}
			}
			if test.wantStatus >= http.StatusInternalServerError && !strings.Contains(logs.String(), `"request_id":"req-42"`) {
				t.Fatalf("server error not logged with the request ID: %s", logs.String())
			}
			wantRetryAfter := ""
			if test.wantCode == "rollback_outcome_unknown" {
				wantRetryAfter = rollbackOutcomeRetryAfter
			}
			if got := recorder.Header().Get("Retry-After"); got != wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, wantRetryAfter)
			}
		})
	}
}

func TestServerWriteJSONCorrelatesAndLogsEncodeFailure(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	recorder := httptest.NewRecorder()

	server.writeJSON(recorder, requestWithID(t, "req-encode"), http.StatusOK, map[string]any{"unencodable": make(chan int)})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	body := decodeErrorBody(t, recorder)
	if body.Error != "internal_error" || body.Message != "internal server error" || body.RequestID != "req-encode" {
		t.Fatalf("body = %+v, want the correlated internal_error body", body)
	}
	if !strings.Contains(logs.String(), "encode response failed") || !strings.Contains(logs.String(), `"request_id":"req-encode"`) {
		t.Fatalf("encode failure not logged with the request ID: %s", logs.String())
	}
}

func TestRequestIDIsBoundedAndPlain(t *testing.T) {
	for name, test := range map[string]struct{ header, want string }{
		"absent":         {header: "", want: "local"},
		"plain":          {header: "req-42_a.b:c", want: "req-42_a.b:c"},
		"too long":       {header: strings.Repeat("a", maxRequestIDLength+1), want: "invalid"},
		"control and ws": {header: "req 1\tforged=1", want: "invalid"},
		"quotes":         {header: `req"}`, want: "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			request := requestWithID(t, test.header)
			if got := requestID(request); got != test.want {
				t.Fatalf("request ID = %q, want %q", got, test.want)
			}
		})
	}
}

func requestWithID(t *testing.T, id string) *http.Request {
	t.Helper()
	var captured *http.Request
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents/incident-1/approve", nil)
	request.Header.Set("X-Request-Id", id)
	requestIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { captured = r })).ServeHTTP(httptest.NewRecorder(), request)
	return captured
}

func decodeErrorBody(t *testing.T, recorder *httptest.ResponseRecorder) errorDTO {
	t.Helper()
	var body errorDTO
	decoder := json.NewDecoder(bytes.NewReader(recorder.Body.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func TestWriteJSONReportsEncodeFailureAsInternalError(t *testing.T) {
	recorder := httptest.NewRecorder()

	writeJSON(recorder, http.StatusOK, map[string]any{"status": "ok", "unencodable": make(chan int)})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var body errorDTO
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	if body.Error != "internal_error" || body.Message != "internal server error" {
		t.Fatalf("error body = %+v, want the internal_error contract body", body)
	}
}

func TestWriteJSONWritesStatusAndBody(t *testing.T) {
	recorder := httptest.NewRecorder()

	writeJSON(recorder, http.StatusAccepted, map[string]string{"status": "ok"})

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if got, want := recorder.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
