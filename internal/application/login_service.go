package application

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// GitHub login (docs/phase7-auth.md, "GitHub login").
const (
	// LoginStateTTL bounds how long a started login can be completed.
	LoginStateTTL = 10 * time.Minute
	// MinLoginStateKeyBytes is the minimum HMAC key length for the login
	// state (AUTH_STATE_SECRET): 256 bits.
	MinLoginStateKeyBytes = 32
	// MaxAuthorizationCodeLength bounds the code query parameter before it
	// is sent anywhere.
	MaxAuthorizationCodeLength = 512
	// loginStateBytes and pkceVerifierBytes are 256 bits each. The verifier
	// encodes to 43 characters, the RFC 7636 minimum.
	loginStateBytes   = 32
	pkceVerifierBytes = 32
	loginExpiryBytes  = 8
	loginPayloadBytes = loginStateBytes + pkceVerifierBytes + loginExpiryBytes
	// maxLoginAttempts bounds retries of the user upsert when a concurrent
	// login of the same user wins the optimistic lock.
	maxLoginAttempts = 3
	// loginStateMACContext binds the MAC to this one purpose and format.
	loginStateMACContext = "rollback-guard/github-login-state/v1\x00"
)

var loginEncoding = base64.RawURLEncoding.Strict()

var (
	// ErrInvalidLoginRequest: the callback's state cookie is missing,
	// tampered with or expired, its state does not match, or the code is
	// unusable. Nothing was sent to the identity provider.
	ErrInvalidLoginRequest = errors.New("invalid login request")
	// ErrLoginDenied: the identity is not a member of the allowed
	// organization. No user row and no session were written.
	ErrLoginDenied = errors.New("login denied")
)

// LoginDependencies are the ports LoginService needs. Sessions issues the
// session inside the login transaction.
type LoginDependencies struct {
	Sessions    *SessionService
	UnitOfWork  ports.AuthUnitOfWork
	Clock       ports.Clock
	IDGenerator ports.IDGenerator
	Random      ports.RandomSource
	Provider    ports.IdentityProvider
}

// LoginPolicy is the deployment's login configuration.
type LoginPolicy struct {
	// AllowedOrganization is the one GitHub organization whose members may
	// sign in, compared case-insensitively.
	AllowedOrganization string
	// BootstrapAdmins are logins created as administrator on first login.
	BootstrapAdmins []string
	// StateKey authenticates the login state cookie (HMAC-SHA256).
	StateKey []byte
}

// LoginStart is what the login endpoint hands to the browser: the provider
// URL to redirect to, and the sealed state to keep in a short-lived cookie.
type LoginStart struct {
	AuthorizationURL string
	SealedState      string
	ExpiresAt        time.Time
}

// LoginResult is a completed login. Token is the raw session token, to be
// handed to the client once and then dropped.
type LoginResult struct {
	Token   string
	Session domain.Session
	User    domain.User
	Actor   domain.Actor
}

// LoginService runs the orchestrator side of the GitHub OAuth login: state
// and PKCE generation, state verification, organization allow-list, user
// upsert with bootstrap admins, and session issue. It never sees the
// provider access token and never logs.
type LoginService struct {
	sessions        *SessionService
	uow             ports.AuthUnitOfWork
	clock           ports.Clock
	ids             ports.IDGenerator
	random          ports.RandomSource
	provider        ports.IdentityProvider
	allowedOrg      string
	bootstrapAdmins map[string]struct{}
	stateKey        []byte
}

func NewLoginService(dependencies LoginDependencies, policy LoginPolicy) (*LoginService, error) {
	if dependencies.Sessions == nil || dependencies.UnitOfWork == nil || dependencies.Clock == nil ||
		dependencies.IDGenerator == nil || dependencies.Random == nil || dependencies.Provider == nil {
		return nil, errors.New("all login service dependencies are required")
	}
	if !domain.IsGitHubLogin(policy.AllowedOrganization) {
		return nil, &domain.ValidationError{Field: "allowed_organization", Reason: "must be a GitHub organization login"}
	}
	admins := make(map[string]struct{}, len(policy.BootstrapAdmins))
	for _, login := range policy.BootstrapAdmins {
		if !domain.IsGitHubLogin(login) {
			return nil, &domain.ValidationError{Field: "bootstrap_admins", Reason: "each must be a GitHub login"}
		}
		admins[strings.ToLower(login)] = struct{}{}
	}
	if len(policy.StateKey) < MinLoginStateKeyBytes {
		return nil, &domain.ValidationError{Field: "state_key", Reason: fmt.Sprintf("must be at least %d bytes", MinLoginStateKeyBytes)}
	}
	return &LoginService{
		sessions:        dependencies.Sessions,
		uow:             dependencies.UnitOfWork,
		clock:           dependencies.Clock,
		ids:             dependencies.IDGenerator,
		random:          dependencies.Random,
		provider:        dependencies.Provider,
		allowedOrg:      policy.AllowedOrganization,
		bootstrapAdmins: admins,
		stateKey:        append([]byte(nil), policy.StateKey...),
	}, nil
}

// Begin starts a login: a fresh 256-bit state and PKCE verifier, sealed with
// their expiry under the state key.
func (s *LoginService) Begin(_ context.Context) (LoginStart, error) {
	payload := make([]byte, loginPayloadBytes)
	if err := s.random.Fill(payload[:loginStateBytes+pkceVerifierBytes]); err != nil {
		return LoginStart{}, fmt.Errorf("generate login state: %w", err)
	}
	expiresAt := s.clock.Now().Add(LoginStateTTL).Truncate(time.Second)
	binary.BigEndian.PutUint64(payload[loginStateBytes+pkceVerifierBytes:], uint64(expiresAt.Unix())) //nolint:gosec // a clock after 1970
	state, verifier := splitLoginPayload(payload)
	challenge := sha256.Sum256([]byte(verifier))
	return LoginStart{
		AuthorizationURL: s.provider.AuthorizationURL(state, loginEncoding.EncodeToString(challenge[:])),
		SealedState:      loginEncoding.EncodeToString(payload) + "." + loginEncoding.EncodeToString(s.mac(payload)),
		ExpiresAt:        expiresAt.UTC(),
	}, nil
}

// Complete finishes a login from the callback's sealed state cookie, state
// and code query parameters. The cookie is verified before anything is sent
// to the provider. A non-member gets ErrLoginDenied (a known user who left
// the organization also loses every open session) and a disabled user
// *domain.UserDisabledError; neither writes a row or a session.
func (s *LoginService) Complete(ctx context.Context, sealedState, state, code string) (LoginResult, error) {
	verifier, ok := s.open(sealedState, state)
	if !ok || !usableCode(code) {
		return LoginResult{}, ErrInvalidLoginRequest
	}
	identity, err := s.provider.Exchange(ctx, code, verifier)
	if err != nil {
		return LoginResult{}, err
	}
	if !s.member(identity) {
		if err := s.revokeFormerMember(ctx, identity.GitHubID); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrLoginDenied
	}
	_, err = domain.NewUser("identity-check", identity.GitHubID, identity.Login, identity.Email, identity.Organizations, domain.RoleViewer, s.clock.Now())
	if err != nil {
		// The provider's data is unusable; its content is not reported.
		return LoginResult{}, fmt.Errorf("github identity: %w", ports.ErrIdentityProviderFailed)
	}
	var result LoginResult
	for attempt := 1; ; attempt++ {
		result, err = s.signIn(ctx, identity)
		if err == nil || attempt == maxLoginAttempts || !retryableLogin(err) {
			break
		}
	}
	if err != nil {
		return LoginResult{}, err
	}
	return result, nil
}

// revokeFormerMember revokes every open session of a known user who is no
// longer in the allowed organization. No row is created for an unknown
// identity, and the user row itself is left unchanged so that re-admission to
// the organization restores access.
func (s *LoginService) revokeFormerMember(ctx context.Context, githubID int64) error {
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByGitHubID(githubID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load user: %w", err)
		}
		if _, err := repos.Sessions().RevokeAllForUser(user.ID(), s.clock.Now()); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("revoke former member sessions: %w", err)
	}
	return nil
}

// signIn upserts the user and issues the session in one transaction.
func (s *LoginService) signIn(ctx context.Context, identity ports.GitHubIdentity) (LoginResult, error) {
	var result LoginResult
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		user, err := s.upsertUser(repos, identity)
		if err != nil {
			return err
		}
		token, session, err := s.sessions.issueIn(repos, user)
		if err != nil {
			return err
		}
		actor, err := domain.ActorFor(user)
		if err != nil {
			return err
		}
		result = LoginResult{Token: token, Session: session, User: user, Actor: actor}
		return nil
	})
	if err != nil {
		return LoginResult{}, err
	}
	return result, nil
}

// upsertUser creates a first-time user, elevating bootstrap admins, or
// refreshes an existing user's profile. An existing user's role is never
// changed here.
func (s *LoginService) upsertUser(repos ports.AuthRepositories, identity ports.GitHubIdentity) (domain.User, error) {
	now := s.clock.Now()
	user, err := repos.Users().GetByGitHubID(identity.GitHubID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		role := domain.RoleViewer
		if _, ok := s.bootstrapAdmins[strings.ToLower(identity.Login)]; ok {
			role = domain.RoleAdministrator
		}
		user, err = domain.NewUser(domain.UserID(s.ids.NewID("user")), identity.GitHubID, identity.Login, identity.Email, identity.Organizations, role, now)
		if err != nil {
			return domain.User{}, err
		}
		if err := repos.Users().Create(user); err != nil {
			return domain.User{}, fmt.Errorf("create user: %w", err)
		}
		return user, nil
	case err != nil:
		return domain.User{}, fmt.Errorf("load user: %w", err)
	}
	if user.Disabled() {
		return domain.User{}, &domain.UserDisabledError{UserID: user.ID()}
	}
	if user.SameProfile(identity.Login, identity.Email, identity.Organizations) {
		return user, nil
	}
	if now.Before(user.UpdatedAt()) {
		now = user.UpdatedAt()
	}
	refreshed, err := user.WithProfile(identity.Login, identity.Email, identity.Organizations, now)
	if err != nil {
		return domain.User{}, err
	}
	if err := repos.Users().Update(refreshed, user.Version()); err != nil {
		return domain.User{}, fmt.Errorf("refresh user: %w", err)
	}
	return refreshed, nil
}

// retryableLogin reports a lost race with a concurrent login of the same
// user: a version conflict on refresh, or a duplicate on first creation.
func retryableLogin(err error) bool {
	return errors.Is(err, domain.ErrVersionConflict) || errors.Is(err, domain.ErrAlreadyExists)
}

func (s *LoginService) member(identity ports.GitHubIdentity) bool {
	for _, org := range identity.Organizations {
		if strings.EqualFold(strings.TrimSpace(org), s.allowedOrg) {
			return true
		}
	}
	return false
}

// open verifies the sealed state's MAC and expiry and that it carries
// state, and returns its PKCE verifier. Every failure is the same false.
func (s *LoginService) open(sealedState, state string) (string, bool) {
	encodedPayload, encodedMAC, ok := strings.Cut(sealedState, ".")
	if !ok || len(encodedPayload) != loginEncoding.EncodedLen(loginPayloadBytes) || len(encodedMAC) != loginEncoding.EncodedLen(sha256.Size) {
		return "", false
	}
	payload, err := loginEncoding.DecodeString(encodedPayload)
	if err != nil || len(payload) != loginPayloadBytes {
		return "", false
	}
	mac, err := loginEncoding.DecodeString(encodedMAC)
	if err != nil || !hmac.Equal(mac, s.mac(payload)) {
		return "", false
	}
	expiry := binary.BigEndian.Uint64(payload[loginStateBytes+pkceVerifierBytes:])
	if !s.clock.Now().Before(time.Unix(int64(expiry), 0)) { //nolint:gosec // authenticated value written by Begin
		return "", false
	}
	sealedStateValue, verifier := splitLoginPayload(payload)
	if subtle.ConstantTimeCompare([]byte(sealedStateValue), []byte(state)) != 1 {
		return "", false
	}
	return verifier, true
}

func (s *LoginService) mac(payload []byte) []byte {
	h := hmac.New(sha256.New, s.stateKey)
	h.Write([]byte(loginStateMACContext))
	h.Write(payload)
	return h.Sum(nil)
}

// splitLoginPayload returns the encoded state and PKCE verifier.
func splitLoginPayload(payload []byte) (string, string) {
	return loginEncoding.EncodeToString(payload[:loginStateBytes]),
		loginEncoding.EncodeToString(payload[loginStateBytes : loginStateBytes+pkceVerifierBytes])
}

// usableCode accepts a non-empty, bounded, printable-ASCII code.
func usableCode(code string) bool {
	if code == "" || len(code) > MaxAuthorizationCodeLength {
		return false
	}
	for i := range len(code) {
		if code[i] <= ' ' || code[i] > '~' {
			return false
		}
	}
	return true
}
