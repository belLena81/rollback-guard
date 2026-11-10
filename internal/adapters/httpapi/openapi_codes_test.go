package httpapi

import (
	"os"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every error code the handlers can send is the OpenAPI Error.error enum,
// exactly: a new code cannot ship undocumented, and the contract cannot
// promise a code that no handler sends.
func TestErrorCodesMatchOpenAPIErrorSchema(t *testing.T) {
	data, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Error struct {
							Enum []string `json:"enum"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"Error"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	documented := slices.Sorted(slices.Values(document.Components.Schemas.Error.Properties.Error.Enum))
	sent := slices.Sorted(slices.Values(errorCodes))
	if !slices.Equal(documented, sent) {
		t.Fatalf("OpenAPI Error.error enum = %v, handlers send %v", documented, sent)
	}
}
