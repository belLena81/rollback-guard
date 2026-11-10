package contract_test

import (
	"os"
	"slices"
	"strings"
	"testing"

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
			responses := requireObject(t, requireObject(t, operations, method), "responses")
			for _, status := range []string{"400", "500"} {
				if !refersToError(t, components(t, document), responses[status]) {
					t.Errorf("%s %s: response %s does not use the Error schema", method, path, status)
				}
			}
		}
	}
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
