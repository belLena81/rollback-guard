package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

func TestAuthorizationFailureReasonIsFixedBeforePersistence(t *testing.T) {
	t.Parallel()

	const sensitiveDetail = "postgres://guardian:hunter2@db/internal"
	tests := []struct {
		name       string
		authorize  error
		wantStatus domain.Status
		wantKind   domain.EventKind
		wantReason string
	}{
		{
			name:       "expired approval",
			authorize:  fmt.Errorf("%s: %w", sensitiveDetail, &domain.ExpiryError{Resource: "approval"}),
			wantStatus: domain.StatusExpired,
			wantKind:   domain.EventIncidentExpired,
			wantReason: authorizationExpiredAuditReason,
		},
		{
			name:       "approval mismatch",
			authorize:  &domain.ApprovalMismatchError{Reason: sensitiveDetail},
			wantStatus: domain.StatusFailed,
			wantKind:   domain.EventIncidentFailed,
			wantReason: authorizationFailureAuditReason,
		},
		{
			name:       "unclassified failure",
			authorize:  errors.New(sensitiveDetail),
			wantStatus: domain.StatusFailed,
			wantKind:   domain.EventIncidentFailed,
			wantReason: authorizationFailureAuditReason,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 3, 14, 0, 0, 0, time.UTC)
			uow := memory.NewUnitOfWork()
			incidentID, proposal := seedApprovedRollbackState(t, uow, now)
			systemActor := mustActor(t, "guardian", domain.ActorSystem)
			svc := &ExecutionService{
				workflow: &workflow{
					uow:            uow,
					inspector:      &fake.Infrastructure{Deployment: domain.DeploymentEvidence{Target: proposal.Target(), CurrentRevision: proposal.CurrentRevision()}},
					executor:       &failingRollbackExecutor{err: errors.New("executor must not be called")},
					approvalPolicy: rejectingApprovalPolicy{err: test.authorize},
					clock:          testkit.NewClock(now),
					ids:            &testkit.IDGenerator{},
					config:         WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Minute, SystemActor: systemActor},
					systemActor:    systemActor,
				},
			}

			_, err := svc.Execute(context.Background(), incidentID, "execute-audit-reason")
			if !errors.Is(err, test.authorize) {
				t.Fatalf("Execute() error = %v, want the authorization error returned to the caller", err)
			}

			incident, err := loadIncident(t, uow, incidentID)
			if err != nil {
				t.Fatal(err)
			}
			if incident.Status() != test.wantStatus {
				t.Fatalf("incident status = %s, want %s", incident.Status(), test.wantStatus)
			}
			var events []domain.OperationEvent
			err = uow.WithinTransaction(context.Background(), func(repositories ports.Repositories) error {
				var listErr error
				events, listErr = repositories.Events().List(incidentID)
				return listErr
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) == 0 {
				t.Fatal("no audit events recorded")
			}
			last := events[len(events)-1]
			if last.Kind() != test.wantKind {
				t.Fatalf("last event kind = %v, want %v", last.Kind(), test.wantKind)
			}
			detail, ok := last.Detail().(domain.TerminalDetail)
			if !ok {
				t.Fatalf("last event detail type = %T, want domain.TerminalDetail", last.Detail())
			}
			if detail.Reason != test.wantReason {
				t.Fatalf("audit reason = %q, want %q", detail.Reason, test.wantReason)
			}
			if strings.Contains(detail.Reason, sensitiveDetail) {
				t.Fatalf("audit reason leaks the raw error text: %q", detail.Reason)
			}
		})
	}
}

type rejectingApprovalPolicy struct {
	err error
}

func (p rejectingApprovalPolicy) Authorize(domain.RollbackProposal, domain.PolicyDecision, domain.Approval, domain.DeploymentEvidence, time.Time) (domain.ApprovedRollback, error) {
	return domain.ApprovedRollback{}, p.err
}
