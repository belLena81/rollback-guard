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
)

const maxJSONBodyBytes = 1 << 20

type Server struct {
	services *application.Services
	actor    domain.Actor
	logger   *slog.Logger
}

func NewServer(services application.Services, actor domain.Actor, logger *slog.Logger) (*Server, error) {
	if services.Incidents == nil || services.Investigations == nil || services.Approvals == nil || services.Executions == nil || services.Verifications == nil || services.Queries == nil {
		return nil, fmt.Errorf("all application services are required")
	}
	if !actor.IsHuman() {
		return nil, fmt.Errorf("local actor must be human")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{services: &services, actor: actor, logger: logger}, nil
}

func (s *Server) Routes(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/incidents", s.createIncident)
	mux.HandleFunc("GET /v1/incidents/{incidentId}", s.getIncident)
	mux.HandleFunc("GET /v1/incidents/{incidentId}/events", s.listEvents)
	mux.HandleFunc("POST /v1/incidents/{incidentId}/approve", s.approve)
	mux.HandleFunc("POST /v1/incidents/{incidentId}/reject", s.reject)
	mux.HandleFunc("POST /v1/incidents/{incidentId}/cancel", s.cancel)
	mux.HandleFunc("GET /health/live", live)
	mux.HandleFunc("GET /health/ready", readiness(ready))
	mux.HandleFunc("GET /metrics", metrics)
	return requestIDMiddleware(mux)
}

func live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func readiness(ready func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ready == nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
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
	incident, err := s.services.Incidents.CreateIncident(r.Context(), application.CreateIncidentCommand{Target: target, Symptom: symptom, Actor: s.actor})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := s.services.Investigations.Investigate(r.Context(), incident.ID()); err != nil {
		writeError(w, r, err)
		return
	}
	summary, err := s.services.Queries.GetIncidentSummary(r.Context(), incident.ID())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, summaryResponse(summary))
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
	events, err := s.services.Queries.ListEvents(r.Context(), domain.IncidentID(r.PathValue("incidentId")))
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
	var request approvalRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	approval, err := s.services.Approvals.Approve(r.Context(), application.ApprovalCommand{
		IncidentID:   incidentID,
		Actor:        s.actor,
		ActionID:     domain.ActionID(strings.TrimSpace(request.ActionID)),
		ProposalHash: domain.ProposalHash(strings.TrimSpace(request.ProposalHash)),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := s.services.Executions.Execute(r.Context(), incidentID, domain.IdempotencyKey("approval:"+string(approval.ID()))); err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := s.services.Verifications.Verify(r.Context(), incidentID); err != nil {
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

func (s *Server) reject(w http.ResponseWriter, r *http.Request) {
	var request rejectionRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	if _, err := s.services.Approvals.Reject(r.Context(), application.RejectionCommand{
		IncidentID:   incidentID,
		Actor:        s.actor,
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
	var request cancelRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	incidentID := domain.IncidentID(r.PathValue("incidentId"))
	if _, err := s.services.Incidents.Cancel(r.Context(), application.CancelIncidentCommand{IncidentID: incidentID, Actor: s.actor, Reason: request.Reason}); err != nil {
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
