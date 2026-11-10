package contract_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const stableAnnotation = "rollback-guardian.io/stable"

// The demo kustomization must emit the namespace before the RBAC that lives
// in it, and must keep pulling in the reviewed RBAC manifest.
func TestDemoKustomizationAppliesNamespaceBeforeRBACAndWorkload(t *testing.T) {
	var root struct {
		Resources []string `json:"resources"`
	}
	decodeSingle(t, filepath.Join("..", "..", "deploy", "kubernetes", "kustomization.yaml"), &root)
	if want := []string{"namespace.yaml", "../rbac", "demo-api.yaml"}; !reflect.DeepEqual(root.Resources, want) {
		t.Fatalf("deploy/kubernetes resources = %v, want %v", root.Resources, want)
	}
	var rbac struct {
		Resources []string `json:"resources"`
	}
	decodeSingle(t, filepath.Join("..", "..", "deploy", "rbac", "kustomization.yaml"), &rbac)
	if want := []string{"mcp.yaml"}; !reflect.DeepEqual(rbac.Resources, want) {
		t.Fatalf("deploy/rbac resources = %v, want %v", rbac.Resources, want)
	}
	var namespace corev1.Namespace
	decodeSingle(t, filepath.Join("..", "..", "deploy", "kubernetes", "namespace.yaml"), &namespace)
	if namespace.Kind != "Namespace" || namespace.Name != "rollback-demo" {
		t.Fatalf("namespace manifest = %s/%s", namespace.Kind, namespace.Name)
	}
}

func TestDemoAPIManifestMatchesRollbackGuardianExpectations(t *testing.T) {
	docs := decodeAll(t, filepath.Join("..", "..", "deploy", "kubernetes", "demo-api.yaml"))
	var deployment appsv1.Deployment
	var service corev1.Service
	for _, raw := range docs {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "Deployment":
			mustUnmarshal(t, raw, &deployment)
		case "Service":
			mustUnmarshal(t, raw, &service)
		default:
			t.Fatalf("unexpected kind %q in demo-api.yaml", meta.Kind)
		}
	}

	if deployment.Namespace != "rollback-demo" || deployment.Name != "demo-api" {
		t.Fatalf("deployment = %s/%s, want rollback-demo/demo-api (the RBAC resourceName)", deployment.Namespace, deployment.Name)
	}
	// One pod: Prometheus scrapes through the NodePort and pods share label sets.
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", deployment.Spec.Replicas)
	}
	// The controller copies Deployment annotations onto every new ReplicaSet and
	// template annotations change the revision digest; the stable mark belongs
	// on one ReplicaSet only (make demo-v1).
	if _, ok := deployment.Annotations[stableAnnotation]; ok {
		t.Fatal("deployment must not carry the stable annotation")
	}
	if _, ok := deployment.Spec.Template.Annotations[stableAnnotation]; ok {
		t.Fatal("pod template must not carry the stable annotation")
	}
	pod := deployment.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("demo-api must not mount a service-account token")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	container := pod.Containers[0]
	// make demo-v1/demo-v2-broken run `kubectl set image ... demo-api=<tag>`.
	if container.Name != "demo-api" || container.Image != "demo-api:v1-healthy" || container.ImagePullPolicy != corev1.PullNever {
		t.Fatalf("container = %s %s %s, want demo-api demo-api:v1-healthy Never", container.Name, container.Image, container.ImagePullPolicy)
	}
	for name, probe := range map[string]*corev1.Probe{"readiness": container.ReadinessProbe, "liveness": container.LivenessProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Port.String() != "http" {
			t.Fatalf("%s probe must GET /healthz on port http", name)
		}
	}
	env := map[string]string{}
	for _, item := range container.Env {
		env[item.Name] = item.Value
	}
	// Metric target labels must equal the default ALLOWED_TARGETS entry
	// local/rollback-demo/demo-api/demo-api; the mode must come from the image.
	wantEnv := map[string]string{"DEMO_CLUSTER": "local", "DEMO_NAMESPACE": "rollback-demo", "DEMO_DEPLOYMENT": "demo-api", "DEMO_SERVICE": "demo-api"}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("container env = %v, want %v", env, wantEnv)
	}
	security := container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		t.Fatal("container must disallow privilege escalation and use a read-only root filesystem")
	}

	if service.Namespace != "rollback-demo" || service.Name != "demo-api" || service.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("service = %s/%s %s, want rollback-demo/demo-api NodePort", service.Namespace, service.Name, service.Spec.Type)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].NodePort != 30080 || service.Spec.Ports[0].TargetPort.String() != "http" {
		t.Fatalf("service ports = %+v, want nodePort 30080 -> http", service.Spec.Ports)
	}
	for key, value := range service.Spec.Selector {
		if deployment.Spec.Template.Labels[key] != value {
			t.Fatalf("service selector %s=%s does not match the pod template", key, value)
		}
	}
}

func decodeAll(t *testing.T, path string) []json.RawMessage {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	})
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	var docs []json.RawMessage
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return docs
			}
			t.Fatal(err)
		}
		if len(raw) > 0 && string(raw) != "null" {
			docs = append(docs, raw)
		}
	}
}

func decodeSingle(t *testing.T, path string, into any) {
	t.Helper()
	docs := decodeAll(t, path)
	if len(docs) != 1 {
		t.Fatalf("%s: documents = %d, want 1", path, len(docs))
	}
	mustUnmarshal(t, docs[0], into)
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}
