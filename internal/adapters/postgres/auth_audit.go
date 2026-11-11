package postgres

import (
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// maxAuthAuditList bounds ListRecent so a caller cannot read the whole table.
const maxAuthAuditList = 1000

const (
	insertAuthAuditSQL = `
		INSERT INTO auth_audit_events (id, occurred_at, actor_id, action, target_user_id, from_role, to_role, session_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
	listAuthAuditSQL = `
		SELECT id, occurred_at, actor_id, action, target_user_id, from_role, to_role, session_count
		FROM auth_audit_events
		WHERE $1::timestamptz IS NULL OR (occurred_at, id) < ($1::timestamptz, $2::text)
		ORDER BY occurred_at DESC, id DESC
		LIMIT $3`
	// lockActiveAdministratorsSQL locks every enabled administrator row. A
	// concurrent demotion waits on the lock and, under READ COMMITTED,
	// re-evaluates the WHERE clause against the committed row, so it no
	// longer counts an administrator the first transaction demoted.
	lockActiveAdministratorsSQL = `
		SELECT id FROM users WHERE role = 'administrator' AND NOT disabled
		ORDER BY id
		FOR UPDATE`
)

func (repo userRepo) LockActiveAdministrators() ([]domain.UserID, error) {
	rows, err := repo.r.tx.Query(repo.r.ctx, lockActiveAdministratorsSQL)
	if err != nil {
		return nil, fmt.Errorf("lock administrators: %w", err)
	}
	defer rows.Close()
	var ids []domain.UserID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan administrator: %w", err)
		}
		ids = append(ids, domain.UserID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock administrators: %w", err)
	}
	return ids, nil
}

type authAuditRepo struct {
	r *repositories
}

func (repo authAuditRepo) Append(event domain.AuthAuditEvent) error {
	if !event.Valid() {
		return &domain.ValidationError{Field: "auth_audit", Reason: "must be built by a constructor"}
	}
	s := event.Snapshot()
	var sessionCount *int
	if s.HasSessionCount {
		sessionCount = &s.SessionCount
	}
	_, err := repo.r.tx.Exec(repo.r.ctx, insertAuthAuditSQL,
		s.ID, s.OccurredAt, string(s.ActorID), s.Action.String(), nullableText(string(s.TargetUserID)),
		nullableRole(s.FromRole), nullableRole(s.ToRole), sessionCount)
	return mapError(err, "auth_audit_event", string(s.ID))
}

func (repo authAuditRepo) ListRecent(limit int) ([]domain.AuthAuditEvent, error) {
	return repo.ListPage(ports.AuthAuditCursor{}, limit)
}

func (repo authAuditRepo) ListPage(after ports.AuthAuditCursor, limit int) ([]domain.AuthAuditEvent, error) {
	if limit <= 0 || limit > maxAuthAuditList {
		return nil, &domain.ValidationError{Field: "limit", Reason: fmt.Sprintf("must be between 1 and %d", maxAuthAuditList)}
	}
	var afterTime *time.Time
	if !after.IsZero() {
		afterTime = &after.OccurredAt
	}
	rows, err := repo.r.tx.Query(repo.r.ctx, listAuthAuditSQL, afterTime, string(after.ID), limit)
	if err != nil {
		return nil, fmt.Errorf("list auth audit events: %w", err)
	}
	defer rows.Close()
	var events []domain.AuthAuditEvent
	for rows.Next() {
		var (
			id, actorID, action      string
			occurredAt               time.Time
			target, fromRole, toRole *string
			sessionCount             *int
		)
		if err := rows.Scan(&id, &occurredAt, &actorID, &action, &target, &fromRole, &toRole, &sessionCount); err != nil {
			return nil, fmt.Errorf("scan auth audit event: %w", err)
		}
		event, err := rehydrateAuthAudit(id, occurredAt, actorID, action, target, fromRole, toRole, sessionCount)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list auth audit events: %w", err)
	}
	return events, nil
}

func rehydrateAuthAudit(
	id string, occurredAt time.Time, actorID, action string, target, fromRole, toRole *string, sessionCount *int,
) (domain.AuthAuditEvent, error) {
	parsedAction, err := domain.ParseAuthAuditAction(action)
	if err != nil {
		return domain.AuthAuditEvent{}, err
	}
	snapshot := domain.AuthAuditSnapshot{
		ID:         domain.AuthAuditEventID(id),
		OccurredAt: occurredAt,
		ActorID:    domain.ActorID(actorID),
		Action:     parsedAction,
	}
	if target != nil {
		snapshot.TargetUserID = domain.UserID(*target)
	}
	for _, field := range []struct {
		value *string
		into  *domain.Role
	}{{fromRole, &snapshot.FromRole}, {toRole, &snapshot.ToRole}} {
		if field.value == nil {
			continue
		}
		if *field.into, err = domain.ParseRole(*field.value); err != nil {
			return domain.AuthAuditEvent{}, err
		}
	}
	if sessionCount != nil {
		snapshot.SessionCount, snapshot.HasSessionCount = *sessionCount, true
	}
	return domain.RehydrateAuthAuditEvent(snapshot)
}

func nullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullableRole(role domain.Role) *string {
	if role == 0 {
		return nil
	}
	return nullableText(role.String())
}
