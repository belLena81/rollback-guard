package domain

import "time"

type Status uint8

const (
	StatusCreated Status = iota + 1
	StatusInvestigating
	StatusAwaitingApproval
	StatusApproved
	StatusExecuting
	StatusVerifying
	StatusResolved
	StatusCanceled
	StatusDeclined
	StatusRejected
	StatusExpired
	StatusFailed
)

func (s Status) String() string {
	names := [...]string{"unknown", "created", "investigating", "awaiting_approval", "approved", "executing", "verifying", "resolved", "canceled", "declined", "rejected", "expired", "failed"}
	if int(s) >= len(names) {
		return names[0]
	}
	return names[s]
}

func (s Status) Terminal() bool {
	switch s {
	case StatusResolved, StatusCanceled, StatusDeclined, StatusRejected, StatusExpired, StatusFailed:
		return true
	case StatusCreated, StatusInvestigating, StatusAwaitingApproval, StatusApproved, StatusExecuting, StatusVerifying:
		return false
	default:
		return false
	}
}

func (s Status) Valid() bool { return s >= StatusCreated && s <= StatusFailed }

type Incident struct {
	id                   IncidentID
	target               DeploymentTarget
	symptom              Symptom
	status               Status
	version              uint64
	createdAt, updatedAt time.Time
}

func NewIncident(id IncidentID, target DeploymentTarget, symptom Symptom, now time.Time) (Incident, error) {
	if err := requireText("incident.id", string(id)); err != nil {
		return Incident{}, err
	}
	if !target.Valid() {
		return Incident{}, invalid("incident.target", "must be valid")
	}
	if !symptom.Valid() {
		return Incident{}, invalid("incident.symptom", "must be valid")
	}
	if now.IsZero() {
		return Incident{}, invalid("incident.created_at", "must not be zero")
	}
	return Incident{id: id, target: target, symptom: symptom, status: StatusCreated, version: 1, createdAt: now.UTC(), updatedAt: now.UTC()}, nil
}
func (i Incident) ID() IncidentID {
	return i.id
}
func (i Incident) Target() DeploymentTarget {
	return i.target
}
func (i Incident) Symptom() Symptom {
	return i.symptom
}
func (i Incident) Status() Status {
	return i.status
}
func (i Incident) Version() uint64 {
	return i.version
}
func (i Incident) CreatedAt() time.Time {
	return i.createdAt
}
func (i Incident) UpdatedAt() time.Time {
	return i.updatedAt
}

func (i *Incident) transition(to Status, now time.Time, allowed ...Status) error {
	for _, from := range allowed {
		if i.status == from {
			i.status, i.updatedAt = to, now.UTC()
			i.version++
			return nil
		}
	}
	return &IllegalTransitionError{From: i.status, To: to}
}
func (i *Incident) StartInvestigation(now time.Time) error {
	return i.transition(StatusInvestigating, now, StatusCreated)
}
func (i *Incident) AwaitApproval(now time.Time) error {
	return i.transition(StatusAwaitingApproval, now, StatusInvestigating)
}
func (i *Incident) Approve(now time.Time) error {
	return i.transition(StatusApproved, now, StatusAwaitingApproval)
}
func (i *Incident) Reject(now time.Time) error {
	return i.transition(StatusRejected, now, StatusAwaitingApproval)
}
func (i *Incident) StartExecution(now time.Time) error {
	return i.transition(StatusExecuting, now, StatusApproved)
}
func (i *Incident) StartVerification(now time.Time) error {
	return i.transition(StatusVerifying, now, StatusExecuting)
}
func (i *Incident) Resolve(now time.Time) error {
	return i.transition(StatusResolved, now, StatusVerifying)
}
func (i *Incident) Decline(now time.Time) error {
	return i.transition(StatusDeclined, now, StatusInvestigating)
}
func (i *Incident) Expire(now time.Time) error {
	return i.transition(StatusExpired, now, StatusAwaitingApproval, StatusApproved)
}
func (i *Incident) Cancel(now time.Time) error {
	return i.transition(StatusCanceled, now, StatusCreated, StatusInvestigating, StatusAwaitingApproval, StatusApproved)
}
func (i *Incident) Fail(now time.Time) error {
	return i.transition(StatusFailed, now, StatusInvestigating, StatusApproved, StatusExecuting, StatusVerifying)
}
