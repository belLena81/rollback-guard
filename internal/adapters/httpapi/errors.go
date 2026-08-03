package httpapi

import (
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}
