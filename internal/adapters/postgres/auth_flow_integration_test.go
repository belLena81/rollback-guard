//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/application/applicationtest"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

type authFlowStack struct {
	github  *githubfake.Server
	handler http.Handler
	flow    applicationtest.Flow
	auth    *AuthUnitOfWork
	logs    *strings.Builder
}

func newAuthFlowStack(t *testing.T) authFlowStack {
	t.Helper()
	shared := isolatedSchemaPool(t, allUpMigrations...)
	pool := repositoryPool(t, shared, 8)
	incidents, err := NewUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthUnitOfWork(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	clock := testkit.NewClock(now)
	ids := &testkit.IDGenerator{}
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	infrastructure := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{
			Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1",
			DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute),
		},
		Metric: domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
		Now:    clock.Now,
	}
	infrastructure.AfterExecute = func() { infrastructure.Metric.Value = 0.02 }
	rollbackPolicy, err := domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: .8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	systemActor, err := domain.NewActor("rollback-guardian", domain.ActorSystem)
	if err != nil {
		t.Fatal(err)
	}
	access, err := application.NewRoleAccessPolicy(auth)
	if err != nil {
		t.Fatal(err)
	}
	services, err := application.NewServices(application.Dependencies{
		UnitOfWork: incidents, Inspector: infrastructure, Metrics: infrastructure, Executor: infrastructure,
		RollbackPolicy: rollbackPolicy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: access, Clock: clock, IDGenerator: ids,
	}, application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: 5 * time.Second, SystemActor: systemActor})
	if err != nil {
		t.Fatal(err)
	}
	diagnoser, err := diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := application.NewInvestigationRunner(infrastructure, infrastructure, diagnoser, clock, ids)
	if err != nil {
		t.Fatal(err)
	}

	github := githubfake.New("flow-client", "flow-client-secret")
	t.Cleanup(github.Close)
	github.AddUser(githubfake.User{ID: 7001, Login: "root", Organizations: []string{"acme"}})
	github.AddUser(githubfake.User{ID: 7002, Login: "ops", Organizations: []string{"acme"}})
	provider, err := githuboauth.New(githuboauth.Config{
		ClientID: "flow-client", ClientSecret: "flow-client-secret", RedirectURL: "http://127.0.0.1:8080" + httpserver.GitHubCallbackPath,
		BaseURL: github.URL(), APIURL: github.URL(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	random := &fake.RandomSource{}
	sessions, err := application.NewSessionService(application.SessionDependencies{UnitOfWork: auth, Clock: clock, IDGenerator: ids, Random: random}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	login, err := application.NewLoginService(application.LoginDependencies{
		Sessions: sessions, UnitOfWork: auth, Clock: clock, IDGenerator: ids, Random: random, Provider: provider,
	}, application.LoginPolicy{AllowedOrganization: "acme", BootstrapAdmins: []domain.BootstrapAdmin{{Login: "root", GitHubID: 7001}}, StateKey: []byte(strings.Repeat("k", application.MinLoginStateKeyBytes))})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := application.NewAdminService(application.AdminDependencies{UnitOfWork: auth, Access: access, Clock: clock, IDGenerator: ids})
	if err != nil {
		t.Fatal(err)
	}
	logs := &strings.Builder{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server, err := httpapi.NewServer(services, logger, httpapi.WithGitHubLogin(login, false), httpapi.WithUserAdmin(admin))
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := httpserver.NewSessionAuthMiddleware(sessions, logger)
	if err != nil {
		t.Fatal(err)
	}
	return authFlowStack{
		github:  github,
		handler: server.Routes(nil, 0, middleware),
		flow:    applicationtest.Flow{Services: services, UnitOfWork: incidents, Clock: clock, IDs: ids, Runner: runner, SystemActor: systemActor, CleanupTimeout: 5 * time.Second},
		auth:    auth,
		logs:    logs,
	}
}

func (s authFlowStack) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, request)
	return recorder
}

// signIn runs the OAuth flow against the fake GitHub and returns the session
// cookie.
func (s authFlowStack) signIn(t *testing.T, login string) *http.Cookie {
	t.Helper()
	begin := s.serve(httptest.NewRequest(http.MethodGet, httpserver.GitHubLoginPath, nil))
	if begin.Code != http.StatusFound {
		t.Fatalf("login = %d", begin.Code)
	}
	s.github.SignInAs(login)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, begin.Header().Get("Location"), nil)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorized.Body.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := url.Parse(authorized.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback := httptest.NewRequest(http.MethodGet, httpserver.GitHubCallbackPath+"?"+back.RawQuery, nil)
	for _, cookie := range begin.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	done := s.serve(callback)
	for _, cookie := range done.Result().Cookies() {
		if cookie.Name == httpserver.SessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("callback = %d issued no session", done.Code)
	return nil
}

func (s authFlowStack) as(cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.AddCookie(cookie)
	if method != http.MethodGet {
		request.Header.Set("Origin", "http://"+request.Host)
	}
	return s.serve(request)
}

// TestGitHubLoginRoleEnforcementAndAuditOnPostgres: the whole 7.5 chain on
// Postgres. A fake-GitHub operator cannot approve (403); the bootstrap admin
// can; the approval event and the auth audit both name the admin.
func TestGitHubLoginRoleEnforcementAndAuditOnPostgres(t *testing.T) {
	stack := newAuthFlowStack(t)
	root := stack.signIn(t, "root")
	ops := stack.signIn(t, "ops")
	if response := stack.as(root, http.MethodPatch, "/v1/users/github:7002/role", `{"role":"operator"}`); response.Code != http.StatusOK {
		t.Fatalf("promote ops = %d %s", response.Code, response.Body)
	}

	created := stack.as(ops, http.MethodPost, "/v1/incidents", `{
		"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
		"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}}`)
	if created.Code != http.StatusAccepted {
		t.Fatalf("operator create = %d %s", created.Code, created.Body)
	}
	var accepted struct {
		Incident struct{ ID string } `json:"incident"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.flow.Investigate(t.Context(), domain.IncidentID(accepted.Incident.ID)); err != nil {
		t.Fatal(err)
	}
	summary := stack.as(ops, http.MethodGet, "/v1/incidents/"+accepted.Incident.ID, "")
	var pending struct {
		Proposal struct {
			ActionID     string `json:"actionId"`
			ProposalHash string `json:"proposalHash"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(summary.Body.Bytes(), &pending); err != nil || pending.Proposal.ProposalHash == "" {
		t.Fatalf("summary = %d %s", summary.Code, summary.Body)
	}
	approval := `{"actionId":"` + pending.Proposal.ActionID + `","proposalHash":"` + pending.Proposal.ProposalHash + `"}`
	approvePath := "/v1/incidents/" + accepted.Incident.ID + "/approve"
	if response := stack.as(ops, http.MethodPost, approvePath, approval); response.Code != http.StatusForbidden {
		t.Fatalf("operator approve = %d %s", response.Code, response.Body)
	}
	if response := stack.as(root, http.MethodPost, approvePath, approval); response.Code != http.StatusOK {
		t.Fatalf("admin approve = %d %s", response.Code, response.Body)
	}

	events := stack.as(ops, http.MethodGet, "/v1/incidents/"+accepted.Incident.ID+"/events", "").Body.String()
	if !strings.Contains(events, `"kind":"rollback.approved"`) || !strings.Contains(events, `"actor":{"id":"github:7001","kind":"human"}`) {
		t.Fatalf("approval event does not name the admin: %s", events)
	}
	if strings.Count(events, `"kind":"rollback.approved"`) != 1 {
		t.Fatalf("operator attempt left an approval event: %s", events)
	}

	var audit []domain.AuthAuditEvent
	err := stack.auth.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		var err error
		audit, err = repos.Audit().ListRecent(10)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d", len(audit))
	}
	row := audit[0].Snapshot()
	if row.Action != domain.AuthAuditRoleChanged || row.ActorID != "github:7001" || row.FromRole != domain.RoleViewer || row.ToRole != domain.RoleOperator {
		t.Fatalf("audit = %+v", row)
	}
	for _, secret := range []string{root.Value, ops.Value, "flow-client-secret"} {
		if strings.Contains(stack.logs.String(), secret) {
			t.Fatal("a session token or client secret reached the logs")
		}
	}
}
