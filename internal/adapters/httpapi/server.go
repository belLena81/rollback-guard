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
)

const (
	defaultReadinessTimeout = 2 * time.Second
	maxJSONBodyBytes        = 1 << 20
)

type Server struct {
	services *application.Services
	logger   *slog.Logger
}

func NewServer(services application.Services, logger *slog.Logger) (*Server, error) {
	if services.Incidents == nil || services.Investigations == nil || services.Approvals == nil || services.Executions == nil || services.Verifications == nil || services.Queries == nil {
		return nil, fmt.Errorf("all application services are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{services: &services, logger: logger}, nil
}

func (s *Server) Routes(ready func(context.Context) error, readyTimeout time.Duration, auth httpserver.AuthMiddleware) http.Handler {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("POST /v1/incidents", s.createIncident)
	apiMux.HandleFunc("GET /v1/incidents/{incidentId}", s.getIncident)
	apiMux.HandleFunc("GET /v1/incidents/{incidentId}/events", s.listEvents)
	apiMux.HandleFunc("POST /v1/incidents/{incidentId}/approve", s.approve)
	apiMux.HandleFunc("POST /v1/incidents/{incidentId}/reject", s.reject)
	apiMux.HandleFunc("POST /v1/incidents/{incidentId}/cancel", s.cancel)
	var apiHandler http.Handler = apiMux
	if auth != nil {
		apiHandler = auth.Wrap(apiHandler)
	}
	rootMux := http.NewServeMux()
	rootMux.Handle("/v1/", apiHandler)
	rootMux.HandleFunc("GET /health/live", live)
	rootMux.HandleFunc("GET /health/ready", readiness(ready, readyTimeout))
	rootMux.HandleFunc("GET /metrics", metrics)
	return requestIDMiddleware(rootMux)
}

func live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func readiness(ready func(context.Context) error, readyTimeout time.Duration) http.HandlerFunc {
	if readyTimeout <= 0 {
		readyTimeout = defaultReadinessTimeout
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if _, err := w.Write([]byte("# Placeholder metrics\nrollback_guard_build_info 1\n")); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
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
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incident.ID())
	if err != nil {
		writeError(w, r, err)
		return
	}
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
	if _, err := s.services.Executions.Execute(r.Context(), incidentID, domain.IdempotencyKey("approval:"+string(approval.ID()))); err != nil {
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
