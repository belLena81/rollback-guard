package application

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// VerificationService decides whether a rolled-back deployment recovered.
type VerificationService struct {
	audit
	uow                  ports.UnitOfWork
	clock                ports.Clock
	inspector            ports.DeploymentInspector
	metrics              ports.MetricsPort
	recoveryTimeout      time.Duration
	recoveryPollInterval time.Duration
}

// awaitRecovery evaluates recovery until it passes or the wait is used up,
// and returns the passing or the last failing evaluation. The wait ends after
// 1 + RecoveryTimeout/RecoveryPollInterval checks or once RecoveryTimeout has
// elapsed on the workflow clock, whichever comes first, so slow checks cannot
// stretch it past RecoveryTimeout.
//
// A check whose deployment or metric read fails is neither a pass nor a
// failure: right after the pod switch the metric can be briefly unreadable,
// so the wait continues. If the last allowed check also fails to read, or the
// context ends, that error is returned. Nothing is persisted here, so on an
// error the incident stays verifying and a retry of the approval resumes it.
func (s *VerificationService) awaitRecovery(
	ctx context.Context,
	incident domain.Incident,
	proposal domain.RollbackProposal,
	execution domain.RollbackExecution,
) (domain.RecoveryVerification, error) {
	checks := 1
	if s.recoveryTimeout > 0 {
		checks += int(s.recoveryTimeout / s.recoveryPollInterval)
	}
	deadline := s.clock.Now().Add(s.recoveryTimeout)
	for check := 1; ; check++ {
		verification, err := s.checkRecovery(ctx, incident, proposal, execution)
		last := check >= checks || !s.clock.Now().Before(deadline)
		if err == nil && (verification.Passed() || last) {
			return verification, nil
		}
		if err != nil && (last || ctx.Err() != nil) {
			return domain.RecoveryVerification{}, err
		}
		if err := waitFor(ctx, s.recoveryPollInterval); err != nil {
			return domain.RecoveryVerification{}, fmt.Errorf("await recovery: %w", err)
		}
	}
}

// checkRecovery reads the deployment and the recovery metric once and
// evaluates them against the proposal's target revision.
func (s *VerificationService) checkRecovery(
	ctx context.Context,
	incident domain.Incident,
	proposal domain.RollbackProposal,
	execution domain.RollbackExecution,
) (domain.RecoveryVerification, error) {
	deployment, err := s.inspector.InspectDeployment(ctx, incident.Target())
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("inspect recovery deployment: %w", err)
	}
	metric, err := s.metrics.Observe(ctx, incident.Target(), incident.Symptom())
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("observe recovery metric: %w", err)
	}
	return domain.EvaluateRecovery(
		incident.Target(),
		proposal.TargetRevision(),
		deployment,
		metric,
		incident.Symptom(),
		incident.ID(),
		execution.ID(),
		s.clock.Now(),
	)
}

// waitFor blocks for d or until ctx ends, whichever comes first.
func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *VerificationService) Verify(ctx context.Context, incidentID domain.IncidentID) (domain.RecoveryVerification, error) {
	var incident domain.Incident
	var proposal domain.RollbackProposal
	var execution domain.RollbackExecution
	err := s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		var err error
		incident, err = repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		proposal, err = repositories.Proposals().GetByIncident(incidentID)
		if err != nil {
			return err
		}
		execution, err = repositories.Executions().GetByIncident(incidentID)
		return err
	})
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("load recovery verification: %w", err)
	}
	if incident.Status() != domain.StatusVerifying {
		return domain.RecoveryVerification{}, &domain.IllegalTransitionError{From: incident.Status(), To: domain.StatusResolved}
	}

	verification, err := s.awaitRecovery(ctx, incident, proposal, execution)
	if err != nil {
		return domain.RecoveryVerification{}, err
	}
	now := verification.CheckedAt()
	err = s.uow.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		fresh, err := repositories.Incidents().Get(incidentID)
		if err != nil {
			return err
		}
		if fresh.Version() != incident.Version() {
			return &domain.VersionConflictError{Expected: incident.Version(), Actual: fresh.Version()}
		}
		if err := repositories.Verifications().Save(verification); err != nil {
			return err
		}
		if err := s.appendEvents(
			repositories,
			incidentID,
			now,
			s.systemEvent(
				domain.EventVerificationCompleted,
				domain.VerificationCompletedDetail{Passed: verification.Passed(), Revision: verification.Revision()},
			),
		); err != nil {
			return err
		}
		if verification.Passed() {
			_, err = s.transition(repositories, fresh, now, (*domain.Incident).Resolve, statusChanged(s.systemActor))
		} else {
			_, err = s.transition(
				repositories,
				fresh,
				now,
				(*domain.Incident).Fail,
				terminated(s.systemActor, domain.EventIncidentFailed, verification.FailureReason()),
			)
		}
		return err
	})
	if err != nil {
		return domain.RecoveryVerification{}, fmt.Errorf("persist recovery verification: %w", err)
	}
	if !verification.Passed() {
		return verification, fmt.Errorf("recovery verification failed: %s", verification.FailureReason())
	}
	return verification, nil
}
