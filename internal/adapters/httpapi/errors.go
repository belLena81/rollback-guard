package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// Error codes of the error body (api/openapi.yaml, components.schemas.Error).
// Every code a response can carry is listed in errorCodes, which a test pins
// against the OpenAPI enum.
const (
	codeValidation             = "validation_failed"
	codeInvalidJSON            = "invalid_json"
	codeUnauthenticated        = "unauthenticated"
	codeForbidden              = "forbidden"
	codeNotFound               = "not_found"
	codeConflict               = "conflict"
	codeRollbackOutcomeUnknown = "rollback_outcome_unknown"
	codeInternal               = "internal_error"
	codeBadGateway             = "bad_gateway"
)

var errorCodes = []string{
	codeValidation,
	codeInvalidJSON,
	codeUnauthenticated,
	codeForbidden,
	codeNotFound,
	codeConflict,
	codeRollbackOutcomeUnknown,
	codeInternal,
	codeBadGateway,
}

type errorDTO struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

type requestIDKey struct{}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		switch {
		case requestID == "":
			requestID = "local"
		case !validRequestID(requestID):
			// Client-controlled and echoed into bodies and logs: keep it
			// bounded and plain rather than trusting arbitrary header text.
			requestID = "invalid"
		}
		w.Header().Set("X-Request-Id", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID)))
	})
}

// maxRequestIDLength bounds a client-supplied X-Request-Id.
const maxRequestIDLength = 128

func validRequestID(id string) bool {
	if len(id) > maxRequestIDLength {
		return false
	}
	for _, c := range id {
		if !requestIDRune(c) {
			return false
		}
	}
	return true
}

func requestIDRune(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return c == '-' || c == '_' || c == '.' || c == ':'
	}
}

func requestID(r *http.Request) string {
	value := r.Context().Value(requestIDKey{})
	if requestID, ok := value.(string); ok {
		return requestID
	}
	return ""
}

// Fixed response messages. Conflict-class and internal errors never echo
// err.Error(): the chain can carry joined cleanup failures with database or
// transport text that must not reach a client.
const (
	messageInternal          = "internal server error"
	messageValidation        = "request validation failed"
	messageNotFound          = "resource not found"
	messageForbidden         = "only a human actor can decide approvals"
	messageRoleForbidden     = "your role does not allow this operation"
	messageLastAdministrator = "the change would leave no active administrator"
	messageUnauthenticated   = "authentication required"
	messageIllegalTransition = "the incident is not in a state that allows this operation"
	messageVersionConflict   = "the incident was changed by another request; reload and retry"
	messageIdempotency       = "the request conflicts with an earlier request for the same operation"
	messageAlreadyExists     = "the record already exists"
	messageApprovalMismatch  = "the approval does not match the current proposal or deployment"
	messageExpired           = "the proposal or approval has expired"
	messagePolicyDenied      = "the rollback policy denied this action"
	// messageRollbackOutcomeUnknown tells the caller what to do: the attempt
	// is still pending and only a retry of the same approval resolves it.
	messageRollbackOutcomeUnknown = "the rollback outcome is unknown; retry the same approval to reconcile it"
)

// rollbackOutcomeRetryAfter is the Retry-After value (seconds) sent with
// rollback_outcome_unknown.
const rollbackOutcomeRetryAfter = "5"

// errorResponse maps err to a status, an error code, and a client message.
// Validation and not-found messages come from the typed domain error alone
// (never the wrapped chain), so they describe the client's input and nothing
// else; every other class gets a fixed message.
func errorResponse(err error) (int, string, string) {
	var validation *domain.ValidationError
	var notFound *domain.NotFoundError
	switch {
	// First: an unknown rollback outcome must reach the client as "retry",
	// whatever else is joined into the chain. 503 because the condition is
	// temporary and the server asks for the identical request again; that
	// retry is safe because it reuses the approval's idempotency key, so the
	// rollback MCP's ledger allows at most one mutation. A 500 would read as
	// a server defect that a client should not blindly retry, a 409 as a
	// request the client must change, and a 202 would promise a completion
	// that nothing performs without the retry.
	case errors.Is(err, ports.ErrRollbackOutcomeUnknown):
		return http.StatusServiceUnavailable, codeRollbackOutcomeUnknown, messageRollbackOutcomeUnknown
	case errors.As(err, &validation):
		return http.StatusBadRequest, codeValidation, validation.Error()
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest, codeValidation, messageValidation
	case errors.As(err, &notFound):
		return http.StatusNotFound, codeNotFound, notFound.Error()
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, codeNotFound, messageNotFound
	// Authentication and authorization come before every conflict class: a
	// caller who may not act learns nothing about the incident's state.
	case errors.Is(err, domain.ErrUnauthenticated):
		return http.StatusUnauthorized, codeUnauthenticated, messageUnauthenticated
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, codeForbidden, messageRoleForbidden
	case errors.Is(err, domain.ErrNonHumanApproval):
		return http.StatusForbidden, codeForbidden, messageForbidden
	case errors.Is(err, domain.ErrIllegalTransition):
		return http.StatusConflict, codeConflict, messageIllegalTransition
	case errors.Is(err, domain.ErrVersionConflict):
		return http.StatusConflict, codeConflict, messageVersionConflict
	case errors.Is(err, domain.ErrAlreadyExists):
		return http.StatusConflict, codeConflict, messageAlreadyExists
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return http.StatusConflict, codeConflict, messageIdempotency
	case errors.Is(err, domain.ErrApprovalMismatch):
		return http.StatusConflict, codeConflict, messageApprovalMismatch
	case errors.Is(err, domain.ErrExpired):
		return http.StatusConflict, codeConflict, messageExpired
	case errors.Is(err, domain.ErrPolicyDenied):
		return http.StatusConflict, codeConflict, messagePolicyDenied
	case errors.Is(err, domain.ErrLastAdministrator):
		return http.StatusConflict, codeConflict, messageLastAdministrator
	default:
		return http.StatusInternalServerError, codeInternal, messageInternal
	}
}

// writeError answers err with a fixed-shape error body carrying the request
// ID. Internal errors are logged with a fixed classification, never with the
// error text, which may hold infrastructure details.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := errorResponse(err)
	switch code {
	case codeRollbackOutcomeUnknown:
		// The attempt stays pending; retrying the same approval reconciles it.
		w.Header().Set("Retry-After", rollbackOutcomeRetryAfter)
		s.logger.ErrorContext(r.Context(), "request failed", "status", status, "cause", codeRollbackOutcomeUnknown, "request_id", requestID(r))
	case codeInternal:
		s.logger.ErrorContext(r.Context(), "request failed", "status", status, "cause", "internal", "request_id", requestID(r))
	case codeUnauthenticated:
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	s.writeJSON(w, r, status, errorDTO{Error: code, Message: message, RequestID: requestID(r)})
}

// writeJSON encodes value for r. An encode failure becomes a correlated,
// logged internal_error response instead of a success status with a broken
// body.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	ctx, id := r.Context(), requestID(r)
	writeJSONWith(w, status, value, id, func(err error) {
		s.logger.ErrorContext(ctx, "encode response failed", "status", status, "request_id", id, "error", err)
	})
}

// writeJSON writes value where no server or request is in scope (fixed
// bodies that cannot fail to encode, and tests).
func writeJSON(w http.ResponseWriter, status int, value any) {
	writeJSONWith(w, status, value, "", func(error) {})
}

// encodeFailureBody is the last-resort internal_error body. It is a constant
// so that writing it cannot fail to encode.
const encodeFailureBody = `{"error":"internal_error","message":"internal server error"}` + "\n"

// writeJSONWith encodes before writing the status line, so an encode failure
// is reported as a 500 carrying requestID, and passed to onEncodeFailure.
func writeJSONWith(w http.ResponseWriter, status int, value any, requestID string, onEncodeFailure func(error)) {
	w.Header().Set("Content-Type", "application/json")
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		onEncodeFailure(err)
		status = http.StatusInternalServerError
		body.Reset()
		if encodeErr := json.NewEncoder(&body).Encode(errorDTO{Error: codeInternal, Message: messageInternal, RequestID: requestID}); encodeErr != nil {
			body.Reset()
			body.WriteString(encodeFailureBody)
		}
	}
	w.WriteHeader(status)
	if _, err := w.Write(body.Bytes()); err != nil {
		// The status line is already sent and the connection is broken or
		// the client is gone; there is nothing left to report to it.
		return
	}
}
