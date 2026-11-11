package application

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// AdminDependencies are the ports AdminService needs.
type AdminDependencies struct {
	UnitOfWork  ports.AuthUnitOfWork
	Access      AccessPolicy
	Clock       ports.Clock
	IDGenerator ports.IDGenerator
}

// AdminService runs user administration and logout. Every change commits in
// one auth transaction together with its audit event, so a rolled-back
// change leaves no audit row and a committed one always has one.
type AdminService struct {
	uow    ports.AuthUnitOfWork
	access AccessPolicy
	clock  ports.Clock
	ids    ports.IDGenerator
}

func NewAdminService(dependencies AdminDependencies) (*AdminService, error) {
	if dependencies.UnitOfWork == nil || dependencies.Access == nil || dependencies.Clock == nil || dependencies.IDGenerator == nil {
		return nil, errors.New("all admin service dependencies are required")
	}
	return &AdminService{
		uow:    dependencies.UnitOfWork,
		access: dependencies.Access,
		clock:  dependencies.Clock,
		ids:    dependencies.IDGenerator,
	}, nil
}

// ChangeRole sets the target user's role. Setting the role a user already
// has changes nothing and writes no audit event. A change that would leave
// no enabled administrator, including an administrator demoting themselves
// as the last one, returns *domain.LastAdministratorError.
func (s *AdminService) ChangeRole(ctx context.Context, actor domain.Actor, target domain.ActorID, role domain.Role) (domain.User, error) {
	if err := s.access.Authorize(ctx, actor, domain.ActionAdminister); err != nil {
		return domain.User{}, fmt.Errorf("authorize change role: %w", err)
	}
	if !role.Valid() {
		return domain.User{}, &domain.ValidationError{Field: "role", Reason: "must be viewer, operator or administrator"}
	}
	var result domain.User
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		admins, err := s.lockAdministrators(repos, actor)
		if err != nil {
			return err
		}
		user, err := loadTarget(repos, target)
		if err != nil {
			return err
		}
		if user.Role() == role {
			result = user
			return nil
		}
		if slices.Contains(admins, user.ID()) && len(admins) == 1 {
			return &domain.LastAdministratorError{}
		}
		now := s.clock.Now()
		changed, err := user.WithRole(role, now)
		if err != nil {
			return fmt.Errorf("change role: %w", err)
		}
		if err := repos.Users().Update(changed, user.Version()); err != nil {
			return fmt.Errorf("update user: %w", err)
		}
		event, err := domain.NewRoleChangedAudit(s.auditID(), actor, user.ID(), user.Role(), role, now)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		if err := repos.Audit().Append(event); err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		result = changed
		return nil
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("change role: %w", err)
	}
	return result, nil
}

// RevokeUserSessions revokes every open session of the target user and
// returns how many it revoked. The audit event is written even when the
// count is zero, because the administrator's action itself is the record.
func (s *AdminService) RevokeUserSessions(ctx context.Context, actor domain.Actor, target domain.ActorID) (int, error) {
	if err := s.access.Authorize(ctx, actor, domain.ActionAdminister); err != nil {
		return 0, fmt.Errorf("authorize revoke sessions: %w", err)
	}
	var revoked int
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		if _, err := s.lockAdministrators(repos, actor); err != nil {
			return err
		}
		user, err := loadTarget(repos, target)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		count, err := repos.Sessions().RevokeAllForUser(user.ID(), now)
		if err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		event, err := domain.NewSessionsRevokedAudit(s.auditID(), actor, user.ID(), count, now)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		if err := repos.Audit().Append(event); err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		revoked = count
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("revoke user sessions: %w", err)
	}
	return revoked, nil
}

// DisableUser disables the target user and revokes every open session of
// theirs in the same auth transaction, so their next request is rejected.
// Disabling an already disabled user changes nothing and writes no audit
// event. Disabling the last enabled administrator, including oneself,
// returns *domain.LastAdministratorError.
func (s *AdminService) DisableUser(ctx context.Context, actor domain.Actor, target domain.ActorID) (domain.User, error) {
	if err := s.access.Authorize(ctx, actor, domain.ActionAdminister); err != nil {
		return domain.User{}, fmt.Errorf("authorize disable user: %w", err)
	}
	var result domain.User
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		admins, err := s.lockAdministrators(repos, actor)
		if err != nil {
			return err
		}
		user, err := loadTarget(repos, target)
		if err != nil {
			return err
		}
		if user.Disabled() {
			result = user
			return nil
		}
		if slices.Contains(admins, user.ID()) && len(admins) == 1 {
			return &domain.LastAdministratorError{}
		}
		now := s.clock.Now()
		disabled, err := user.Disable(now)
		if err != nil {
			return fmt.Errorf("disable user: %w", err)
		}
		if err := repos.Users().Update(disabled, user.Version()); err != nil {
			return fmt.Errorf("update user: %w", err)
		}
		count, err := repos.Sessions().RevokeAllForUser(user.ID(), now)
		if err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		event, err := domain.NewUserDisabledAudit(s.auditID(), actor, user.ID(), count, now)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		if err := repos.Audit().Append(event); err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		result = disabled
		return nil
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("disable user: %w", err)
	}
	return result, nil
}

// EnableUser enables a disabled target user. Sessions revoked by the
// disable stay revoked; the user must log in again. Enabling an enabled
// user changes nothing and writes no audit event.
func (s *AdminService) EnableUser(ctx context.Context, actor domain.Actor, target domain.ActorID) (domain.User, error) {
	if err := s.access.Authorize(ctx, actor, domain.ActionAdminister); err != nil {
		return domain.User{}, fmt.Errorf("authorize enable user: %w", err)
	}
	var result domain.User
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		if _, err := s.lockAdministrators(repos, actor); err != nil {
			return err
		}
		user, err := loadTarget(repos, target)
		if err != nil {
			return err
		}
		if !user.Disabled() {
			result = user
			return nil
		}
		now := s.clock.Now()
		enabled, err := user.Enable(now)
		if err != nil {
			return fmt.Errorf("enable user: %w", err)
		}
		if err := repos.Users().Update(enabled, user.Version()); err != nil {
			return fmt.Errorf("update user: %w", err)
		}
		event, err := domain.NewUserEnabledAudit(s.auditID(), actor, user.ID(), now)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		if err := repos.Audit().Append(event); err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		result = enabled
		return nil
	})
	if err != nil {
		return domain.User{}, fmt.Errorf("enable user: %w", err)
	}
	return result, nil
}

// Logout revokes the principal's own session. Any authenticated user may
// end their own session, so no role is required.
func (s *AdminService) Logout(ctx context.Context, principal domain.Principal) error {
	if !principal.Valid() {
		return &domain.UnauthenticatedError{}
	}
	err := s.uow.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		now := s.clock.Now()
		if err := repos.Sessions().Revoke(principal.SessionID(), now); err != nil {
			return fmt.Errorf("revoke session: %w", err)
		}
		event, err := domain.NewLogoutAudit(s.auditID(), principal.Actor(), principal.User().ID(), now)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		if err := repos.Audit().Append(event); err != nil {
			return fmt.Errorf("append audit event: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// lockAdministrators locks the enabled administrators for the rest of the
// transaction and re-checks, under that lock, that actor is still one of
// them. The AccessPolicy check ran in an earlier transaction; this closes
// the window in which the actor could have been demoted or disabled since.
func (s *AdminService) lockAdministrators(repos ports.AuthRepositories, actor domain.Actor) ([]domain.UserID, error) {
	admins, err := repos.Users().LockActiveAdministrators()
	if err != nil {
		return nil, fmt.Errorf("lock administrators: %w", err)
	}
	githubID, ok := githubIDOf(actor.ID())
	if !ok {
		return nil, &domain.ForbiddenError{Action: domain.ActionAdminister}
	}
	user, err := repos.Users().GetByGitHubID(githubID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, &domain.UnauthenticatedError{}
	}
	if err != nil {
		return nil, fmt.Errorf("load actor: %w", err)
	}
	if !slices.Contains(admins, user.ID()) {
		return nil, &domain.ForbiddenError{Role: user.Role(), Action: domain.ActionAdminister}
	}
	return admins, nil
}

// loadTarget resolves a target user by its public actor ID
// ("github:<id>"), the identifier the API exposes.
func loadTarget(repos ports.AuthRepositories, target domain.ActorID) (domain.User, error) {
	githubID, ok := githubIDOf(target)
	if !ok {
		return domain.User{}, &domain.ValidationError{Field: "userId", Reason: "must be github:<numeric id>"}
	}
	user, err := repos.Users().GetByGitHubID(githubID)
	if err != nil {
		return domain.User{}, fmt.Errorf("load user: %w", err)
	}
	return user, nil
}

func (s *AdminService) auditID() domain.AuthAuditEventID {
	return domain.AuthAuditEventID(s.ids.NewID("auth-audit"))
}
