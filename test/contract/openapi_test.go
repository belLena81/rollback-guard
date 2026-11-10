package contract_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/httpapi"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"

	"sigs.k8s.io/yaml"
)

func TestOpenAPIIncludesSanitizedDiagnosisProvenance(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	components := requireObject(t, document, "components")
	schemas := requireObject(t, components, "schemas")
	provenanceValue, exists := schemas["DiagnosisProvenance"]
	if !exists {
		t.Fatal("DiagnosisProvenance schema is missing")
	}
	provenance, ok := provenanceValue.(map[string]any)
	if !ok {
		t.Fatalf("DiagnosisProvenance = %#v", provenanceValue)
	}
	properties := requireObject(t, provenance, "properties")
	for _, field := range []string{"requestedStrategy", "model", "promptVersion", "inputTokens", "outputTokens", "fallbackReason"} {
		if _, exists := properties[field]; !exists {
			t.Fatalf("DiagnosisProvenance field %q is missing", field)
		}
	}
	for _, forbidden := range []string{"prompt", "apiKey", "providerError", "rawResponse"} {
		if _, exists := properties[forbidden]; exists {
			t.Fatalf("DiagnosisProvenance exposes forbidden field %q", forbidden)
		}
	}
}

// The error body is a documented, closed shape, and every /v1 operation
// documents its error responses with it.
func TestOpenAPIDocumentsTheErrorBody(t *testing.T) {
	document := loadOpenAPI(t)
	schemas := requireObject(t, requireObject(t, document, "components"), "schemas")
	errorSchema := requireObject(t, schemas, "Error")
	if errorSchema["additionalProperties"] != false {
		t.Fatal("Error schema must not allow additional properties")
	}
	required, ok := errorSchema["required"].([]any)
	if !ok || len(required) != 2 || !containsAll(required, "error", "message") {
		t.Fatalf("Error.required = %v, want [error message]", required)
	}
	properties := requireObject(t, errorSchema, "properties")
	for _, field := range []string{"error", "message", "requestId"} {
		requireObject(t, properties, field)
	}
	if len(properties) != 3 {
		t.Fatalf("Error properties = %v, want error, message, requestId", properties)
	}
	codes, ok := requireObject(t, properties, "error")["enum"].([]any)
	if !ok || !containsAll(codes, "validation_failed", "invalid_json", "not_found", "conflict", "rollback_outcome_unknown", "internal_error") {
		t.Fatalf("Error.error enum = %v", codes)
	}

	paths := requireObject(t, document, "paths")
	for path, item := range paths {
		if len(path) < 4 || path[:4] != "/v1/" {
			continue
		}
		operations, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %s = %#v", path, item)
		}
		for method := range operations {
			operation := requireObject(t, operations, method)
			responses := requireObject(t, operation, "responses")
			statuses := []string{"500"}
			// 400 is required wherever the client supplies input.
			_, hasParameters := operation["parameters"]
			_, hasBody := operation["requestBody"]
			if hasParameters || hasBody {
				statuses = append(statuses, "400")
			}
			for _, status := range statuses {
				if !refersToError(t, components(t, document), responses[status]) {
					t.Errorf("%s %s: response %s does not use the Error schema", method, path, status)
				}
			}
		}
	}
}

// publicOperations are the only operations that may omit a session: the OAuth
// entry points. /health/* and /metrics are unauthenticated by design.
var publicOperations = map[string]bool{
	"/v1/auth/github/login":    true,
	"/v1/auth/github/callback": true,
}

// sessionRoutes exist only in AUTH_MODE=github: they need a session principal.
var sessionRoutes = map[string]bool{
	"/v1/auth/github/login":              true,
	"/v1/auth/github/callback":           true,
	"/v1/auth/logout":                    true,
	"/v1/me":                             true,
	"/v1/users/{userId}/role":            true,
	"/v1/users/{userId}/sessions:revoke": true,
}

type documentedOperation struct{ method, path string }

func documentedOperations(t *testing.T) []documentedOperation {
	t.Helper()
	var operations []documentedOperation
	for path, item := range requireObject(t, loadOpenAPI(t), "paths") {
		methods, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %s = %#v", path, item)
		}
		for method := range methods {
			if method == "parameters" {
				continue
			}
			operations = append(operations, documentedOperation{method: strings.ToUpper(method), path: path})
		}
	}
	if len(operations) == 0 {
		t.Fatal("OpenAPI documents no operations")
	}
	return operations
}

func (o documentedOperation) request() *http.Request {
	path := strings.NewReplacer("{incidentId}", "incident-1", "{userId}", "github:1").Replace(o.path)
	return httptest.NewRequest(o.method, path, strings.NewReader("{}"))
}

// unrouted reports whether the router's mux answered on its own (no
// pattern matched): its 404/405 are plain text, every handler answers JSON
// or a redirect.
func unrouted(recorder *httptest.ResponseRecorder) bool {
	notRouted := recorder.Code == http.StatusNotFound || recorder.Code == http.StatusMethodNotAllowed
	return notRouted && strings.HasPrefix(recorder.Header().Get("Content-Type"), "text/plain")
}

type stubLogin struct{}

func (stubLogin) Begin(context.Context) (application.LoginStart, error) {
	return application.LoginStart{AuthorizationURL: "https://github.example/login", SealedState: "s", ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (stubLogin) Complete(context.Context, string, string, string) (application.LoginResult, error) {
	return application.LoginResult{}, application.ErrInvalidLoginRequest
}

type stubAdmin struct{}

func (stubAdmin) ChangeRole(context.Context, domain.Actor, domain.ActorID, domain.Role) (domain.User, error) {
	return domain.User{}, &domain.NotFoundError{Resource: "user"}
}

func (stubAdmin) RevokeUserSessions(context.Context, domain.Actor, domain.ActorID) (int, error) {
	return 0, &domain.NotFoundError{Resource: "user"}
}

func (stubAdmin) Logout(context.Context, domain.Principal) error { return nil }

type noSessions struct{}

func (noSessions) Resolve(context.Context, string) (domain.Principal, error) {
	return domain.Principal{}, &domain.UnauthenticatedError{}
}

func contractRouter(t *testing.T, github bool, auth httpserver.AuthMiddleware) http.Handler {
	t.Helper()
	var options []httpapi.Option
	if github {
		options = append(options, httpapi.WithGitHubLogin(stubLogin{}, true), httpapi.WithUserAdmin(stubAdmin{}))
	}
	server, err := httpapi.NewServer(contractServices(t), slog.New(slog.DiscardHandler), options...)
	if err != nil {
		t.Fatal(err)
	}
	return server.Routes(func(context.Context) error { return nil }, time.Second, auth)
}

// TestRouterServesEveryOpenAPIOperation: in AUTH_MODE=github the router
// registers every documented path and method; in local mode exactly the
// session routes are absent.
func TestRouterServesEveryOpenAPIOperation(t *testing.T) {
	github := contractRouter(t, true, nil)
	local := contractRouter(t, false, nil)
	for _, operation := range documentedOperations(t) {
		recorder := httptest.NewRecorder()
		github.ServeHTTP(recorder, operation.request())
		if unrouted(recorder) {
			t.Errorf("github mode: %s %s is documented but not routed (%d)", operation.method, operation.path, recorder.Code)
		}
		recorder = httptest.NewRecorder()
		local.ServeHTTP(recorder, operation.request())
		if sessionRoutes[operation.path] != unrouted(recorder) {
			t.Errorf("local mode: %s %s routed = %v, want %v", operation.method, operation.path, !unrouted(recorder), !sessionRoutes[operation.path])
		}
	}
}

// TestSessionAuthCoversEveryNonPublicOperation: with the real session
// middleware and no session, every /v1 operation but the OAuth entry points
// answers 401, and the public ones never do.
func TestSessionAuthCoversEveryNonPublicOperation(t *testing.T) {
	auth, err := httpserver.NewSessionAuthMiddleware(noSessions{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	router := contractRouter(t, true, auth)
	for _, operation := range documentedOperations(t) {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, operation.request())
		public := publicOperations[operation.path] || !strings.HasPrefix(operation.path, "/v1/")
		if public == (recorder.Code == http.StatusUnauthorized) {
			t.Errorf("%s %s without a session = %d, public = %v", operation.method, operation.path, recorder.Code, public)
		}
	}
}

func TestOpenAPISecuresEveryV1OperationExceptOAuthEntryPoints(t *testing.T) {
	document := loadOpenAPI(t)
	schemes := requireObject(t, components(t, document), "securitySchemes")
	cookie := requireObject(t, schemes, "sessionCookie")
	if cookie["type"] != "apiKey" || cookie["in"] != "cookie" {
		t.Fatalf("sessionCookie = %v, want apiKey in cookie", cookie)
	}
	bearer := requireObject(t, schemes, "bearerAuth")
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("bearerAuth = %v, want http bearer", bearer)
	}
	if _, global := document["security"]; global {
		t.Fatal("security must be declared per operation, not globally")
	}

	for path, item := range requireObject(t, document, "paths") {
		methods, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("path %s = %#v", path, item)
		}
		for method := range methods {
			operation := requireObject(t, methods, method)
			security, declared := operation["security"].([]any)
			switch {
			case strings.HasPrefix(path, "/v1/") && !publicOperations[path]:
				if !declared || !containsSchemes(security, "sessionCookie", "bearerAuth") {
					t.Errorf("%s %s: security = %v, want sessionCookie or bearerAuth", method, path, operation["security"])
				}
				responses := requireObject(t, operation, "responses")
				for _, status := range []string{"401", "403"} {
					if !refersToError(t, components(t, document), responses[status]) {
						t.Errorf("%s %s: response %s does not use the Error schema", method, path, status)
					}
				}
			default:
				// OAuth entry points, /health/* and /metrics: explicitly open.
				if !declared || len(security) != 0 {
					t.Errorf("%s %s: security = %v, want explicit empty list", method, path, operation["security"])
				}
			}
		}
	}
}

func containsSchemes(requirements []any, want ...string) bool {
	for _, name := range want {
		found := false
		for _, requirement := range requirements {
			if object, ok := requirement.(map[string]any); ok {
				if _, exists := object[name]; exists {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// approve documents rollback_outcome_unknown as a 503 with Retry-After that
// tells the caller to retry the same approval.
func TestOpenAPIApproveDocumentsUnknownRollbackOutcome(t *testing.T) {
	document := loadOpenAPI(t)
	approve := requireObject(t, requireObject(t, requireObject(t, document, "paths"), "/v1/incidents/{incidentId}/approve"), "post")
	responses := requireObject(t, approve, "responses")
	response, ok := responses["503"]
	if !ok {
		t.Fatal("approve has no 503 response")
	}
	resolved := resolveResponse(t, components(t, document), response)
	if !refersToError(t, components(t, document), response) {
		t.Fatal("approve 503 does not use the Error schema")
	}
	headers := requireObject(t, resolved, "headers")
	requireObject(t, headers, "Retry-After")
	description, ok := resolved["description"].(string)
	if !ok {
		t.Fatal("approve 503 has no description")
	}
	for _, phrase := range []string{"rollback_outcome_unknown", "same approval"} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("approve 503 description %q does not mention %q", description, phrase)
		}
	}
	for _, status := range []string{"403", "404", "409"} {
		if !refersToError(t, components(t, document), responses[status]) {
			t.Errorf("approve response %s does not use the Error schema", status)
		}
	}
}

func loadOpenAPI(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	return document
}

func components(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	return requireObject(t, document, "components")
}

// resolveResponse follows one #/components/responses/<name> reference.
func resolveResponse(t *testing.T, components map[string]any, response any) map[string]any {
	t.Helper()
	object, ok := response.(map[string]any)
	if !ok {
		t.Fatalf("response = %#v", response)
	}
	if ref, ok := object["$ref"].(string); ok {
		const prefix = "#/components/responses/"
		if !strings.HasPrefix(ref, prefix) {
			t.Fatalf("unexpected response reference %q", ref)
		}
		return requireObject(t, requireObject(t, components, "responses"), strings.TrimPrefix(ref, prefix))
	}
	return object
}

func refersToError(t *testing.T, components map[string]any, response any) bool {
	t.Helper()
	if response == nil {
		return false
	}
	resolved := resolveResponse(t, components, response)
	content, ok := resolved["content"].(map[string]any)
	if !ok {
		return false
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		return false
	}
	schema, ok := media["schema"].(map[string]any)
	return ok && schema["$ref"] == "#/components/schemas/Error"
}

func containsAll(values []any, want ...string) bool {
	for _, item := range want {
		if !slices.Contains(values, any(item)) {
			return false
		}
	}
	return true
}

func requireObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, exists := object[key]
	if !exists {
		t.Fatalf("object field %q is missing", key)
	}
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("object field %q = %#v", key, value)
	}
	return result
}
