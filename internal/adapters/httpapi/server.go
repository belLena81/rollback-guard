package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const maxJSONBodyBytes = 1 << 20

type Server struct {
	services   *application.Services
	logger     *slog.Logger
	instrument RouteMiddleware
	metrics    http.Handler
}

// RouteMiddleware wraps the handler registered for one route pattern (for
// example "POST /v1/incidents/{incidentId}/approve"). It receives the path
// part of the pattern so telemetry labels stay bounded.
type RouteMiddleware func(route string, next http.Handler) http.Handler

type Option func(*Server)

// WithRouteMiddleware instruments every registered route. Requests that
// match no route (mux 404/405) are not instrumented.
func WithRouteMiddleware(middleware RouteMiddleware) Option {
	return func(s *Server) { s.instrument = middleware }
}

// WithMetricsHandler serves handler at GET /metrics.
func WithMetricsHandler(handler http.Handler) Option {
	return func(s *Server) { s.metrics = handler }
}

func NewServer(services application.Services, logger *slog.Logger, opts ...Option) (*Server, error) {
	if services.Incidents == nil || services.Investigations == nil || services.Approvals == nil || services.Executions == nil || services.Verifications == nil || services.Queries == nil {
		return nil, fmt.Errorf("all application services are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{services: &services, logger: logger, metrics: http.HandlerFunc(emptyMetrics)}
	for _, opt := range opts {
		opt(server)
	}
	if server.instrument == nil {
		server.instrument = func(_ string, next http.Handler) http.Handler { return next }
	}
	return server, nil
}

func (s *Server) Routes(ready func(context.Context) error, readyTimeout time.Duration, auth httpserver.AuthMiddleware) http.Handler {
	apiMux := http.NewServeMux()
	s.handle(apiMux, "POST /v1/incidents", s.createIncident)
	s.handle(apiMux, "GET /v1/incidents/{incidentId}", s.getIncident)
	s.handle(apiMux, "GET /v1/incidents/{incidentId}/events", s.listEvents)
	s.handle(apiMux, "POST /v1/incidents/{incidentId}/approve", s.approve)
	s.handle(apiMux, "POST /v1/incidents/{incidentId}/reject", s.reject)
	s.handle(apiMux, "POST /v1/incidents/{incidentId}/cancel", s.cancel)
	var apiHandler http.Handler = apiMux
	if auth != nil {
		apiHandler = auth.Wrap(apiHandler)
	}
	rootMux := http.NewServeMux()
	rootMux.Handle("/v1/", apiHandler)
	health := httpserver.NewHealth(ready, readyTimeout, s.logger)
	s.handle(rootMux, http.MethodGet+" "+httpserver.LivePath, health.Live)
	s.handle(rootMux, http.MethodGet+" "+httpserver.ReadyPath, health.Ready)
	// Requests matching no pattern (404/405 from the mux) are not
	// instrumented: a catch-all route would turn 405 responses into 404s.
	rootMux.Handle("GET /metrics", s.instrument("/metrics", s.metrics))
	return requestIDMiddleware(rootMux)
}

// handle registers handler for pattern, instrumented under the pattern's
// path, with the incident ID path value (when present) in the log
// correlation of the request context.
func (s *Server) handle(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	route := pattern
	if _, path, found := strings.Cut(pattern, " "); found {
		route = path
	}
	// Both wrappers run inside the mux, so path values are resolved; the
	// correlation is set before the instrumentation logs the request.
	mux.Handle(pattern, withPathCorrelation(s.instrument(route, handler)))
}

func withPathCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if incidentID := r.PathValue("incidentId"); incidentID != "" {
			r = r.WithContext(ports.WithCorrelation(r.Context(), ports.Correlation{IncidentID: domain.IncidentID(incidentID)}))
		}
		next.ServeHTTP(w, r)
	})
}

// emptyMetrics answers scrapes with an empty exposition when no registry is
// wired (tests and tools that construct the server without telemetry).
func emptyMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

type createIncidentRequest struct {
	Target  targetDTO  `json:"target"`
	Symptom symptomDTO `json:"symptom"`
}

type approvalRequest struct {
	ActionID     string `json:"actionId"`
	ProposalHash string `json:"proposalHash"`
}

type rejectionRequest struct {
	ActionID     string `json:"actionId"`
	ProposalHash string `json:"proposalHash"`
	Reason       string `json:"reason"`
}

type cancelRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) createIncident(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request createIncidentRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	target, err := request.Target.domain()
	if err != nil {
		writeError(w, r, err)
		return
	}
	symptom, err := request.Symptom.domain()
	if err != nil {
		writeError(w, r, err)
		return
	}
	incident, err := s.services.Incidents.CreateIncidentAsync(r.Context(), application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: actor})
	if err != nil {
		writeError(w, r, err)
		return
	}
	ctx := ports.WithCorrelation(r.Context(), ports.Correlation{IncidentID: incident.ID()})
	summary, err := s.services.Queries.GetIncidentSummary(ctx, incident.ID())
	if err != nil {
		writeError(w, r, err)
		return
	}
	logAttrs := []any{"status", incident.Status().String()}
	if summary.Investigation != nil {
		ctx = ports.WithCorrelation(ctx, ports.Correlation{OperationID: summary.Investigation.OperationID, IdempotencyKey: summary.Investigation.IdempotencyKey})
	}
	s.logger.InfoContext(ctx, "incident created", logAttrs...)
	w.Header().Set("Location", "/v1/incidents/"+string(incident.ID()))
	writeJSON(w, http.StatusAccepted, summaryResponse(summary))
}

func (s *Server) getIncident(w http.ResponseWriter, r *http.Request) {
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), domain.IncidentID(r.PathValue("incidentId")))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	if _, err := s.services.Queries.GetIncident(r.Context(), incidentID); err != nil {
		writeError(w, r, err)
		return
	}
	events, err := s.services.Queries.ListEvents(r.Context(), incidentID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	response := make([]eventDTO, 0, len(events))
	for _, event := range events {
		response = append(response, eventResponse(event))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": response})
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request approvalRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	approval, err := s.services.Approvals.Approve(r.Context(), application.ApprovalCommand{
		IncidentID:   incidentID,
		Actor:        actor,
		ActionID:     domain.ActionID(strings.TrimSpace(request.ActionID)),
		ProposalHash: domain.ProposalHash(strings.TrimSpace(request.ProposalHash)),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	idempotencyKey := domain.IdempotencyKey("approval:" + string(approval.ID()))
	executionCtx := ports.WithCorrelation(r.Context(), ports.Correlation{IdempotencyKey: string(idempotencyKey)})
	s.logger.InfoContext(executionCtx, "rollback approved; executing")
	if _, err := s.services.Executions.Execute(executionCtx, incidentID, idempotencyKey); err != nil {
		if s.writeCommittedExecutionFailure(w, r, incidentID) {
			return
		}
		writeError(w, r, err)
		return
	}
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if summary.Incident.Status() == domain.StatusVerifying {
		if _, err := s.services.Verifications.Verify(r.Context(), incidentID); err != nil {
			if s.writeCommittedVerificationFailure(w, r, incidentID) {
				return
			}
			writeError(w, r, err)
			return
		}
		summary, err = s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
		if err != nil {
			writeError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
}

func (s *Server) writeCommittedExecutionFailure(w http.ResponseWriter, r *http.Request, incidentID domain.IncidentID) bool {
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
	if err != nil || summary.Execution == nil {
		return false
	}
	if summary.Incident.Status() != domain.StatusFailed || summary.Execution.Outcome() != domain.ExecutionFailed {
		return false
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
	return true
}

func (s *Server) writeCommittedVerificationFailure(w http.ResponseWriter, r *http.Request, incidentID domain.IncidentID) bool {
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
	if err != nil || summary.Verification == nil {
		return false
	}
	if summary.Incident.Status() != domain.StatusFailed || summary.Verification.Passed() {
		return false
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
	return true
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request rejectionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	if _, err := s.services.Approvals.Reject(r.Context(), application.RejectionCommand{
		IncidentID:   incidentID,
		Actor:        actor,
		ActionID:     domain.ActionID(strings.TrimSpace(request.ActionID)),
		ProposalHash: domain.ProposalHash(strings.TrimSpace(request.ProposalHash)),
		Reason:       request.Reason,
	}); err != nil {
		writeError(w, r, err)
		return
	}
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request cancelRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	if _, err := s.services.Incidents.Cancel(r.Context(), application.CancelIncidentCommand{IncidentID: incidentID, Actor: actor, Reason: request.Reason}); err != nil {
		writeError(w, r, err)
		return
	}
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incidentID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summaryResponse(summary))
}

func requestActor(w http.ResponseWriter, r *http.Request) (domain.Actor, bool) {
	actor, ok := httpserver.ActorFromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorDTO{Error: "unauthenticated", Message: "request actor is missing", RequestID: requestID(r)})
		return domain.Actor{}, false
	}
	return actor, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorDTO{Error: "invalid_json", Message: "request body must be valid JSON"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorDTO{Error: "invalid_json", Message: "request body must contain exactly one JSON value"})
		return false
	}
	return true
}
