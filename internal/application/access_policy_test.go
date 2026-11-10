package application

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// switchablePolicy lets a harness set up state permissively and then switch
// to the policy under test. The harness's own inspection reads (by
// testReader) stay allowed so assertions do not depend on the policy.
type switchablePolicy struct{ current AccessPolicy }

func (p *switchablePolicy) Authorize(ctx context.Context, actor domain.Actor, action domain.Action) error {
	if actor == testReader() && action == domain.ActionRead {
		return nil
	}
	return p.current.Authorize(ctx, actor, action)
}

var userCreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// authUsers seeds a memory auth store. GitHub IDs: 1 viewer, 2 operator,
// 3 administrator, 4 disabled administrator.
func authUsers(t *testing.T) *memory.AuthUnitOfWork {
	t.Helper()
	store := memory.NewAuthUnitOfWork()
	seed := []struct {
		id       int64
		role     domain.Role
		disabled bool
	}{
		{1, domain.RoleViewer, false},
		{2, domain.RoleOperator, false},
		{3, domain.RoleAdministrator, false},
		{4, domain.RoleAdministrator, true},
	}
	err := store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		for _, s := range seed {
			user, err := domain.NewUser(domain.UserID(fmt.Sprintf("user-%d", s.id)), s.id, fmt.Sprintf("login-%d", s.id), "", nil, s.role, userCreatedAt)
			if err != nil {
				return err
			}
			if s.disabled {
				if user, err = user.Disable(userCreatedAt); err != nil {
					return err
				}
			}
			if err := repos.Users().Create(user); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func githubActor(t *testing.T, id int64) domain.Actor {
	t.Helper()
	return mustActor(t, fmt.Sprintf("github:%d", id), domain.ActorHuman)
}

func newRolePolicy(t *testing.T, store ports.AuthUnitOfWork) *RoleAccessPolicy {
	t.Helper()
	policy, err := NewRoleAccessPolicy(store)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestRoleAccessPolicyMatrix(t *testing.T) {
	policy := newRolePolicy(t, authUsers(t))
	actions := []domain.Action{
		domain.ActionRead, domain.ActionCreateIncident, domain.ActionRejectIncident,
		domain.ActionCancelIncident, domain.ActionApproveRollback, domain.ActionAdminister,
	}
	allowed := map[int64]map[domain.Action]bool{
		1: {domain.ActionRead: true},
		2: {domain.ActionRead: true, domain.ActionCreateIncident: true, domain.ActionRejectIncident: true, domain.ActionCancelIncident: true},
		3: {domain.ActionRead: true, domain.ActionCreateIncident: true, domain.ActionRejectIncident: true, domain.ActionCancelIncident: true, domain.ActionApproveRollback: true, domain.ActionAdminister: true},
	}
	for githubID, permitted := range allowed {
		for _, action := range actions {
			t.Run(fmt.Sprintf("github:%d/%s", githubID, action), func(t *testing.T) {
				err := policy.Authorize(context.Background(), githubActor(t, githubID), action)
				if permitted[action] {
					if err != nil {
						t.Fatalf("Authorize = %v, want allowed", err)
					}
					return
				}
				var forbidden *domain.ForbiddenError
				if !errors.As(err, &forbidden) || forbidden.Action != action {
					t.Fatalf("Authorize = %v, want *ForbiddenError for %s", err, action)
				}
			})
		}
	}
}

func TestRoleAccessPolicyDeniesUnresolvableAndNonHumanActors(t *testing.T) {
	policy := newRolePolicy(t, authUsers(t))
	tests := []struct {
		name  string
		actor domain.Actor
		want  error
	}{
		{name: "disabled user", actor: githubActor(t, 4), want: domain.ErrUnauthenticated},
		{name: "unknown user", actor: githubActor(t, 99), want: domain.ErrUnauthenticated},
		{name: "non-github human", actor: mustActor(t, "local-admin", domain.ActorHuman), want: domain.ErrUnauthenticated},
		{name: "non-canonical id", actor: mustActor(t, "github:03", domain.ActorHuman), want: domain.ErrUnauthenticated},
		{name: "system actor named like an admin", actor: mustActor(t, "github:3", domain.ActorSystem), want: domain.ErrForbidden},
		{name: "agent actor", actor: mustActor(t, "github:3", domain.ActorAgent), want: domain.ErrForbidden},
		{name: "zero actor", actor: domain.Actor{}, want: domain.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := policy.Authorize(context.Background(), tt.actor, domain.ActionRead); !errors.Is(err, tt.want) {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestRoleAccessPolicyReadsTheLiveRole(t *testing.T) {
	store := authUsers(t)
	policy := newRolePolicy(t, store)
	admin := githubActor(t, 3)
	if err := policy.Authorize(context.Background(), admin, domain.ActionApproveRollback); err != nil {
		t.Fatal(err)
	}
	setRole(t, store, 3, domain.RoleOperator)
	if err := policy.Authorize(context.Background(), admin, domain.ActionApproveRollback); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("after demotion Authorize = %v, want forbidden", err)
	}
}

func setRole(t *testing.T, store ports.AuthUnitOfWork, githubID int64, role domain.Role) {
	t.Helper()
	err := store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByGitHubID(githubID)
		if err != nil {
			return err
		}
		changed, err := user.WithRole(role, user.UpdatedAt().Add(time.Second))
		if err != nil {
			return err
		}
		return repos.Users().Update(changed, user.Version())
	})
	if err != nil {
		t.Fatal(err)
	}
}

type failingAuthUnitOfWork struct{}

func (failingAuthUnitOfWork) WithinAuthTransaction(context.Context, func(ports.AuthRepositories) error) error {
	return errors.New("database unavailable")
}

func TestRoleAccessPolicyInfrastructureFailureIsNotAuthDecision(t *testing.T) {
	policy := newRolePolicy(t, failingAuthUnitOfWork{})
	err := policy.Authorize(context.Background(), githubActor(t, 3), domain.ActionRead)
	if err == nil || errors.Is(err, domain.ErrUnauthenticated) || errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("Authorize = %v, want an infrastructure error", err)
	}
}

func TestAccessPolicyConstructorsFailClosed(t *testing.T) {
	if _, err := NewRoleAccessPolicy(nil); err == nil {
		t.Fatal("NewRoleAccessPolicy(nil) succeeded")
	}
	if _, err := NewLocalAccessPolicy(mustActor(t, "guardian", domain.ActorSystem)); err == nil {
		t.Fatal("NewLocalAccessPolicy accepted a system actor")
	}
	if _, err := NewLocalAccessPolicy(domain.Actor{}); err == nil {
		t.Fatal("NewLocalAccessPolicy accepted the zero actor")
	}
	_, err := NewServices(Dependencies{
		UnitOfWork: memory.NewUnitOfWork(), Inspector: nilInfra{}, Metrics: nilInfra{}, Executor: nilInfra{},
		RollbackPolicy: domain.EligibilityPolicy{}, ApprovalPolicy: domain.ApprovalAuthorizer{}, Clock: nilClock{}, IDGenerator: nilIDs{},
	}, WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: mustActor(t, "guardian", domain.ActorSystem)})
	if err == nil {
		t.Fatal("NewServices accepted a nil AccessPolicy")
	}
}

func TestLocalAccessPolicy(t *testing.T) {
	local := mustActor(t, "local-admin", domain.ActorHuman)
	policy, err := NewLocalAccessPolicy(local)
	if err != nil {
		t.Fatal(err)
	}
	for action := domain.ActionRead; action <= domain.ActionAdminister; action++ {
		if err := policy.Authorize(context.Background(), local, action); err != nil {
			t.Fatalf("local actor %s = %v, want allowed", action, err)
		}
	}
	for _, other := range []domain.Actor{
		mustActor(t, "someone-else", domain.ActorHuman),
		mustActor(t, "local-admin", domain.ActorSystem),
		mustActor(t, "local-admin", domain.ActorAgent),
		{},
	} {
		if err := policy.Authorize(context.Background(), other, domain.ActionRead); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("actor %v = %v, want forbidden", other, err)
		}
	}
	if err := policy.Authorize(context.Background(), local, domain.Action(0)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("invalid action = %v, want forbidden", err)
	}
}

// deniedPolicy records each call and denies it.
type deniedPolicy struct{ calls []domain.Action }

func (p *deniedPolicy) Authorize(_ context.Context, _ domain.Actor, action domain.Action) error {
	p.calls = append(p.calls, action)
	return &domain.ForbiddenError{Role: domain.RoleViewer, Action: action}
}

// TestHumanUseCasesAuthorizeBeforeAnyTransaction: every human use case asks
// the policy for its own action first, and a denial opens no transaction.
// The typed denial survives the services' error wrapping.
func TestHumanUseCasesAuthorizeBeforeAnyTransaction(t *testing.T) {
	ctx := context.Background()
	human := mustActor(t, "alice", domain.ActorHuman)
	approval := ApprovalCommand{IncidentID: "incident-1", Actor: human, ActionID: "action-1", ProposalHash: "hash"}
	tests := []struct {
		name string
		want domain.Action
		run  func(Services) error
	}{
		{"create", domain.ActionCreateIncident, func(s Services) error {
			_, err := s.Incidents.CreateIncidentAsync(ctx, CreateIncidentCommand{Actor: human})
			return err
		}},
		{"cancel", domain.ActionCancelIncident, func(s Services) error {
			_, err := s.Incidents.Cancel(ctx, CancelIncidentCommand{IncidentID: "incident-1", Actor: human, Reason: "r"})
			return err
		}},
		{"approve", domain.ActionApproveRollback, func(s Services) error {
			_, err := s.Approvals.Approve(ctx, approval)
			return err
		}},
		{"approve and execute", domain.ActionApproveRollback, func(s Services) error {
			_, err := s.ApproveAndExecute.Run(ctx, approval)
			return err
		}},
		{"reject", domain.ActionRejectIncident, func(s Services) error {
			_, err := s.Approvals.Reject(ctx, RejectionCommand{IncidentID: "incident-1", Actor: human, ActionID: "action-1", ProposalHash: "hash", Reason: "r"})
			return err
		}},
		{"get incident", domain.ActionRead, func(s Services) error {
			_, err := s.Queries.GetIncident(ctx, human, "incident-1")
			return err
		}},
		{"get summary", domain.ActionRead, func(s Services) error {
			_, err := s.Queries.GetIncidentSummary(ctx, human, "incident-1")
			return err
		}},
		{"list events", domain.ActionRead, func(s Services) error {
			_, err := s.Queries.ListEvents(ctx, human, "incident-1")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spy := &transactionSpyUnitOfWork{}
			policy := &deniedPolicy{}
			services, err := NewServices(Dependencies{
				UnitOfWork: spy, Inspector: nilInfra{}, Metrics: nilInfra{}, Executor: nilInfra{},
				RollbackPolicy: domain.EligibilityPolicy{}, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: policy, Clock: nilClock{}, IDGenerator: nilIDs{},
			}, WorkflowConfig{ProposalTTL: time.Minute, ApprovalTTL: time.Minute, CleanupTimeout: time.Second, SystemActor: mustActor(t, "guardian", domain.ActorSystem)})
			if err != nil {
				t.Fatal(err)
			}
			err = tt.run(services)
			var forbidden *domain.ForbiddenError
			if !errors.As(err, &forbidden) || forbidden.Action != tt.want || forbidden.Role != domain.RoleViewer {
				t.Fatalf("error = %v, want *ForbiddenError{viewer, %s}", err, tt.want)
			}
			if spy.calls != 0 {
				t.Fatalf("%d transactions opened after denial", spy.calls)
			}
			if len(policy.calls) != 1 || policy.calls[0] != tt.want {
				t.Fatalf("policy calls = %v, want [%s]", policy.calls, tt.want)
			}
		})
	}
}

type nilInfra struct{}

func (nilInfra) InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return domain.DeploymentEvidence{}, errors.New("unused")
}

func (nilInfra) Observe(context.Context, domain.DeploymentTarget, domain.Symptom) (domain.MetricEvidence, error) {
	return domain.MetricEvidence{}, errors.New("unused")
}

func (nilInfra) ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (domain.RollbackExecution, error) {
	return domain.RollbackExecution{}, errors.New("unused")
}

type nilClock struct{}

func (nilClock) Now() time.Time { return userCreatedAt }

type nilIDs struct{}

func (nilIDs) NewID(prefix string) string { return prefix + "-1" }

// TestRoleEnforcementOnApprovalPath drives the real approval path with the
// role policy: only an administrator approves, the recorded approver is the
// authorized actor, and system actors cannot use human use cases.
func TestRoleEnforcementOnApprovalPath(t *testing.T) {
	tests := []struct {
		name    string
		actor   func(*testing.T) domain.Actor
		wantErr error
	}{
		{name: "viewer", actor: func(t *testing.T) domain.Actor { return githubActor(t, 1) }, wantErr: domain.ErrForbidden},
		{name: "operator", actor: func(t *testing.T) domain.Actor { return githubActor(t, 2) }, wantErr: domain.ErrForbidden},
		{name: "disabled administrator", actor: func(t *testing.T) domain.Actor { return githubActor(t, 4) }, wantErr: domain.ErrUnauthenticated},
		{name: "system actor", actor: func(t *testing.T) domain.Actor { return mustActor(t, "rollback-guardian", domain.ActorSystem) }, wantErr: domain.ErrForbidden},
		{name: "administrator", actor: func(t *testing.T) domain.Actor { return githubActor(t, 3) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newApproveHarness(t, harnessOptions{access: newRolePolicy(t, authUsers(t))})
			before := len(h.events(t))
			command := h.command()
			command.Actor = tt.actor(t)
			_, err := h.services.ApproveAndExecute.Run(context.Background(), command)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Run = %v, want %v", err, tt.wantErr)
				}
				if h.status(t) != domain.StatusAwaitingApproval || len(h.events(t)) != before || len(h.executor.calls) != 0 {
					t.Fatalf("denied approval changed state: status %s, events %d->%d, executions %d", h.status(t), before, len(h.events(t)), len(h.executor.calls))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			approved := false
			for _, event := range h.events(t) {
				if event.Kind() == domain.EventApprovalRecorded {
					approved = true
					if event.Actor() != command.Actor {
						t.Fatalf("approval recorded actor %v, want %v", event.Actor(), command.Actor)
					}
				}
			}
			if !approved {
				t.Fatal("no approval event recorded")
			}
		})
	}
}

func TestSystemActorCannotUseHumanUseCases(t *testing.T) {
	h := newApproveHarness(t, harnessOptions{access: newRolePolicy(t, authUsers(t))})
	ctx := context.Background()
	system := mustActor(t, "rollback-guardian", domain.ActorSystem)
	errs := map[string]error{}
	_, errs["create"] = h.services.Incidents.CreateIncidentAsync(ctx, CreateIncidentCommand{Actor: system})
	_, errs["cancel"] = h.services.Incidents.Cancel(ctx, CancelIncidentCommand{IncidentID: h.incident, Actor: system, Reason: "r"})
	_, errs["reject"] = h.services.Approvals.Reject(ctx, RejectionCommand{IncidentID: h.incident, Actor: system, ActionID: h.proposal.ActionID(), ProposalHash: h.proposal.Hash(), Reason: "r"})
	_, errs["read"] = h.services.Queries.GetIncidentSummary(ctx, system, h.incident)
	for name, err := range errs {
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s by system actor = %v, want forbidden", name, err)
		}
	}
}
