package contract_test

import (
	"os"
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
