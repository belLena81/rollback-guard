package contract_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestMCPRolesAreNamespaceScopedAndLeastPrivilege(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "rbac", "mcp.yaml")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close RBAC manifest: %v", err)
		}
	})

	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	roles := map[string]rbacv1.Role{}
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		var metadata struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Kind != "Role" {
			continue
		}
		var role rbacv1.Role
		if err := json.Unmarshal(raw, &role); err != nil {
			t.Fatal(err)
		}
		if role.Namespace != "rollback-demo" {
			t.Fatalf("role %s namespace = %q", role.Name, role.Namespace)
		}
		roles[role.Name] = role
	}

	assertRules(t, roles["rollback-guardian-kubernetes-readonly"].Rules, []rbacv1.PolicyRule{
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{"demo-api"}, Verbs: []string{"get"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"list"}},
		{APIGroups: []string{""}, Resources: []string{"pods", "events"}, Verbs: []string{"list"}},
	})
	assertRules(t, roles["rollback-guardian-kubernetes-rollback"].Rules, []rbacv1.PolicyRule{
		{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, ResourceNames: []string{"demo-api"}, Verbs: []string{"get", "patch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: []string{"list"}},
	})
}

func assertRules(t *testing.T, got, want []rbacv1.PolicyRule) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RBAC rules = %#v, want %#v", got, want)
	}
}
