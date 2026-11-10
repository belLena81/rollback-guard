package domain

import (
	"strings"
	"time"
)

// AuthAuditAction names a security-relevant change to users or sessions.
type AuthAuditAction uint8

const (
	AuthAuditRoleChanged AuthAuditAction = iota + 1
	AuthAuditSessionsRevoked
	AuthAuditLogout
)

func (a AuthAuditAction) String() string {
	switch a {
	case AuthAuditRoleChanged:
		return "role_changed"
	case AuthAuditSessionsRevoked:
		return "sessions_revoked"
	case AuthAuditLogout:
		return "logout"
	default:
		return "invalid"
	}
}

func (a AuthAuditAction) Valid() bool {
	return a >= AuthAuditRoleChanged && a <= AuthAuditLogout
}

func ParseAuthAuditAction(value string) (AuthAuditAction, error) {
	for action := AuthAuditRoleChanged; action <= AuthAuditLogout; action++ {
		if action.String() == value {
			return action, nil
		}
	}
	return 0, invalid("auth_audit.action", "unknown action")
}

type AuthAuditEventID string

// AuthAuditEvent is one append-only record of an auth change. It holds only
// identifiers, roles and counts: never tokens, hashes, emails or free text.
// The actor is always the human who made the change.
type AuthAuditEvent struct {
	id           AuthAuditEventID
	occurredAt   time.Time
	actor        Actor
	action       AuthAuditAction
	targetUserID UserID
	fromRole     Role
	toRole       Role
	sessionCount int
	hasCount     bool
}

// AuthAuditSnapshot is the persisted form. Zero TargetUserID, FromRole and
// ToRole mean absent; HasSessionCount says whether SessionCount is set.
type AuthAuditSnapshot struct {
	ID              AuthAuditEventID
	OccurredAt      time.Time
	ActorID         ActorID
	Action          AuthAuditAction
	TargetUserID    UserID
	FromRole        Role
	ToRole          Role
	SessionCount    int
	HasSessionCount bool
}

// NewRoleChangedAudit records actor changing target's role from one role to
// a different one.
func NewRoleChangedAudit(id AuthAuditEventID, actor Actor, target UserID, from, to Role, now time.Time) (AuthAuditEvent, error) {
	if !from.Valid() || !to.Valid() || from == to {
		return AuthAuditEvent{}, invalid("auth_audit.role", "from and to must be different valid roles")
	}
	return newAuthAudit(AuthAuditEvent{id: id, occurredAt: now, actor: actor, action: AuthAuditRoleChanged, targetUserID: target, fromRole: from, toRole: to})
}

// NewSessionsRevokedAudit records actor revoking count sessions of target.
func NewSessionsRevokedAudit(id AuthAuditEventID, actor Actor, target UserID, count int, now time.Time) (AuthAuditEvent, error) {
	return newAuthAudit(AuthAuditEvent{
		id: id, occurredAt: now, actor: actor, action: AuthAuditSessionsRevoked,
		targetUserID: target, sessionCount: count, hasCount: true,
	})
}

// NewLogoutAudit records actor (user) ending their own session.
func NewLogoutAudit(id AuthAuditEventID, actor Actor, user UserID, now time.Time) (AuthAuditEvent, error) {
	return newAuthAudit(AuthAuditEvent{id: id, occurredAt: now, actor: actor, action: AuthAuditLogout, targetUserID: user})
}

func RehydrateAuthAuditEvent(s AuthAuditSnapshot) (AuthAuditEvent, error) {
	actor, err := NewActor(s.ActorID, ActorHuman)
	if err != nil {
		return AuthAuditEvent{}, invalid("auth_audit.actor", "must be a valid human actor")
	}
	event := AuthAuditEvent{
		id: s.ID, occurredAt: s.OccurredAt, actor: actor, action: s.Action, targetUserID: s.TargetUserID,
		fromRole: s.FromRole, toRole: s.ToRole, sessionCount: s.SessionCount, hasCount: s.HasSessionCount,
	}
	return newAuthAudit(event)
}

// newAuthAudit checks the fields every action shares and the exact shape of
// each action, so no row carries fields its action does not define.
func newAuthAudit(e AuthAuditEvent) (AuthAuditEvent, error) {
	switch {
	case strings.TrimSpace(string(e.id)) == "":
		return AuthAuditEvent{}, invalid("auth_audit.id", "required value is empty")
	case e.occurredAt.IsZero():
		return AuthAuditEvent{}, invalid("auth_audit.occurred_at", "must be set")
	case !e.actor.IsHuman():
		return AuthAuditEvent{}, invalid("auth_audit.actor", "must be a human actor")
	case strings.TrimSpace(string(e.targetUserID)) == "":
		return AuthAuditEvent{}, invalid("auth_audit.target_user_id", "required value is empty")
	}
	roles := e.fromRole != 0 || e.toRole != 0
	switch e.action {
	case AuthAuditRoleChanged:
		if !e.fromRole.Valid() || !e.toRole.Valid() || e.fromRole == e.toRole || e.hasCount {
			return AuthAuditEvent{}, invalid("auth_audit", "role_changed needs two different roles and no session count")
		}
	case AuthAuditSessionsRevoked:
		if roles || !e.hasCount || e.sessionCount < 0 {
			return AuthAuditEvent{}, invalid("auth_audit", "sessions_revoked needs a non-negative session count and no roles")
		}
	case AuthAuditLogout:
		if roles || e.hasCount {
			return AuthAuditEvent{}, invalid("auth_audit", "logout carries no roles or session count")
		}
	default:
		return AuthAuditEvent{}, invalid("auth_audit.action", "unknown action")
	}
	e.occurredAt = e.occurredAt.UTC()
	return e, nil
}

func (e AuthAuditEvent) ID() AuthAuditEventID      { return e.id }
func (e AuthAuditEvent) OccurredAt() time.Time     { return e.occurredAt }
func (e AuthAuditEvent) Actor() Actor              { return e.actor }
func (e AuthAuditEvent) Action() AuthAuditAction   { return e.action }
func (e AuthAuditEvent) TargetUserID() UserID      { return e.targetUserID }
func (e AuthAuditEvent) FromRole() Role            { return e.fromRole }
func (e AuthAuditEvent) ToRole() Role              { return e.toRole }
func (e AuthAuditEvent) SessionCount() (int, bool) { return e.sessionCount, e.hasCount }

// Valid reports whether e was built by a constructor.
func (e AuthAuditEvent) Valid() bool { return e.action.Valid() }

func (e AuthAuditEvent) Snapshot() AuthAuditSnapshot {
	return AuthAuditSnapshot{
		ID: e.id, OccurredAt: e.occurredAt, ActorID: e.actor.ID(), Action: e.action, TargetUserID: e.targetUserID,
		FromRole: e.fromRole, ToRole: e.toRole, SessionCount: e.sessionCount, HasSessionCount: e.hasCount,
	}
}
