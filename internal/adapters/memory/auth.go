package memory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type authData struct {
	users    map[domain.UserID]domain.User
	sessions map[domain.SessionID]domain.Session
	audit    []domain.AuthAuditEvent
}

func (d *authData) clone() *authData {
	return &authData{users: maps.Clone(d.users), sessions: maps.Clone(d.sessions), audit: slices.Clone(d.audit)}
}

// AuthUnitOfWork is the in-process ports.AuthUnitOfWork. It mirrors the
// Postgres adapter: uniqueness of GitHub ID and case-insensitive login,
// unique token hashes, and all-or-nothing commits.
type AuthUnitOfWork struct {
	mu   sync.Mutex
	data *authData
}

func NewAuthUnitOfWork() *AuthUnitOfWork {
	return &AuthUnitOfWork{data: &authData{
		users:    map[domain.UserID]domain.User{},
		sessions: map[domain.SessionID]domain.Session{},
	}}
}

func (u *AuthUnitOfWork) WithinAuthTransaction(ctx context.Context, fn func(ports.AuthRepositories) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context ended: %w", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	tx := authRepositories{data: u.data.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("context ended: %w", err)
	}
	u.data = tx.data
	return nil
}

type authRepositories struct{ data *authData }

func (r authRepositories) Users() ports.UserRepository       { return userRepo(r) }
func (r authRepositories) Sessions() ports.SessionRepository { return sessionRepo(r) }
func (r authRepositories) Audit() ports.AuthAuditRepository  { return authAuditRepo(r) }

type userRepo struct{ data *authData }

func (r userRepo) Create(user domain.User) error {
	if _, ok := r.data.users[user.ID()]; ok {
		return &domain.AlreadyExistsError{Resource: "user"}
	}
	if r.conflicts(user) {
		return &domain.AlreadyExistsError{Resource: "user"}
	}
	r.data.users[user.ID()] = user
	return nil
}

// conflicts reports whether another user holds user's GitHub ID or login.
func (r userRepo) conflicts(user domain.User) bool {
	for id, other := range r.data.users {
		if id == user.ID() {
			continue
		}
		if other.GitHubID() == user.GitHubID() || strings.EqualFold(other.Login(), user.Login()) {
			return true
		}
	}
	return false
}

func (r userRepo) GetByID(id domain.UserID) (domain.User, error) {
	user, ok := r.data.users[id]
	if !ok {
		return domain.User{}, &domain.NotFoundError{Resource: "user", ID: string(id)}
	}
	return user, nil
}

func (r userRepo) GetByGitHubID(githubID int64) (domain.User, error) {
	for _, user := range r.data.users {
		if user.GitHubID() == githubID {
			return user, nil
		}
	}
	return domain.User{}, &domain.NotFoundError{Resource: "user", ID: "github:" + strconv.FormatInt(githubID, 10)}
}

func (r userRepo) Update(user domain.User, expected uint64) error {
	current, ok := r.data.users[user.ID()]
	if !ok {
		return &domain.NotFoundError{Resource: "user", ID: string(user.ID())}
	}
	if user.Version() != expected+1 {
		return &domain.ValidationError{Field: "user.version", Reason: "must be one past the expected version"}
	}
	if current.Version() != expected {
		return &domain.VersionConflictError{Expected: expected, Actual: current.Version()}
	}
	if current.GitHubID() != user.GitHubID() || !current.CreatedAt().Equal(user.CreatedAt()) {
		return &domain.ValidationError{Field: "user", Reason: "github_id and created_at are immutable"}
	}
	if r.conflicts(user) {
		return &domain.AlreadyExistsError{Resource: "user"}
	}
	r.data.users[user.ID()] = user
	return nil
}

type sessionRepo struct{ data *authData }

func (r sessionRepo) Create(session domain.Session) error {
	if _, ok := r.data.sessions[session.ID()]; ok {
		return &domain.AlreadyExistsError{Resource: "session"}
	}
	if _, ok := r.byHash(session.TokenHash()); ok {
		return &domain.AlreadyExistsError{Resource: "session"}
	}
	user, ok := r.data.users[session.UserID()]
	if !ok {
		return &domain.NotFoundError{Resource: "user", ID: string(session.UserID())}
	}
	if user.Disabled() {
		return &domain.UserDisabledError{UserID: user.ID()}
	}
	r.data.sessions[session.ID()] = session
	return nil
}

func (r sessionRepo) byHash(hash domain.SessionTokenHash) (domain.Session, bool) {
	for _, session := range r.data.sessions {
		if session.TokenHash().Equal(hash) {
			return session, true
		}
	}
	return domain.Session{}, false
}

func (r sessionRepo) ResolveActive(tokenHash domain.SessionTokenHash, now time.Time) (domain.Session, domain.User, error) {
	if tokenHash.IsZero() {
		return domain.Session{}, domain.User{}, &domain.NoActiveSessionError{}
	}
	if now.IsZero() {
		return domain.Session{}, domain.User{}, &domain.ValidationError{Field: "now", Reason: "must be set"}
	}
	session, ok := r.byHash(tokenHash)
	if !ok || !session.IsActive(now) {
		return domain.Session{}, domain.User{}, &domain.NoActiveSessionError{}
	}
	user, ok := r.data.users[session.UserID()]
	if !ok || user.Disabled() {
		return domain.Session{}, domain.User{}, &domain.NoActiveSessionError{}
	}
	return session, user, nil
}

// revoke revokes session at now, never before its creation.
func revoke(session domain.Session, now time.Time) (domain.Session, error) {
	if now.Before(session.CreatedAt()) {
		now = session.CreatedAt()
	}
	return session.Revoke(now)
}

func (r sessionRepo) Revoke(id domain.SessionID, now time.Time) error {
	if now.IsZero() {
		return &domain.ValidationError{Field: "session.revoked_at", Reason: "must be set"}
	}
	session, ok := r.data.sessions[id]
	if !ok {
		return &domain.NotFoundError{Resource: "session", ID: string(id)}
	}
	revoked, err := revoke(session, now)
	if err != nil {
		return err
	}
	r.data.sessions[id] = revoked
	return nil
}

func (r sessionRepo) RevokeAllForUser(userID domain.UserID, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, &domain.ValidationError{Field: "session.revoked_at", Reason: "must be set"}
	}
	count := 0
	for id, session := range r.data.sessions {
		if session.UserID() != userID || session.Revoked() {
			continue
		}
		revoked, err := revoke(session, now)
		if err != nil {
			return 0, err
		}
		r.data.sessions[id] = revoked
		count++
	}
	return count, nil
}

func (r sessionRepo) DeleteExpired(before time.Time) (int, error) {
	if before.IsZero() {
		return 0, &domain.ValidationError{Field: "before", Reason: "must be set"}
	}
	count := 0
	for id, session := range r.data.sessions {
		if !session.ExpiresAt().After(before) {
			delete(r.data.sessions, id)
			count++
		}
	}
	return count, nil
}

var _ ports.AuthUnitOfWork = (*AuthUnitOfWork)(nil)
