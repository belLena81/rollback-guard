package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// A second insert of the same record is a conflict with what is already
// stored, not a malformed request: callers and the HTTP layer treat the two
// differently (409 against 400).
func TestDuplicateInsertIsAConflictNotAValidationError(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	incident := repositoryIncident(t)
	decision, err := domain.NewPolicyDecision("policy-1", incident.ID(), []domain.RuleResult{{Name: "allowed", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewRollbackProposal("action-1", incident.ID(), decision.ID(), incident.Target(), "revision-1", "revision-2", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	human, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval-1", human, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := domain.InvestigationDispatch{IncidentID: incident.ID(), State: domain.InvestigationTaskQueued, Stage: domain.InvestigationStageQueued, Version: 1}

	createIncident := func(r ports.Repositories) error { return r.Incidents().Create(incident) }
	saveDecision := func(r ports.Repositories) error { return r.PolicyDecisions().Save(decision) }
	saveProposal := func(r ports.Repositories) error { return r.Proposals().Save(proposal) }
	saveApproval := func(r ports.Repositories) error { return r.Approvals().Save(approval) }
	createDispatch := func(r ports.Repositories) error { return r.InvestigationTasks().Create(dispatch) }

	tests := []struct {
		name   string
		before []func(ports.Repositories) error // the records the insert refers to
		insert func(ports.Repositories) error
	}{
		{"incident", nil, createIncident},
		{"policy decision", []func(ports.Repositories) error{createIncident}, saveDecision},
		{"proposal", []func(ports.Repositories) error{createIncident, saveDecision}, saveProposal},
		{"approval", []func(ports.Repositories) error{createIncident, saveDecision, saveProposal}, saveApproval},
		{"investigation dispatch", []func(ports.Repositories) error{createIncident}, createDispatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uow := memory.NewUnitOfWork()
			for _, seed := range tt.before {
				if err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error { return seed(r) }); err != nil {
					t.Fatalf("seed error = %v", err)
				}
			}
			if err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error { return tt.insert(r) }); err != nil {
				t.Fatalf("first insert error = %v", err)
			}
			err := uow.WithinTransaction(context.Background(), func(r ports.Repositories) error { return tt.insert(r) })
			if !errors.Is(err, domain.ErrAlreadyExists) {
				t.Fatalf("second insert error = %v, want already exists", err)
			}
			if errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrVersionConflict) {
				t.Fatalf("second insert error = %v, must be neither a validation error nor a version conflict", err)
			}
		})
	}
}
