package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// ObserveUnitOfWork decorates next so workflow metrics are derived from what
// a transaction actually commits: audit events (incident lifecycle, approval
// decisions, rollback execution and verification outcomes) and investigation
// dispatch rows reaching a terminal state. Observations from a transaction
// that rolls back are discarded, so counters agree with the audit timeline.
func (p *Provider) ObserveUnitOfWork(next ports.UnitOfWork) (ports.UnitOfWork, error) {
	if next == nil {
		return nil, fmt.Errorf("unit of work is required")
	}
	return &observedUnitOfWork{next: next, metrics: p.metrics}, nil
}

type observedUnitOfWork struct {
	next    ports.UnitOfWork
	metrics *metrics
}

func (u *observedUnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	var pending *transactionObservations
	err := u.next.WithinTransaction(ctx, func(repositories ports.Repositories) error {
		// A unit of work may invoke fn more than once; only the last
		// invocation can commit.
		pending = &transactionObservations{terminalIncidents: map[domain.IncidentID]bool{}, dispatchStates: map[domain.IncidentID]domain.InvestigationTaskState{}}
		return fn(observedRepositories{Repositories: repositories, observations: pending})
	})
	if err == nil && pending != nil {
		pending.flush(u.metrics)
	}
	return err
}

type transactionObservations struct {
	created           int
	terminal          []domain.Status
	terminalIncidents map[domain.IncidentID]bool
	decisions         []string
	executions        []string
	verifications     []string
	dispatches        []domain.InvestigationDispatch
	dispatchStates    map[domain.IncidentID]domain.InvestigationTaskState
}

func (o *transactionObservations) recordEvent(event domain.OperationEvent) {
	switch detail := event.Detail().(type) {
	case domain.IncidentCreatedDetail:
		o.created++
	case domain.StatusChangedDetail:
		if detail.To.Terminal() {
			o.recordTerminal(event.IncidentID(), detail.To)
		}
	case domain.ApprovalRecordedDetail:
		o.decisions = append(o.decisions, "approved")
	case domain.RejectionRecordedDetail:
		o.decisions = append(o.decisions, "rejected")
	case domain.ExecutionCompletedDetail:
		outcome := "failed"
		if detail.Outcome == domain.ExecutionSucceeded {
			outcome = "succeeded"
		}
		o.executions = append(o.executions, outcome)
	case domain.VerificationCompletedDetail:
		result := "failed"
		if detail.Passed {
			result = "passed"
		}
		o.verifications = append(o.verifications, result)
	case domain.TerminalDetail:
		switch event.Kind() {
		case domain.EventIncidentFailed:
			o.recordTerminal(event.IncidentID(), domain.StatusFailed)
		case domain.EventIncidentExpired:
			o.recordTerminal(event.IncidentID(), domain.StatusExpired)
		case domain.EventIncidentCanceled:
			o.recordTerminal(event.IncidentID(), domain.StatusCanceled)
		case domain.EventIncidentCreated, domain.EventStatusChanged, domain.EventEvidenceCollected,
			domain.EventDiagnosisCompleted, domain.EventPolicyEvaluated, domain.EventProposalCreated,
			domain.EventApprovalRecorded, domain.EventRejectionRecorded, domain.EventExecutionCompleted,
			domain.EventVerificationCompleted:
			// The domain only pairs TerminalDetail with the three terminal
			// kinds above; any other kind is not a terminal transition.
		}
	}
}

// recordTerminal counts one terminal transition per incident per transaction
// even when both a status-change and a terminal event are appended.
func (o *transactionObservations) recordTerminal(incidentID domain.IncidentID, status domain.Status) {
	if o.terminalIncidents[incidentID] {
		return
	}
	o.terminalIncidents[incidentID] = true
	o.terminal = append(o.terminal, status)
}

func (o *transactionObservations) flush(m *metrics) {
	if o.created > 0 {
		m.incidentsCreated.Add(float64(o.created))
	}
	for _, status := range o.terminal {
		m.incidentsTerminal.WithLabelValues(status.String()).Inc()
	}
	for _, decision := range o.decisions {
		m.approvalDecisions.WithLabelValues(decision).Inc()
	}
	for _, outcome := range o.executions {
		m.rollbackExecutions.WithLabelValues(outcome).Inc()
	}
	for _, result := range o.verifications {
		m.recoveryVerifications.WithLabelValues(result).Inc()
	}
	for _, dispatch := range o.dispatches {
		outcome := dispatch.State.String()
		reason := "none"
		if dispatch.State == domain.InvestigationTaskFailed {
			reason = dispatchFailureReason(dispatch.FailureCode)
		}
		m.dispatches.WithLabelValues(outcome, reason).Inc()
		end := dispatch.CompletedAt
		if end.IsZero() {
			end = dispatch.UpdatedAt
		}
		if !dispatch.CreatedAt.IsZero() && end.After(dispatch.CreatedAt) {
			m.dispatchDuration.WithLabelValues(outcome).Observe(end.Sub(dispatch.CreatedAt).Seconds())
		}
	}
}

// dispatchFailureReason bounds the failure code label to the codes the
// coordinator writes.
func dispatchFailureReason(code string) string {
	switch code {
	case "submission_failed", "investigation_failed", "investigation_timeout", "invalid_investigation_result":
		return code
	default:
		return "other"
	}
}

type observedRepositories struct {
	ports.Repositories
	observations *transactionObservations
}

func (r observedRepositories) Events() ports.EventRepository {
	return observedEvents{EventRepository: r.Repositories.Events(), observations: r.observations}
}

func (r observedRepositories) InvestigationTasks() ports.InvestigationTaskRepository {
	return observedTasks{InvestigationTaskRepository: r.Repositories.InvestigationTasks(), observations: r.observations}
}

type observedEvents struct {
	ports.EventRepository
	observations *transactionObservations
}

func (e observedEvents) Append(event domain.OperationEvent) error {
	if err := e.EventRepository.Append(event); err != nil {
		return err
	}
	e.observations.recordEvent(event)
	return nil
}

type observedTasks struct {
	ports.InvestigationTaskRepository
	observations *transactionObservations
}

func (t observedTasks) Create(task domain.InvestigationDispatch) error {
	if err := t.InvestigationTaskRepository.Create(task); err != nil {
		return err
	}
	t.observations.dispatchStates[task.IncidentID] = task.State
	return nil
}

func (t observedTasks) GetByIncident(incidentID domain.IncidentID) (domain.InvestigationDispatch, error) {
	task, err := t.InvestigationTaskRepository.GetByIncident(incidentID)
	if err == nil {
		t.remember(task)
	}
	return task, err
}

func (t observedTasks) ClaimNext(now time.Time, owner string, leaseUntil time.Time) (domain.InvestigationDispatch, error) {
	task, err := t.InvestigationTaskRepository.ClaimNext(now, owner, leaseUntil)
	if err == nil {
		t.remember(task)
	}
	return task, err
}

func (t observedTasks) remember(task domain.InvestigationDispatch) {
	if _, seen := t.observations.dispatchStates[task.IncidentID]; !seen {
		t.observations.dispatchStates[task.IncidentID] = task.State
	}
}

// Update counts a dispatch only on its transition into a terminal state, so
// repeated terminal writes (for example a second cancel) are not recounted.
func (t observedTasks) Update(task domain.InvestigationDispatch, expectedVersion uint64) error {
	previous, known := t.observations.dispatchStates[task.IncidentID]
	if !known {
		current, err := t.InvestigationTaskRepository.GetByIncident(task.IncidentID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		previous = current.State
	}
	if err := t.InvestigationTaskRepository.Update(task, expectedVersion); err != nil {
		return err
	}
	t.observations.dispatchStates[task.IncidentID] = task.State
	if task.State.Terminal() && !previous.Terminal() {
		t.observations.dispatches = append(t.observations.dispatches, task)
	}
	return nil
}
