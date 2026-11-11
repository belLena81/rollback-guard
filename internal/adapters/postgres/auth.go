package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthUnitOfWork runs user and session repositories in one transaction.
type AuthUnitOfWork struct {
	pool *pgxpool.Pool
}

func NewAuthUnitOfWork(pool *pgxpool.Pool) (*AuthUnitOfWork, error) {
	if pool == nil {
		return nil, errors.New("postgres pool is required")
	}
	return &AuthUnitOfWork{pool: pool}, nil
}

func (u *AuthUnitOfWork) WithinAuthTransaction(ctx context.Context, fn func(ports.AuthRepositories) error) error {
	return withinTransaction(ctx, u.pool, func(tx pgx.Tx) error {
		return fn(authRepositories{&repositories{ctx: ctx, tx: tx}})
	})
}

// WithinAuthReadTransaction runs fn in a READ ONLY transaction. The
// repositories handed to fn are the same tx-scoped ones as in a write
// transaction (context rules of the ADR are unchanged); the database, not
// only the narrower port type, refuses any write.
func (u *AuthUnitOfWork) WithinAuthReadTransaction(ctx context.Context, fn func(ports.AuthReadRepositories) error) error {
	return withinReadTransaction(ctx, u.pool, func(tx pgx.Tx) error {
		return fn(authReadRepositories{&repositories{ctx: ctx, tx: tx}})
	})
}

type authReadRepositories struct {
	r *repositories
}

func (a authReadRepositories) Users() ports.UserReader       { return userRepo(a) }
func (a authReadRepositories) Sessions() ports.SessionReader { return sessionRepo(a) }
func (a authReadRepositories) Audit() ports.AuthAuditReader  { return authAuditRepo(a) }

type authRepositories struct {
	r *repositories
}

func (a authRepositories) Users() ports.UserRepository       { return userRepo(a) }
func (a authRepositories) Sessions() ports.SessionRepository { return sessionRepo(a) }
func (a authRepositories) Audit() ports.AuthAuditRepository  { return authAuditRepo(a) }

// userColumns are read through the alias u, both alone and in the session join.
const userColumns = `u.id, u.github_id, u.login, u.email, u.organizations, u.role, u.disabled, u.version, u.created_at, u.updated_at`

const sessionColumns = `s.id, s.user_id, s.token_hash, s.created_at, s.expires_at, s.revoked_at, s.last_seen_at`

const (
	insertUserSQL = `
		INSERT INTO users (id, github_id, login, email, organizations, role, disabled, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	selectUserByIDSQL       = `SELECT ` + userColumns + ` FROM users u WHERE u.id = $1`
	selectUserByGitHubIDSQL = `SELECT ` + userColumns + ` FROM users u WHERE u.github_id = $1`
	updateUserSQL           = `
		UPDATE users SET login = $1, email = $2, organizations = $3, role = $4, disabled = $5, version = $6, updated_at = $7
		WHERE id = $8 AND version = $9 AND github_id = $10 AND created_at = $11`
	selectUserIdentitySQL = `SELECT version, github_id, created_at FROM users WHERE id = $1`

	// insertSessionSQL inserts only for an enabled user and takes FOR SHARE
	// on the user row, so it serializes with a concurrent disable: either
	// the disable's RevokeAllForUser sees this session, or this insert sees
	// the user disabled and writes nothing.
	insertSessionSQL = `
		INSERT INTO sessions (id, user_id, token_hash, created_at, expires_at, revoked_at, last_seen_at)
		SELECT $1::text, u.id, $3::bytea, $4::timestamptz, $5::timestamptz, $6::timestamptz, $7::timestamptz
		FROM users u
		WHERE u.id = $2::text AND NOT u.disabled
		FOR SHARE`
	selectUserDisabledSQL = `SELECT disabled FROM users WHERE id = $1`
	// resolveActiveSessionSQL is one lookup on the token_hash unique index.
	// now is a bound argument, never now(), so the caller's clock decides.
	resolveActiveSessionSQL = `
		SELECT ` + sessionColumns + `, ` + userColumns + `
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
			AND s.revoked_at IS NULL
			AND s.expires_at > $2
			AND NOT u.disabled`
	// Revocation never predates creation (sessions CHECK); the first
	// revocation time is kept.
	revokeSessionSQL = `
		UPDATE sessions SET revoked_at = COALESCE(revoked_at, GREATEST(created_at, $2))
		WHERE id = $1`
	revokeUserSessionsSQL = `
		UPDATE sessions SET revoked_at = GREATEST(created_at, $2)
		WHERE user_id = $1 AND revoked_at IS NULL`
	deleteExpiredSessionsSQL = `DELETE FROM sessions WHERE expires_at <= $1`
)

// foreignKeyViolation is the PostgreSQL SQLSTATE for a foreign key violation.
const foreignKeyViolation = "23503"

type userRepo struct {
	r *repositories
}

func (repo userRepo) Create(user domain.User) error {
	s := user.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, insertUserSQL,
		s.ID, s.GitHubID, s.Login, s.Email, organizationsValue(s.Organizations), s.Role.String(), s.Disabled, s.Version, s.CreatedAt, s.UpdatedAt)
	return mapError(err, "user", string(s.ID))
}

func (repo userRepo) GetByID(id domain.UserID) (domain.User, error) {
	return scanUser(repo.r.tx.QueryRow(repo.r.ctx, selectUserByIDSQL, id), string(id))
}

func (repo userRepo) GetByGitHubID(githubID int64) (domain.User, error) {
	return scanUser(repo.r.tx.QueryRow(repo.r.ctx, selectUserByGitHubIDSQL, githubID), fmt.Sprintf("github:%d", githubID))
}

// Update requires user to be exactly one version past expected (a domain
// change) and never rewrites the immutable GitHub ID or creation time.
func (repo userRepo) Update(user domain.User, expected uint64) error {
	s := user.Snapshot()
	if s.Version != expected+1 {
		return &domain.ValidationError{Field: "user.version", Reason: "must be one past the expected version"}
	}
	command, err := repo.r.tx.Exec(repo.r.ctx, updateUserSQL,
		s.Login, s.Email, organizationsValue(s.Organizations), s.Role.String(), s.Disabled, s.Version, s.UpdatedAt, s.ID, expected,
		s.GitHubID, s.CreatedAt)
	if err != nil {
		return mapError(err, "user", string(s.ID))
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var actual uint64
	var githubID int64
	var createdAt time.Time
	err = repo.r.tx.QueryRow(repo.r.ctx, selectUserIdentitySQL, s.ID).Scan(&actual, &githubID, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.NotFoundError{Resource: "user", ID: string(s.ID)}
	}
	if err != nil {
		return fmt.Errorf("read user version: %w", err)
	}
	if actual != expected {
		return &domain.VersionConflictError{Expected: expected, Actual: actual}
	}
	return &domain.ValidationError{Field: "user", Reason: "github_id and created_at are immutable"}
}

// organizationsValue keeps a nil slice from becoming SQL NULL.
func organizationsValue(organizations []string) []string {
	if organizations == nil {
		return []string{}
	}
	return organizations
}

type userFields struct {
	id, login, email, role string
	githubID               int64
	organizations          []string
	disabled               bool
	version                uint64
	createdAt, updatedAt   time.Time
}

func (f *userFields) targets() []any {
	return []any{&f.id, &f.githubID, &f.login, &f.email, &f.organizations, &f.role, &f.disabled, &f.version, &f.createdAt, &f.updatedAt}
}

func (f *userFields) user() (domain.User, error) {
	role, err := domain.ParseRole(f.role)
	if err != nil {
		return domain.User{}, err
	}
	return domain.RehydrateUser(domain.UserSnapshot{
		ID:            domain.UserID(f.id),
		GitHubID:      f.githubID,
		Login:         f.login,
		Email:         f.email,
		Organizations: f.organizations,
		Role:          role,
		Disabled:      f.disabled,
		CreatedAt:     f.createdAt,
		UpdatedAt:     f.updatedAt,
		Version:       f.version,
	})
}

func scanUser(row pgx.Row, lookup string) (domain.User, error) {
	var fields userFields
	if err := row.Scan(fields.targets()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.User{}, &domain.NotFoundError{Resource: "user", ID: lookup}
		}
		return domain.User{}, fmt.Errorf("scan user: %w", err)
	}
	return fields.user()
}

type sessionRepo struct {
	r *repositories
}

func (repo sessionRepo) Create(session domain.Session) error {
	s := session.Snapshot()
	var revokedAt *time.Time
	if !s.RevokedAt.IsZero() {
		revokedAt = &s.RevokedAt
	}
	command, err := repo.r.tx.Exec(repo.r.ctx, insertSessionSQL,
		s.ID, s.UserID, s.TokenHash.Bytes(), s.CreatedAt, s.ExpiresAt, revokedAt, s.LastSeenAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
		return &domain.NotFoundError{Resource: "user", ID: string(s.UserID)}
	}
	if err != nil {
		// Errors name only the session ID; a duplicate token hash is
		// reported as an existing session without the hash.
		return mapError(err, "session", string(s.ID))
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var disabled bool
	err = repo.r.tx.QueryRow(repo.r.ctx, selectUserDisabledSQL, s.UserID).Scan(&disabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &domain.NotFoundError{Resource: "user", ID: string(s.UserID)}
	case err != nil:
		return fmt.Errorf("read user state: %w", err)
	default:
		return &domain.UserDisabledError{UserID: s.UserID}
	}
}

func (repo sessionRepo) ResolveActive(tokenHash domain.SessionTokenHash, now time.Time) (domain.Session, domain.User, error) {
	if tokenHash.IsZero() {
		return domain.Session{}, domain.User{}, &domain.NoActiveSessionError{}
	}
	if now.IsZero() {
		return domain.Session{}, domain.User{}, &domain.ValidationError{Field: "now", Reason: "must be set"}
	}
	var (
		sessionID, userID              string
		digest                         []byte
		createdAt, expiresAt, lastSeen time.Time
		revokedAt                      *time.Time
		user                           userFields
	)
	targets := append([]any{&sessionID, &userID, &digest, &createdAt, &expiresAt, &revokedAt, &lastSeen}, user.targets()...)
	err := repo.r.tx.QueryRow(repo.r.ctx, resolveActiveSessionSQL, tokenHash.Bytes(), now).Scan(targets...)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.User{}, &domain.NoActiveSessionError{}
	}
	if err != nil {
		return domain.Session{}, domain.User{}, fmt.Errorf("resolve session: %w", err)
	}
	hash, err := domain.NewSessionTokenHash(digest)
	if err != nil {
		return domain.Session{}, domain.User{}, err
	}
	snapshot := domain.SessionSnapshot{
		ID:         domain.SessionID(sessionID),
		UserID:     domain.UserID(userID),
		TokenHash:  hash,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
		LastSeenAt: lastSeen,
	}
	if revokedAt != nil {
		snapshot.RevokedAt = *revokedAt
	}
	session, err := domain.RehydrateSession(snapshot)
	if err != nil {
		return domain.Session{}, domain.User{}, err
	}
	resolved, err := user.user()
	if err != nil {
		return domain.Session{}, domain.User{}, err
	}
	return session, resolved, nil
}

func (repo sessionRepo) Revoke(id domain.SessionID, now time.Time) error {
	if now.IsZero() {
		return &domain.ValidationError{Field: "session.revoked_at", Reason: "must be set"}
	}
	command, err := repo.r.tx.Exec(repo.r.ctx, revokeSessionSQL, id, now)
	if err != nil {
		return mapError(err, "session", string(id))
	}
	if command.RowsAffected() == 0 {
		return &domain.NotFoundError{Resource: "session", ID: string(id)}
	}
	return nil
}

func (repo sessionRepo) RevokeAllForUser(userID domain.UserID, now time.Time) (int, error) {
	if now.IsZero() {
		return 0, &domain.ValidationError{Field: "session.revoked_at", Reason: "must be set"}
	}
	command, err := repo.r.tx.Exec(repo.r.ctx, revokeUserSessionsSQL, userID, now)
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return int(command.RowsAffected()), nil
}

func (repo sessionRepo) DeleteExpired(before time.Time) (int, error) {
	if before.IsZero() {
		return 0, &domain.ValidationError{Field: "before", Reason: "must be set"}
	}
	command, err := repo.r.tx.Exec(repo.r.ctx, deleteExpiredSessionsSQL, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return int(command.RowsAffected()), nil
}

var _ ports.AuthUnitOfWork = (*AuthUnitOfWork)(nil)
