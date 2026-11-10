package memory

import (
	"slices"
	"sort"

	"github.com/belLena81/rollback-guard/internal/domain"
)

const maxAuthAuditList = 1000

// LockActiveAdministrators needs no lock here: the unit of work already
// serializes whole transactions.
func (r userRepo) LockActiveAdministrators() ([]domain.UserID, error) {
	var ids []domain.UserID
	for id, user := range r.data.users {
		if user.Role() == domain.RoleAdministrator && !user.Disabled() {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

type authAuditRepo struct{ data *authData }

func (r authAuditRepo) Append(event domain.AuthAuditEvent) error {
	if !event.Valid() {
		return &domain.ValidationError{Field: "auth_audit", Reason: "must be built by a constructor"}
	}
	for _, existing := range r.data.audit {
		if existing.ID() == event.ID() {
			return &domain.AlreadyExistsError{Resource: "auth_audit_event"}
		}
	}
	r.data.audit = append(r.data.audit, event)
	return nil
}

func (r authAuditRepo) ListRecent(limit int) ([]domain.AuthAuditEvent, error) {
	if limit <= 0 || limit > maxAuthAuditList {
		return nil, &domain.ValidationError{Field: "limit", Reason: "must be between 1 and 1000"}
	}
	events := slices.Clone(r.data.audit)
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].OccurredAt().Equal(events[j].OccurredAt()) {
			return events[i].OccurredAt().After(events[j].OccurredAt())
		}
		return events[i].ID() > events[j].ID()
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}
