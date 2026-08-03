package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestRehydrateIncidentPreservesPersistedState(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	symptom, err := domain.NewSymptom("http_error_rate", .35, .1, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := domain.RehydrateIncident(domain.IncidentSnapshot{
		ID:        "incident-1",
		Target:    target,
		Symptom:   symptom,
		Status:    domain.StatusAwaitingApproval,
		Version:   3,
		CreatedAt: now,
		UpdatedAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if incident.ID() != "incident-1" || incident.Status() != domain.StatusAwaitingApproval || incident.Version() != 3 {
		t.Fatalf("rehydrated incident = %q %s v%d", incident.ID(), incident.Status(), incident.Version())
	}
}

func TestRehydrateProposalRejectsHashDrift(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	_, err := domain.RehydrateRollbackProposal(domain.RollbackProposalSnapshot{
		ActionID:         "action-1",
		IncidentID:       "incident-1",
		PolicyDecisionID: "policy-1",
		Target:           target,
		CurrentRevision:  "revision-2",
		TargetRevision:   "revision-1",
		ExpiresAt:        now.Add(time.Minute),
		Hash:             "not-canonical",
	})
	if !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("error = %v, want approval mismatch", err)
	}
}

func TestEvidenceSnapshotIsImmutable(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	target := mustTarget(t, "cluster", "namespace", "deployment", "service")
	evidence, err := domain.NewDeploymentEvidence("evidence-1", "incident-1", domain.DeploymentEvidence{
		Target:                 target,
		CurrentRevision:        "revision-2",
		PreviousStableRevision: "revision-1",
		DesiredReplicas:        3,
		ReadyReplicas:          1,
		AvailableReplicas:      1,
		Healthy:                false,
		RolloutAt:              now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evidence.Snapshot()
	snapshot.Deployment.CurrentRevision = "mutated"
	value, ok := evidence.Deployment()
	if !ok || value.CurrentRevision != "revision-2" {
		t.Fatalf("snapshot mutation leaked into evidence: %q", value.CurrentRevision)
	}
}
