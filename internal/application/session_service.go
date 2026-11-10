package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// SessionTokenBytes is the entropy of a session token: 256 bits.
const SessionTokenBytes = 32

// sessionTokenEncoding renders a token as unpadded base64url. Strict decoding
// rejects non-canonical encodings, so each token has exactly one spelling.
var sessionTokenEncoding = base64.RawURLEncoding.Strict()

// SessionDependencies are the ports SessionService needs.
type SessionDependencies struct {
	UnitOfWork  ports.AuthUnitOfWork
	Clock       ports.Clock
	IDGenerator ports.IDGenerator
	Random      ports.RandomSource
}

// SessionService issues, resolves and revokes server-side sessions
// (docs/adr-auth-sessions.md). The raw token leaves Issue exactly once; only
// its SHA-256 is stored, and the service never logs or formats it. It makes
// no authorization decisions.
type SessionService struct {
	uow    ports.AuthUnitOfWork
	clock  ports.Clock
	ids    ports.IDGenerator
	random ports.RandomSource
	ttl    time.Duration
}

func NewSessionService(dependencies SessionDependencies, ttl time.Duration) (*SessionService, error) {
	if dependencies.UnitOfWork == nil || dependencies.Clock == nil || dependencies.IDGenerator == nil || dependencies.Random == nil {
		return nil, errors.New("all session service dependencies are required")
	}
	if ttl <= 0 {
		return nil, &domain.ValidationError{Field: "session_ttl", Reason: "must be positive"}
	}
	return &SessionService{
		uow:    dependencies.UnitOfWork,
		clock:  dependencies.Clock,
		ids:    dependencies.IDGenerator,
		random: dependencies.Random,
		ttl:    ttl,
	}, nil
}

// Issue creates a session for user and returns its raw token, which the
// caller must hand to the client and then drop. A disabled user, by the
// given value or by the stored row, gets no session.
func (s *SessionService) Issue(ctx context.Context, user domain.User) (string, domain.Session, error) {
	if _, err := domain.ActorFor(user); err != nil {
		return "", domain.Session{}, err
	}
	secret := make([]byte, SessionTokenBytes)
	if err := s.random.Fill(secret); err != nil {
		return "", domain.Session{}, fmt.Errorf("generate session token: %w", err)
	}
	hash, err := hashToken(secret)
	if err != nil {
		return "", domain.Session{}, err
	}
	now := s.clock.Now()
	session, err := domain.NewSession(domain.SessionID(s.ids.NewID("session")), user.ID(), hash, now, now.Add(s.ttl))
	if err != nil {
		return "", domain.Session{}, err
	}
	if err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		return repos.Sessions().Create(session)
	}); err != nil {
		return "", domain.Session{}, fmt.Errorf("create session: %w", err)
	}
	return sessionTokenEncoding.EncodeToString(secret), session, nil
}

// Resolve returns the Principal behind rawToken, reading the live session and
// user rows. Every authentication failure returns the same
// *domain.UnauthenticatedError; any other error is an infrastructure failure
// that callers must not report as 401.
func (s *SessionService) Resolve(ctx context.Context, rawToken string) (domain.Principal, error) {
	hash, ok := parseToken(rawToken)
	if !ok {
		return domain.Principal{}, &domain.UnauthenticatedError{}
	}
	var principal domain.Principal
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		session, user, err := repos.Sessions().ResolveActive(hash, s.clock.Now())
		if err != nil {
			return err
		}
		principal, err = domain.NewPrincipal(user, session.ID())
		if err != nil {
			return &domain.NoActiveSessionError{}
		}
		return nil
	})
	switch {
	case err == nil:
		return principal, nil
	case errors.Is(err, domain.ErrNoActiveSession):
		return domain.Principal{}, &domain.UnauthenticatedError{}
	default:
		return domain.Principal{}, fmt.Errorf("resolve session: %w", err)
	}
}

// Revoke revokes one session; it takes effect on the next Resolve.
func (s *SessionService) Revoke(ctx context.Context, id domain.SessionID) error {
	return s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		return repos.Sessions().Revoke(id, s.clock.Now())
	})
}

// RevokeAllForUser revokes every active session of the user and returns how
// many it revoked.
func (s *SessionService) RevokeAllForUser(ctx context.Context, userID domain.UserID) (int, error) {
	var count int
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		var err error
		count, err = repos.Sessions().RevokeAllForUser(userID, s.clock.Now())
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// parseToken decodes a canonical token and returns its hash. Malformed input
// reports false without saying why.
func parseToken(rawToken string) (domain.SessionTokenHash, bool) {
	if len(rawToken) != sessionTokenEncoding.EncodedLen(SessionTokenBytes) {
		return domain.SessionTokenHash{}, false
	}
	secret, err := sessionTokenEncoding.DecodeString(rawToken)
	if err != nil || len(secret) != SessionTokenBytes {
		return domain.SessionTokenHash{}, false
	}
	hash, err := hashToken(secret)
	if err != nil {
		return domain.SessionTokenHash{}, false
	}
	return hash, true
}

func hashToken(secret []byte) (domain.SessionTokenHash, error) {
	digest := sha256.Sum256(secret)
	hash, err := domain.NewSessionTokenHash(digest[:])
	if err != nil {
		return domain.SessionTokenHash{}, fmt.Errorf("hash session token: %w", err)
	}
	return hash, nil
}
