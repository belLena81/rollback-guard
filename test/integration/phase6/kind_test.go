//go:build kind

// Package phase6 checks the live local kind deployment (make deploy demo-v1)
// through the same Kubernetes adapter and service-account credentials the
// host-run Kubernetes MCP processes use. Run with `make phase6-kind-check`.
package phase6

import (
	"context"
	"os"
	"testing"
	"time"

	kubernetesadapter "github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	"github.com/belLena81/rollback-guard/internal/domain"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	namespace  = "rollback-demo"
	deployment = "demo-api"
)

func TestReadonlyAdapterSeesExactlyOneStableReplicaSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := clientFor(t, "KIND_READONLY_KUBECONFIG")
	target, err := domain.NewDeploymentTarget("local", namespace, deployment, deployment)
	if err != nil {
		t.Fatal(err)
	}
	service, err := kubernetesadapter.NewService(client, kubernetesadapter.Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}

	evidence, err := service.InspectDeployment(ctx, target)
	if err != nil {
		t.Fatalf("inspect deployment as readonly SA: %v", err)
	}
	if evidence.PreviousStableRevision == "" {
		t.Fatal("no stable revision; run `make demo-v1` to mark the verified ReplicaSet")
	}
	replicaSets, err := service.ReplicaSets(ctx, target)
	if err != nil {
		t.Fatalf("list replica sets as readonly SA: %v", err)
	}
	stable := 0
	for _, replicaSet := range replicaSets {
		if replicaSet.Stable {
			stable++
			if replicaSet.Revision != evidence.PreviousStableRevision {
				t.Fatalf("stable replica set %s revision %s, inspect reported %s", replicaSet.Name, replicaSet.Revision, evidence.PreviousStableRevision)
			}
		}
	}
	if stable != 1 {
		t.Fatalf("stable replica sets = %d, want exactly 1", stable)
	}

	// Digest parity: the Deployment template and the stable ReplicaSet template
	// must hash identically when v1 is live, or a rollback could never be
	// verified as reaching its target revision.
	live, err := client.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	onStable := live.Spec.Template.Spec.Containers[0].Image == "demo-api:v1-healthy"
	if onStable != (evidence.CurrentRevision == evidence.PreviousStableRevision) {
		t.Fatalf("image %s: current revision %s, stable revision %s", live.Spec.Template.Spec.Containers[0].Image, evidence.CurrentRevision, evidence.PreviousStableRevision)
	}
}

func TestServiceAccountsAreLeastPrivilegeAgainstLiveAPIServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	readonly := clientFor(t, "KIND_READONLY_KUBECONFIG")
	rollback := clientFor(t, "KIND_ROLLBACK_KUBECONFIG")
	dryRun := metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	noop := []byte(`{}`)

	if _, err := readonly.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{}); err != nil {
		t.Fatalf("readonly get deployment: %v", err)
	}
	_, err := readonly.AppsV1().Deployments(namespace).Patch(ctx, deployment, types.StrategicMergePatchType, noop, dryRun)
	assertForbidden(t, "readonly patch deployment", err)
	_, err = readonly.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	assertForbidden(t, "readonly list secrets", err)

	if _, err := rollback.AppsV1().Deployments(namespace).Patch(ctx, deployment, types.StrategicMergePatchType, noop, dryRun); err != nil {
		t.Fatalf("rollback dry-run patch demo-api: %v", err)
	}
	_, err = rollback.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	assertForbidden(t, "rollback list deployments", err)
	_, err = rollback.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	assertForbidden(t, "rollback list secrets", err)
	_, err = rollback.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	assertForbidden(t, "rollback list pods", err)
	if err := rollback.AppsV1().Deployments(namespace).Delete(ctx, deployment, metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("rollback dry-run delete demo-api: err = %v, want Forbidden", err)
	}
}

func clientFor(t *testing.T, env string) kubernetes.Interface {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Fatalf("%s must point at a service-account kubeconfig (make kubeconfigs)", env)
	}
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("load %s: %v", env, err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertForbidden(t *testing.T, action string, err error) {
	t.Helper()
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("%s: err = %v, want Forbidden", action, err)
	}
}
