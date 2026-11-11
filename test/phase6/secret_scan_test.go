package phase6_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	a2aadapter "github.com/belLena81/rollback-guard/internal/adapters/a2a"
	"github.com/belLena81/rollback-guard/internal/adapters/diagnosis"
	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth"
	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/kubernetesserver"
	"github.com/belLena81/rollback-guard/internal/adapters/mcp/observabilityserver"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/runtime"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	scanClientSecret = "scan-oauth-client-secret-9f3a" //nolint:gosec // G101: a test fixture
	scanStateSecret  = "scan-auth-state-secret-0123456789abcdef0123456789"
)

// TestNoSecretReachesPayloadsAuditOrLogs drives create, investigate (REST ->
// coordinator -> A2A -> Investigator -> MCP), approve, execute and verify
// with real GitHub-login sessions, then scans every sink a secret could leak
// into: persisted operation events, auth audit rows, every A2A and MCP
// request and response body on the wire (messages, tasks, tool results),
// every REST response body, and the slog output of all four processes.
//
// Scope: the orchestrator's own Inspector, Metrics and Executor are the
// in-process fake, so execution and verification produce no MCP traffic
// here; the rollback-mode MCP server needs Postgres and is covered by the
// e2e token scan (scripts/demo/check-no-tokens.sh).
func TestNoSecretReachesPayloadsAuditOrLogs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spans := tracetest.NewInMemoryExporter()
	now := time.Now().UTC()
	target := must(domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api"))

	// MCP servers.
	kubeProcess := newProcess(t, "kubernetes-mcp-readonly", spans)
	kubeHandler := must(kubernetesserver.NewHTTPHandler(&kubernetesBackend{now: now, target: target}, kubernetesserver.ModeReadonly, nil, time.Minute, kubeProcess.logger))
	kubeWire := &syncBuffer{}
	kubeServer := kubeProcess.serve(recordBodies(kubeWire, kubeHandler))
	observabilityProcess := newProcess(t, "observability-mcp", spans)
	observabilityHandler := must(observabilityserver.NewHTTPHandler(metricsBackend{now: now}, nil, time.Minute, observabilityProcess.logger))
	observabilityWire := &syncBuffer{}
	observabilityServer := observabilityProcess.serve(recordBodies(observabilityWire, observabilityHandler))

	// Investigator.
	investigatorProcess := newProcess(t, "investigator", spans)
	ids := &runtime.IDGenerator{}
	readCaller := connectMCP(t, ctx, investigatorProcess.telemetry, kubeServer.URL+"/mcp", protocol.ServerKubernetesReadonly, []string{protocol.ToolGetDeployment, protocol.ToolGetReplicaSets, protocol.ToolListPods, protocol.ToolGetPodEvents, protocol.ToolGetRolloutStatus, protocol.ToolWatchRollout})
	metricsCaller := connectMCP(t, ctx, investigatorProcess.telemetry, observabilityServer.URL+"/mcp", protocol.ServerObservability, []string{protocol.ToolGetHTTPErrorRate, protocol.ToolCompareErrorRateWindows, protocol.ToolGetRequestLatency, protocol.ToolGetPodCPUUsage, protocol.ToolGetPodMemoryUsage, protocol.ToolEvaluateRecovery})
	inspector := must(protocol.NewDeploymentInspector(readCaller))
	metricsAdapter := must(protocol.NewMetricsAdapter(metricsCaller, 5*time.Minute))
	rules := must(diagnosis.NewRuleDiagnoser(ids, diagnosis.RuleDiagnoserConfig{CorrelationWindow: 10 * time.Minute}))
	runner := must(application.NewInvestigationRunner(inspector, metricsAdapter, rules, runtime.Clock{}, ids))
	var a2aHandler http.Handler
	a2aWire := &syncBuffer{}
	investigatorServer := investigatorProcess.serve(recordBodies(a2aWire, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a2aHandler.ServeHTTP(w, r) })))
	a2aHandler = must(a2aadapter.NewServerHandler(runner, nil, investigatorProcess.logger, a2aadapter.ServerConfig{PublicURL: investigatorServer.URL, TaskTimeout: 20 * time.Second}))

	// Orchestrator with AUTH_MODE=github wiring: fake GitHub, login, session
	// middleware, RoleAccessPolicy and AdminService.
	orchestratorProcess := newProcess(t, "orchestrator", spans)
	incidents := memory.NewUnitOfWork()
	auth := memory.NewAuthUnitOfWork()
	access := must(application.NewRoleAccessPolicy(auth))
	infra := &fake.Infrastructure{
		Deployment: domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-bad", PreviousStableRevision: "revision-stable", DesiredReplicas: 1, RolloutAt: now.Add(-time.Minute)},
		Metric:     domain.MetricEvidence{Name: "http_error_rate", Value: 0.35, Threshold: 0.10, Unit: "ratio", ObservedAt: now},
	}
	infra.AfterExecute = func() { infra.Metric.Value = 0.01 }
	policy := must(domain.NewEligibilityPolicy(domain.EligibilityPolicyConfig{MinimumConfidence: 0.8, CorrelationWindow: 10 * time.Minute, AllowedTargets: []domain.DeploymentTarget{target}}))
	system := must(domain.NewActor("rollback-guardian", domain.ActorSystem))
	services := must(application.NewServices(application.Dependencies{UnitOfWork: incidents, Inspector: infra, Metrics: infra, Executor: infra, RollbackPolicy: policy, ApprovalPolicy: domain.ApprovalAuthorizer{}, Access: access, Clock: runtime.Clock{}, IDGenerator: ids},
		application.WorkflowConfig{ProposalTTL: 15 * time.Minute, ApprovalTTL: 10 * time.Minute, CleanupTimeout: time.Second, SystemActor: system}))
	gateway := must(a2aadapter.NewGateway(ctx, investigatorServer.URL, orchestratorProcess.telemetry.HTTPClient(30*time.Second)))
	t.Cleanup(func() { closeOrFail(t, gateway) })
	var reported []error
	coordinator := must(application.NewInvestigationCoordinator(incidents, gateway, services.Investigations, runtime.Clock{}, application.InvestigationCoordinatorConfig{
		WorkerID: "worker-1", TaskTimeout: 20 * time.Second, CancelTimeout: time.Second,
		ReportError: func(_ context.Context, err error) { reported = append(reported, err) },
	}))

	github := githubfake.New("scan-client", scanClientSecret)
	t.Cleanup(github.Close)
	github.AddUser(githubfake.User{ID: 9001, Login: "root", Organizations: []string{"acme"}})
	github.AddUser(githubfake.User{ID: 9002, Login: "ops", Organizations: []string{"acme"}})
	var restHandler http.Handler
	restWire := &syncBuffer{}
	restServer := orchestratorProcess.serve(recordBodies(restWire, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { restHandler.ServeHTTP(w, r) })))
	provider := must(githuboauth.New(githuboauth.Config{
		ClientID: "scan-client", ClientSecret: scanClientSecret, RedirectURL: restServer.URL + httpserver.GitHubCallbackPath,
		BaseURL: github.URL(), APIURL: github.URL(), Timeout: 5 * time.Second,
	}))
	random := &fake.RandomSource{}
	sessions := must(application.NewSessionService(application.SessionDependencies{UnitOfWork: auth, Clock: runtime.Clock{}, IDGenerator: ids, Random: random}, time.Hour))
	login := must(application.NewLoginService(application.LoginDependencies{
		Sessions: sessions, UnitOfWork: auth, Clock: runtime.Clock{}, IDGenerator: ids, Random: random, Provider: provider,
	}, application.LoginPolicy{AllowedOrganization: "acme", BootstrapAdmins: []domain.BootstrapAdmin{{Login: "root", GitHubID: 9001}}, StateKey: []byte(scanStateSecret)}))
	admin := must(application.NewAdminService(application.AdminDependencies{UnitOfWork: auth, Access: access, Clock: runtime.Clock{}, IDGenerator: ids}))
	api := must(httpapi.NewServer(services, orchestratorProcess.logger, httpapi.WithGitHubLogin(login, false), httpapi.WithUserAdmin(admin)))
	middleware := must(httpserver.NewSessionAuthMiddleware(sessions, orchestratorProcess.logger))
	restHandler = api.Routes(nil, time.Second, middleware)

	client := &scanClient{t: t, base: restServer.URL, github: github}
	root := client.signIn("root")
	ops := client.signIn("ops")
	client.expect(root, http.MethodPatch, "/v1/users/github:9002/role", `{"role":"operator"}`, http.StatusOK)
	body := `{"target":{"cluster":"local","namespace":"rollback-demo","deployment":"demo-api","service":"demo-api"},"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.1,"unit":"ratio","startedAt":"` + now.Add(-30*time.Second).Format(time.RFC3339Nano) + `"}}`
	var created struct {
		Incident struct{ ID string } `json:"incident"`
	}
	client.decode(client.expect(ops, http.MethodPost, "/v1/incidents", body, http.StatusAccepted), &created)
	// RoleAccessPolicy only admits known users, so read as the bootstrap admin.
	var reader domain.Actor
	if err := auth.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByGitHubID(9001)
		if err != nil {
			return err
		}
		reader, err = domain.ActorFor(user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	investigated := func() bool {
		summary, err := services.Queries.GetIncidentSummary(context.Background(), reader, domain.IncidentID(created.Incident.ID))
		return err == nil && summary.Incident.Status() != domain.StatusInvestigating
	}
	if err := coordinator.Run(withDeadline(t, ctx, investigated)); err != nil && !strings.Contains(err.Error(), "context canceled") {
		t.Fatal(err)
	}
	if len(reported) != 0 {
		t.Fatalf("coordinator reported %v", reported)
	}
	var pending struct {
		Proposal struct {
			ActionID     string `json:"actionId"`
			ProposalHash string `json:"proposalHash"`
		} `json:"proposal"`
	}
	client.decode(client.expect(ops, http.MethodGet, "/v1/incidents/"+created.Incident.ID, "", http.StatusOK), &pending)
	approval := `{"actionId":"` + pending.Proposal.ActionID + `","proposalHash":"` + pending.Proposal.ProposalHash + `"}`
	client.expect(root, http.MethodPost, "/v1/incidents/"+created.Incident.ID+"/approve", approval, http.StatusOK)
	incidentID := domain.IncidentID(created.Incident.ID)
	summary := must(services.Queries.GetIncidentSummary(ctx, reader, incidentID))
	if summary.Incident.Status() != domain.StatusResolved {
		if _, err := services.Verifications.Verify(ctx, incidentID); err != nil {
			t.Fatalf("verify: %v", err)
		}
		summary = must(services.Queries.GetIncidentSummary(ctx, reader, incidentID))
	}
	if summary.Incident.Status() != domain.StatusResolved {
		t.Fatalf("incident ended %s, want resolved", summary.Incident.Status())
	}
	// Bearer use of a session token, so an Authorization value is exercised.
	client.expect(client.bearer(ops), http.MethodGet, "/v1/incidents/"+created.Incident.ID+"/events", "", http.StatusOK)
	client.expect(root, http.MethodPost, "/v1/auth/logout", "", http.StatusNoContent)

	// The secrets in play, in every encoding a careless log or payload would use.
	accessTokens := github.IssuedTokens()
	if len(accessTokens) < 2 {
		t.Fatalf("fake GitHub issued %d access tokens, want one per login", len(accessTokens))
	}
	secrets := map[string]string{
		"oauth client secret": scanClientSecret,
		"AUTH_STATE_SECRET":   scanStateSecret,
	}
	for i, token := range accessTokens {
		secrets["github access token "+string(rune('a'+i))] = token
	}
	for name, cookie := range map[string]*http.Cookie{"root": root, "ops": ops} {
		addTokenEncodings(t, secrets, name, cookie.Value)
	}
	maps.Copy(secrets, client.authorizationValues())

	events := must(services.Queries.ListEvents(ctx, reader, incidentID))
	if len(events) == 0 {
		t.Fatal("no operation events persisted")
	}
	var audit []domain.AuthAuditEvent
	if err := auth.WithinAuthTransaction(ctx, func(repos ports.AuthRepositories) error {
		var err error
		audit, err = repos.Audit().ListRecent(100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshots := make([]domain.AuthAuditSnapshot, 0, len(audit))
	for _, event := range audit {
		snapshots = append(snapshots, event.Snapshot())
	}
	if len(snapshots) < 2 {
		t.Fatalf("auth audit rows = %d, want role change and logout", len(snapshots))
	}
	sinks := map[string]string{
		"operation events":          eventSnapshots(t, events),
		"auth audit rows":           string(must(json.Marshal(snapshots))),
		"A2A messages and tasks":    a2aWire.String(),
		"MCP kubernetes results":    kubeWire.String(),
		"MCP observability results": observabilityWire.String(),
		"REST bodies":               restWire.String(),
	}
	for _, process := range []*process{orchestratorProcess, investigatorProcess, kubeProcess, observabilityProcess} {
		sinks[process.name+" logs"] = process.logs.String()
	}
	for sink, content := range sinks {
		// The MCP servers log nothing on success; every other sink must
		// have captured something, or the scan would be vacuous.
		if strings.TrimSpace(content) == "" && !strings.HasSuffix(sink, "-mcp logs") && !strings.HasSuffix(sink, "-readonly logs") {
			t.Errorf("sink %q captured nothing", sink)
		}
		for name, secret := range secrets {
			if strings.Contains(content, secret) {
				t.Errorf("%s leaked into %s", name, sink)
			}
		}
		if strings.Contains(content, "Bearer ") {
			t.Errorf("an Authorization header value leaked into %s", sink)
		}
	}
	for _, want := range []struct{ sink, marker string }{
		{"A2A messages and tasks", created.Incident.ID},
		{"MCP kubernetes results", "revision-bad"},
		{"MCP observability results", "http_error_rate"},
	} {
		if !strings.Contains(sinks[want.sink], want.marker) {
			t.Errorf("%s does not contain %q; the capture missed the payloads", want.sink, want.marker)
		}
	}
}

// addTokenEncodings adds the raw session token, its base64 forms, the raw
// bytes in standard base64 and hex, and its stored SHA-256 in hex and
// base64.
func addTokenEncodings(t *testing.T, secrets map[string]string, name, token string) {
	t.Helper()
	if len(token) < 40 {
		t.Fatalf("%s session token is implausibly short", name)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("%s session token is not base64url: %v", name, err)
	}
	digest := sha256.Sum256(raw)
	secrets[name+" session token"] = token
	secrets[name+" session token base64"] = base64.StdEncoding.EncodeToString([]byte(token))
	secrets[name+" session token bytes base64"] = base64.StdEncoding.EncodeToString(raw)
	secrets[name+" session token bytes hex"] = hex.EncodeToString(raw)
	secrets[name+" session hash hex"] = hex.EncodeToString(digest[:])
	secrets[name+" session hash base64"] = base64.StdEncoding.EncodeToString(digest[:])
}

// eventSnapshots is the persisted form of every event, details included.
func eventSnapshots(t *testing.T, events []domain.OperationEvent) string {
	t.Helper()
	snapshots := make([]domain.OperationEventSnapshot, 0, len(events))
	for _, event := range events {
		snapshots = append(snapshots, event.Snapshot())
	}
	encoded, err := json.Marshal(snapshots)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// recordBodies tees every request and response body of next into wire,
// except the OAuth callback's response: api/openapi.yaml returns the new
// session token there once, by design (SessionResponse).
func recordBodies(wire *syncBuffer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == httpserver.GitHubCallbackPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err == nil {
				if _, err := wire.Write(append(body, '\n')); err != nil {
					panic(err)
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(&teeWriter{ResponseWriter: w, wire: wire}, r)
	})
}

type teeWriter struct {
	http.ResponseWriter
	wire *syncBuffer
}

func (w *teeWriter) Write(data []byte) (int, error) {
	if _, err := w.wire.Write(data); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(data) //nolint:wrapcheck // a transparent proxy
}

func (w *teeWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// scanClient talks to the orchestrator over real HTTP and remembers every
// Authorization value it sent.
type scanClient struct {
	t       *testing.T
	base    string
	github  *githubfake.Server
	headers []string
}

// bearerMarker marks a cookie whose token is sent as a bearer header.
const bearerMarker = "bearer"

func (c *scanClient) bearer(cookie *http.Cookie) *http.Cookie {
	return &http.Cookie{Name: bearerMarker, Value: cookie.Value}
}

func (c *scanClient) authorizationValues() map[string]string {
	values := map[string]string{}
	for i, header := range c.headers {
		values["authorization header "+string(rune('a'+i))] = header
	}
	return values
}

func (c *scanClient) noRedirects() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// reply is a fully read and closed response.
type reply struct {
	status  int
	header  http.Header
	cookies []*http.Cookie
	body    []byte
}

func (c *scanClient) send(request *http.Request) reply {
	c.t.Helper()
	response, err := c.noRedirects().Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if closeErr := response.Body.Close(); closeErr != nil || err != nil {
		c.t.Fatalf("read %s: %v %v", request.URL.Path, err, closeErr)
	}
	return reply{status: response.StatusCode, header: response.Header, cookies: response.Cookies(), body: body}
}

// signIn runs the OAuth flow against the fake GitHub over real HTTP and
// returns the session cookie.
func (c *scanClient) signIn(login string) *http.Cookie {
	c.t.Helper()
	begin := c.send(must(http.NewRequestWithContext(c.t.Context(), http.MethodGet, c.base+httpserver.GitHubLoginPath, nil)))
	if begin.status != http.StatusFound {
		c.t.Fatalf("login = %d", begin.status)
	}
	c.github.SignInAs(login)
	authorized := c.send(must(http.NewRequestWithContext(c.t.Context(), http.MethodGet, begin.header.Get("Location"), nil)))
	back := must(url.Parse(authorized.header.Get("Location")))
	callback := must(http.NewRequestWithContext(c.t.Context(), http.MethodGet, c.base+httpserver.GitHubCallbackPath+"?"+back.RawQuery, nil))
	for _, cookie := range begin.cookies {
		callback.AddCookie(cookie)
	}
	done := c.send(callback)
	for _, cookie := range done.cookies {
		if cookie.Name == httpserver.SessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	c.t.Fatalf("callback = %d issued no session", done.status)
	return nil
}

func (c *scanClient) expect(cookie *http.Cookie, method, path, body string, status int) []byte {
	c.t.Helper()
	request := must(http.NewRequestWithContext(c.t.Context(), method, c.base+path, strings.NewReader(body)))
	if cookie.Name == bearerMarker {
		header := "Bearer " + cookie.Value
		c.headers = append(c.headers, header)
		request.Header.Set("Authorization", header)
	} else {
		request.AddCookie(cookie)
		request.Header.Set("Origin", c.base)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := c.send(request)
	if response.status != status {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, response.status, status, response.body)
	}
	return response.body
}

func (c *scanClient) decode(body []byte, into any) {
	c.t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		c.t.Fatalf("decode %s: %v", body, err)
	}
}
