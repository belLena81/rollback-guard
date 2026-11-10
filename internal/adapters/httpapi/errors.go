package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/belLena81/rollback-guard/internal/domain"
)

type errorDTO struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

type requestIDKey struct{}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = "local"
		}
		w.Header().Set("X-Request-Id", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID)))
	})
}

func requestID(r *http.Request) string {
	value := r.Context().Value(requestIDKey{})
	if requestID, ok := value.(string); ok {
		return requestID
	}
	return ""
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	message := "internal server error"
	switch {
	case errors.Is(err, domain.ErrValidation):
		status = http.StatusBadRequest
		code = "validation_failed"
		message = err.Error()
	case errors.Is(err, domain.ErrNotFound):
		status = http.StatusNotFound
		code = "not_found"
		message = err.Error()
	case errors.Is(err, domain.ErrNonHumanApproval):
		status = http.StatusForbidden
		code = "forbidden"
		message = err.Error()
	case errors.Is(err, domain.ErrIllegalTransition),
		errors.Is(err, domain.ErrVersionConflict),
		errors.Is(err, domain.ErrIdempotencyConflict),
		errors.Is(err, domain.ErrApprovalMismatch),
		errors.Is(err, domain.ErrExpired),
		errors.Is(err, domain.ErrPolicyDenied):
		status = http.StatusConflict
		code = "conflict"
		message = err.Error()
	}
	writeJSON(w, status, errorDTO{Error: code, Message: message, RequestID: requestID(r)})
}

// encodeFailureBody is the internal_error body sent when a response value
// cannot be encoded. It is fixed so that writing it cannot fail to encode.
const encodeFailureBody = `{"error":"internal_error","message":"internal server error"}` + "\n"

// writeJSON encodes before writing the status line, so an encode failure is
// reported as a 500 instead of a success status with a broken body.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(value); err != nil {
		status = http.StatusInternalServerError
		body.Reset()
		body.WriteString(encodeFailureBody)
	}
	w.WriteHeader(status)
	if _, err := w.Write(body.Bytes()); err != nil {
		// The status line is already sent and the connection is broken or
		// the client is gone; there is nothing left to report to it.
		return
	}
}
