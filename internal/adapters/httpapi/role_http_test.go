package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/fake"
	"github.com/belLena81/rollback-guard/internal/adapters/memory"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/belLena81/rollback-guard/internal/testkit"
)

// GitHub IDs of the seeded users; the actor ID is github:<id>.
const (
	viewerID   int64 = 11
	operatorID int64 = 12
	adminID    int64 = 13
	targetID   int64 = 20
)

// roleHarness serves the full router in AUTH_MODE=github shape: real session
// middleware, RoleAccessPolicy services and the AdminService, all over the
// memory auth store.
type roleHarness struct {
	server   *testServer
	store    *memory.AuthUnitOfWork
	sessions *application.SessionService
	clock    *testkit.Clock
	handler  http.Handler
	logs     *bytes.Buffer
	tokens   map[int64]string
}

func newRoleHarness(t *testing.T) *roleHarness {
	t.Helper()
	h := &roleHarness{
		store:  memory.NewAuthUnitOfWork(),
		clock:  testkit.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)),
		logs:   &bytes.Buffer{},
		tokens: map[int64]string{},
	}
	roles := map[int64]domain.Role{viewerID: domain.RoleViewer, operatorID: domain.RoleOperator, adminID: domain.RoleAdministrator, targetID: domain.RoleViewer}
	users := map[int64]domain.User{}
	err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		for id, role := range roles {
			user, err := domain.NewUser(domain.UserID(fmt.Sprintf("user-%d", id)), id, fmt.Sprintf("login-%d", id), "", nil, role, h.clock.Now())
			if err != nil {
				return err
			}
			users[id] = user
			if err := repos.Users().Create(user); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := &testkit.IDGenerator{}
	h.sessions, err = application.NewSessionService(application.SessionDependencies{UnitOfWork: h.store, Clock: h.clock, IDGenerator: ids, Random: &fake.RandomSource{}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for id, user := range users {
		token, _, err := h.sessions.Issue(context.Background(), user)
		if err != nil {
			t.Fatal(err)
		}
		h.tokens[id] = token
	}
	access, err := application.NewRoleAccessPolicy(h.store)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := application.NewAdminService(application.AdminDependencies{UnitOfWork: h.store, Access: access, Clock: h.clock, IDGenerator: ids})
	if err != nil {
		t.Fatal(err)
	}
	h.server = newTestServerWithAccess(t, nil, access)
	logger := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server, err := NewServer(*h.server.services, logger, WithUserAdmin(admin))
	if err != nil {
		t.Fatal(err)
	}
	middleware, err := httpserver.NewSessionAuthMiddleware(h.sessions, logger)
	if err != nil {
		t.Fatal(err)
	}
	h.handler = server.Routes(nil, 0, middleware)
	return h
}

func (h *roleHarness) do(t *testing.T, githubID int64, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token, ok := h.tokens[githubID]; ok {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	return recorder
}

const createIncidentBody = `{
	"target":{"cluster":"local","namespace":"demo","deployment":"demo-api","service":"demo-api"},
	"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-03T10:00:00Z"}
}`

// pending creates an incident as the operator and investigates it in
// process, so it waits for approval with a proposal.
func (h *roleHarness) pending(t *testing.T) incidentSummaryDTO {
	t.Helper()
	response := h.do(t, operatorID, http.MethodPost, "/v1/incidents", createIncidentBody)
	if response.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", response.Code, response.Body)
	}
	var accepted incidentSummaryDTO
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.flow.Investigate(context.Background(), domain.IncidentID(accepted.Incident.ID)); err != nil {
		t.Fatal(err)
	}
	response = h.do(t, viewerID, http.MethodGet, "/v1/incidents/"+accepted.Incident.ID, "")
	var summary incidentSummaryDTO
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil || summary.Proposal == nil {
		t.Fatalf("summary = %d %s", response.Code, response.Body)
	}
	return summary
}

func mustJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return body
}

func decisionBody(summary incidentSummaryDTO, reason string) string {
	body := mustJSON(map[string]string{"actionId": summary.Proposal.ActionID, "proposalHash": summary.Proposal.ProposalHash, "reason": reason})
	if reason == "" {
		body = mustJSON(approvalRequest{ActionID: summary.Proposal.ActionID, ProposalHash: summary.Proposal.ProposalHash})
	}
	return string(body)
}

type roleRequest func(t *testing.T, h *roleHarness) (method, path, body string)

var roleRequests = map[string]roleRequest{
	"create": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodPost, "/v1/incidents", createIncidentBody
	},
	"read": func(t *testing.T, h *roleHarness) (string, string, string) {
		return http.MethodGet, "/v1/incidents/" + h.pending(t).Incident.ID + "/events", ""
	},
	"approve": func(t *testing.T, h *roleHarness) (string, string, string) {
		summary := h.pending(t)
		return http.MethodPost, "/v1/incidents/" + summary.Incident.ID + "/approve", decisionBody(summary, "")
	},
	"reject": func(t *testing.T, h *roleHarness) (string, string, string) {
		summary := h.pending(t)
		return http.MethodPost, "/v1/incidents/" + summary.Incident.ID + "/reject", decisionBody(summary, "not now")
	},
	"cancel": func(t *testing.T, h *roleHarness) (string, string, string) {
		return http.MethodPost, "/v1/incidents/" + h.pending(t).Incident.ID + "/cancel", `{"reason":"maintenance"}`
	},
	"me": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodGet, "/v1/me", ""
	},
	"role": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodPatch, "/v1/users/github:20/role", `{"role":"operator"}`
	},
	"revoke": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodPost, "/v1/users/github:20/sessions:revoke", ""
	},
	"disable": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodPost, "/v1/users/github:20/disable", ""
	},
	"enable": func(*testing.T, *roleHarness) (string, string, string) {
		return http.MethodPost, "/v1/users/github:20/enable", ""
	},
}

func TestHTTPRoleMatrix(t *testing.T) {
	const ok, created, forbidden = http.StatusOK, http.StatusAccepted, http.StatusForbidden
	want := map[string]map[int64]int{
		"create":  {viewerID: forbidden, operatorID: created, adminID: created},
		"read":    {viewerID: ok, operatorID: ok, adminID: ok},
		"approve": {viewerID: forbidden, operatorID: forbidden, adminID: ok},
		"reject":  {viewerID: forbidden, operatorID: ok, adminID: ok},
		"cancel":  {viewerID: forbidden, operatorID: ok, adminID: ok},
		"me":      {viewerID: ok, operatorID: ok, adminID: ok},
		"role":    {viewerID: forbidden, operatorID: forbidden, adminID: ok},
		"revoke":  {viewerID: forbidden, operatorID: forbidden, adminID: ok},
		"disable": {viewerID: forbidden, operatorID: forbidden, adminID: ok},
		"enable":  {viewerID: forbidden, operatorID: forbidden, adminID: ok},
	}
	for name, byRole := range want {
		for githubID, status := range byRole {
			t.Run(fmt.Sprintf("%s/github:%d", name, githubID), func(t *testing.T) {
				h := newRoleHarness(t)
				method, path, body := roleRequests[name](t, h)
				response := h.do(t, githubID, method, path, body)
				if response.Code != status {
					t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, status, response.Body)
				}
				if status == forbidden {
					assertErrorBody(t, response, forbidden, "forbidden")
				}
			})
		}
	}
}

func TestHTTPNoSessionIsUnauthenticated(t *testing.T) {
	h := newRoleHarness(t)
	for name, request := range roleRequests {
		method, path, body := request(t, h)
		if response := h.do(t, 0, method, path, body); response.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d", name, response.Code)
		}
	}
}

// TestHTTPIncidentEventsRecordTheSessionActor: every human decision event
// carries the authenticated github:<id>, never a header-supplied actor.
func TestHTTPIncidentEventsRecordTheSessionActor(t *testing.T) {
	h := newRoleHarness(t)
	rejected := h.pending(t)
	if response := h.do(t, operatorID, http.MethodPost, "/v1/incidents/"+rejected.Incident.ID+"/reject", decisionBody(rejected, "not now")); response.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", response.Code, response.Body)
	}
	canceled := h.pending(t)
	if response := h.do(t, adminID, http.MethodPost, "/v1/incidents/"+canceled.Incident.ID+"/cancel", `{"reason":"maintenance"}`); response.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", response.Code, response.Body)
	}
	// Approve last: executing the fake rollback changes its revisions.
	approved := h.pending(t)
	if response := h.do(t, adminID, http.MethodPost, "/v1/incidents/"+approved.Incident.ID+"/approve", decisionBody(approved, "")); response.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", response.Code, response.Body)
	}
	cases := []struct {
		incident string
		kind     string
		actor    string
	}{
		{approved.Incident.ID, "incident.created", "github:12"},
		{approved.Incident.ID, "rollback.approved", "github:13"},
		{rejected.Incident.ID, "rollback.rejected", "github:12"},
		{canceled.Incident.ID, "incident.canceled", "github:13"},
	}
	for _, tc := range cases {
		response := h.do(t, viewerID, http.MethodGet, "/v1/incidents/"+tc.incident+"/events", "")
		var listed struct {
			Events []eventDTO `json:"events"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range listed.Events {
			if event.Kind != tc.kind {
				continue
			}
			found = true
			if event.Actor.ID != tc.actor || event.Actor.Kind != "human" {
				t.Errorf("%s actor = %+v, want %s", tc.kind, event.Actor, tc.actor)
			}
		}
		if !found {
			t.Errorf("no %s event in %s", tc.kind, response.Body)
		}
	}
	// A spoofed actor header is ignored: the session decides.
	request := httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(createIncidentBody))
	request.Header.Set("Authorization", "Bearer "+h.tokens[viewerID])
	request.Header.Set("X-Actor", "github:13")
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("viewer with spoofed header create = %d", recorder.Code)
	}
}

func (h *roleHarness) updateUser(t *testing.T, githubID int64, change func(domain.User) (domain.User, error)) {
	t.Helper()
	err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		user, err := repos.Users().GetByGitHubID(githubID)
		if err != nil {
			return err
		}
		changed, err := change(user)
		if err != nil {
			return err
		}
		return repos.Users().Update(changed, user.Version())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSessionRevocationAndDisableTakeEffectOnNextRequest(t *testing.T) {
	h := newRoleHarness(t)
	if response := h.do(t, adminID, http.MethodPost, "/v1/users/github:12/sessions:revoke", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revoked":1`) {
		t.Fatalf("revoke = %d %s", response.Code, response.Body)
	}
	if response := h.do(t, operatorID, http.MethodGet, "/v1/me", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session = %d", response.Code)
	}
	h.updateUser(t, viewerID, func(user domain.User) (domain.User, error) { return user.Disable(h.clock.Now()) })
	if response := h.do(t, viewerID, http.MethodGet, "/v1/me", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user = %d", response.Code)
	}
}

// TestHTTPDisableEndpointTakesEffectOnNextRequest drives disable and enable
// through the real router, session middleware and AdminService: the
// disabled user's next request is 401, their sessions stay revoked after
// enable, and the audit rows and logs hold no token.
func TestHTTPDisableEndpointTakesEffectOnNextRequest(t *testing.T) {
	h := newRoleHarness(t)
	if response := h.do(t, operatorID, http.MethodGet, "/v1/me", ""); response.Code != http.StatusOK {
		t.Fatalf("operator before disable = %d", response.Code)
	}
	response := h.do(t, adminID, http.MethodPost, "/v1/users/github:12/disable", "")
	if response.Code != http.StatusOK || response.Body.String() != `{"actorId":"github:12","login":"login-12","role":"operator","disabled":true}`+"\n" {
		t.Fatalf("disable = %d %q", response.Code, response.Body)
	}
	for _, request := range [][2]string{{http.MethodGet, "/v1/me"}, {http.MethodPost, "/v1/incidents"}} {
		response := h.do(t, operatorID, request[0], request[1], createIncidentBody)
		assertErrorBody(t, response, http.StatusUnauthorized, "unauthenticated")
	}
	// Already disabled: 200, unchanged, no second audit row.
	if response := h.do(t, adminID, http.MethodPost, "/v1/users/github:12/disable", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"disabled":true`) {
		t.Fatalf("second disable = %d %s", response.Code, response.Body)
	}
	response = h.do(t, adminID, http.MethodPost, "/v1/users/github:12/enable", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"disabled":false`) {
		t.Fatalf("enable = %d %s", response.Code, response.Body)
	}
	// Enabling does not resurrect the revoked session: the user logs in again.
	assertErrorBody(t, h.do(t, operatorID, http.MethodGet, "/v1/me", ""), http.StatusUnauthorized, "unauthenticated")
	var actions []string
	err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		events, err := repos.Audit().ListRecent(10)
		for _, e := range events {
			actions = append(actions, e.Action().String())
		}
		return err
	})
	if err != nil || strings.Join(actions, ",") != "user_enabled,user_disabled" {
		t.Fatalf("audit actions = %v, %v", actions, err)
	}
	h.assertAuditAndLogsClean(t)
}

func TestHTTPDemotionTakesEffectMidFlight(t *testing.T) {
	h := newRoleHarness(t)
	summary := h.pending(t)
	h.updateUser(t, adminID, func(user domain.User) (domain.User, error) {
		return user.WithRole(domain.RoleOperator, h.clock.Now())
	})
	response := h.do(t, adminID, http.MethodPost, "/v1/incidents/"+summary.Incident.ID+"/approve", decisionBody(summary, ""))
	assertErrorBody(t, response, http.StatusForbidden, "forbidden")
	response = h.do(t, adminID, http.MethodGet, "/v1/me", "")
	if !strings.Contains(response.Body.String(), `"role":"operator"`) {
		t.Fatalf("me after demotion = %s", response.Body)
	}
}

func TestHTTPMeReturnsLiveIdentity(t *testing.T) {
	h := newRoleHarness(t)
	response := h.do(t, operatorID, http.MethodGet, "/v1/me", "")
	var me meDTO
	if err := json.Unmarshal(response.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me != (meDTO{ActorID: "github:12", Login: "login-12", Role: "operator"}) || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("me = %+v headers = %v", me, response.Header())
	}
}

func TestHTTPLogoutRevokesSessionAndClearsCookie(t *testing.T) {
	h := newRoleHarness(t)
	response := h.do(t, operatorID, http.MethodPost, "/v1/auth/logout", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", response.Code, response.Body)
	}
	cookie := cookieNamed(response, httpserver.SessionCookieName)
	if cookie == nil || cookie.MaxAge >= 0 || cookie.Value != "" {
		t.Fatalf("session cookie not cleared: %+v", cookie)
	}
	if response := h.do(t, operatorID, http.MethodGet, "/v1/me", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("after logout = %d", response.Code)
	}
	h.assertAuditAndLogsClean(t)
}

func TestHTTPUserAdministrationResponses(t *testing.T) {
	h := newRoleHarness(t)
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{"unknown field", http.MethodPatch, "/v1/users/github:20/role", `{"role":"operator","admin":true}`, http.StatusBadRequest, "invalid_json"},
		{"trailing data", http.MethodPatch, "/v1/users/github:20/role", `{"role":"operator"}{}`, http.StatusBadRequest, "invalid_json"},
		{"oversized body", http.MethodPatch, "/v1/users/github:20/role", `{"role":"` + strings.Repeat("a", maxRoleBodyBytes) + `"}`, http.StatusBadRequest, "invalid_json"},
		{"unknown role", http.MethodPatch, "/v1/users/github:20/role", `{"role":"root"}`, http.StatusBadRequest, "validation_failed"},
		{"malformed user", http.MethodPatch, "/v1/users/user-20/role", `{"role":"operator"}`, http.StatusBadRequest, ""},
		{"unknown user", http.MethodPatch, "/v1/users/github:99/role", `{"role":"operator"}`, http.StatusNotFound, "not_found"},
		{"last admin self-demotion", http.MethodPatch, "/v1/users/github:13/role", `{"role":"viewer"}`, http.StatusConflict, "conflict"},
		{"unknown user revoke", http.MethodPost, "/v1/users/github:99/sessions:revoke", "", http.StatusNotFound, "not_found"},
		{"unknown user disable", http.MethodPost, "/v1/users/github:99/disable", "", http.StatusNotFound, "not_found"},
		{"unknown user enable", http.MethodPost, "/v1/users/github:99/enable", "", http.StatusNotFound, "not_found"},
		{"malformed user disable", http.MethodPost, "/v1/users/user-20/disable", "", http.StatusBadRequest, "validation_failed"},
		{"last admin self-disable", http.MethodPost, "/v1/users/github:13/disable", "", http.StatusConflict, "conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.do(t, adminID, tt.method, tt.path, tt.body)
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tt.status, response.Body)
			}
			if tt.code != "" {
				assertErrorBody(t, response, tt.status, tt.code)
			}
		})
	}
	response := h.do(t, adminID, http.MethodPatch, "/v1/users/github:20/role", `{"role":"operator"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"actorId":"github:20","login":"login-20","role":"operator"`) {
		t.Fatalf("promote = %d %s", response.Code, response.Body)
	}
	if response := h.do(t, adminID, http.MethodGet, "/v1/me", ""); !strings.Contains(response.Body.String(), `"role":"administrator"`) {
		t.Fatalf("admin role changed after refused self-demotion: %s", response.Body)
	}
	h.assertAuditAndLogsClean(t)
}

// assertAuditAndLogsClean: no session token, token hash or bearer value
// reaches the audit rows or the logs.
func (h *roleHarness) assertAuditAndLogsClean(t *testing.T) {
	t.Helper()
	var audit []domain.AuthAuditEvent
	err := h.store.WithinAuthTransaction(context.Background(), func(repos ports.AuthRepositories) error {
		var err error
		audit, err = repos.Audit().ListRecent(100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) == 0 {
		t.Fatal("no audit rows")
	}
	rows, err := json.Marshal(auditSnapshots(audit))
	if err != nil {
		t.Fatal(err)
	}
	for id, token := range h.tokens {
		for _, sink := range []string{string(rows), h.logs.String()} {
			if strings.Contains(sink, token) {
				t.Fatalf("token of github:%d leaked", id)
			}
		}
	}
	if strings.Contains(h.logs.String(), "Bearer") {
		t.Fatal("authorization header leaked into logs")
	}
}

func auditSnapshots(events []domain.AuthAuditEvent) []domain.AuthAuditSnapshot {
	snapshots := make([]domain.AuthAuditSnapshot, 0, len(events))
	for _, event := range events {
		snapshots = append(snapshots, event.Snapshot())
	}
	return snapshots
}
