package application

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// audit is what every service that changes an incident shares: the ID source
// for audit events and the system actor that performs automated transitions.
// Services embed it to get transition and appendEvents.
type audit struct {
	ids         ports.IDGenerator
	systemActor domain.Actor
}

// auditEvent is one operation event to append for an incident. The event ID,
// sequence, and time are assigned when it is appended.
type auditEvent struct {
	kind   domain.EventKind
	actor  domain.Actor
	detail domain.EventDetail
}

// eventsFor builds the audit events of an incident transition once the
// statuses it moved between are known.
type eventsFor func(from, to domain.Status) []auditEvent

func statusChangeEvent(actor domain.Actor, from, to domain.Status) auditEvent {
	return auditEvent{kind: domain.EventStatusChanged, actor: actor, detail: domain.StatusChangedDetail{From: from, To: to}}
}

func incidentCreated(command CreateIncidentCommand) auditEvent {
	return auditEvent{kind: domain.EventIncidentCreated, actor: command.Actor, detail: domain.IncidentCreatedDetail{Target: command.Target}}
}

// statusChanged audits a transition as a status change by actor.
func statusChanged(actor domain.Actor) eventsFor {
	return func(from, to domain.Status) []auditEvent {
		return []auditEvent{statusChangeEvent(actor, from, to)}
	}
}

// approvalEvents audits a recorded approval decision: the decision itself,
// then the incident's status change, both attributed to the deciding actor.
func approvalEvents(actor domain.Actor, approval domain.Approval, reason string) eventsFor {
	return func(from, to domain.Status) []auditEvent {
		recorded := auditEvent{kind: domain.EventApprovalRecorded, actor: actor, detail: domain.ApprovalRecordedDetail{ApprovalID: approval.ID(), Actor: approval.Actor()}}
		if approval.Decision() == domain.ApprovalRejected {
			recorded = auditEvent{kind: domain.EventRejectionRecorded, actor: actor, detail: domain.RejectionRecordedDetail{ApprovalID: approval.ID(), Reason: reason}}
		}
		return []auditEvent{recorded, statusChangeEvent(actor, from, to)}
	}
}

// terminated audits a transition into a terminal status with its own event
// kind and reason.
func terminated(actor domain.Actor, kind domain.EventKind, reason string) eventsFor {
	return func(_, _ domain.Status) []auditEvent {
		return []auditEvent{{kind: kind, actor: actor, detail: domain.TerminalDetail{Reason: reason}}}
	}
}

// transition applies one state change to the loaded incident, persists it
// with the version it was loaded at, and appends the audit events the change
// produces, all through repositories so the caller's transaction commits or
// rolls back the change and its audit trail together. The statuses the events
// describe are read from the incident before and after apply, never passed in.
//
// The incident write comes first: a concurrent request that already moved the
// incident on fails here with a version conflict, before the caller writes
// anything else that could collide with the winner's rows.
func (a audit) transition(ctx context.Context, repositories ports.Repositories, incident domain.Incident, now time.Time, apply func(*domain.Incident, time.Time) error, events eventsFor) (domain.Incident, error) {
	expected, from := incident.Version(), incident.Status()
	if err := apply(&incident, now); err != nil {
		return domain.Incident{}, err
	}
	if err := repositories.Incidents().Update(incident, expected); err != nil {
		return domain.Incident{}, err
	}
	if err := a.appendEvents(ctx, repositories, incident.ID(), now, events(from, incident.Status())...); err != nil {
		return domain.Incident{}, err
	}
	return incident, nil
}

// appendEvents appends the events to the incident's audit trail in order.
func (a audit) appendEvents(ctx context.Context, repositories ports.Repositories, incidentID domain.IncidentID, now time.Time, events ...auditEvent) error {
	for _, e := range events {
		sequence, err := repositories.Events().NextSequence(ctx, incidentID)
		if err != nil {
			return fmt.Errorf("get next event sequence: %w", err)
		}
		event, err := domain.NewOperationEvent(domain.EventID(a.ids.NewID("event")), incidentID, sequence, e.kind, now, e.actor, e.detail)
		if err != nil {
			return err
		}
		if err := repositories.Events().Append(event); err != nil {
			return err
		}
	}
	return nil
}

// systemEvent is an audit event performed by the system actor.
func (a audit) systemEvent(kind domain.EventKind, detail domain.EventDetail) auditEvent {
	return auditEvent{kind: kind, actor: a.systemActor, detail: detail}
}
