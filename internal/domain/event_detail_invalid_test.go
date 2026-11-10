package domain

import (
	"errors"
	"testing"
	"time"
)

func TestOperationEventRejectsInvalidPayloadsForTheRightKind(t *testing.T) {
	system, err := NewActor("guardian", ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := NewActor("alice", ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := NewActor("bob", ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		kind   EventKind
		actor  Actor
		detail EventDetail
	}{
		{"status unchanged", EventStatusChanged, system, StatusChangedDetail{From: StatusCreated, To: StatusCreated}},
		{"status invalid", EventStatusChanged, system, StatusChangedDetail{From: StatusCreated, To: Status(250)}},
		{"evidence without id", EventEvidenceCollected, system, EvidenceCollectedDetail{Kind: EvidenceDeployment}},
		{"evidence kind out of range", EventEvidenceCollected, system, EvidenceCollectedDetail{EvidenceID: "e", Kind: EvidenceKind(250)}},
		{"approval by system actor", EventApprovalRecorded, system, ApprovalRecordedDetail{ApprovalID: "a", Actor: system}},
		{"approval actor differs from event actor", EventApprovalRecorded, alice, ApprovalRecordedDetail{ApprovalID: "a", Actor: bob}},
		{"rejection without reason", EventRejectionRecorded, alice, RejectionRecordedDetail{ApprovalID: "a", Reason: "  "}},
		{"execution outcome out of range", EventExecutionCompleted, system, ExecutionCompletedDetail{ExecutionID: "x", Outcome: ExecutionOutcome(250)}},
		{"verification without revision", EventVerificationCompleted, system, VerificationCompletedDetail{Passed: true}},
		{"terminal without reason", EventIncidentFailed, system, TerminalDetail{Reason: " "}},
		{"policy without decision", EventPolicyEvaluated, system, PolicyEvaluatedDetail{}},
		{"proposal without hash", EventProposalCreated, system, ProposalCreatedDetail{ActionID: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewOperationEvent("event-1", "incident-1", 1, tt.kind, time.Unix(1000, 0), tt.actor, tt.detail)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("NewOperationEvent() error = %v, want validation error", err)
			}
		})
	}
}
