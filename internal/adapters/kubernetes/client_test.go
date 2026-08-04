package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestInspectDeploymentUsesOwnedStableReplicaSet(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	foreign := testReplicaSet(deployment, "foreign", "foreign:v1", true, time.Now().Add(-2*time.Minute))
	foreign.OwnerReferences[0].UID = "other"
	client := fake.NewClientset(deployment, current, stable, foreign)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}

	evidence, err := service.InspectDeployment(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent := mustRevision(t, deployment.Spec.Template)
	wantStable := mustRevision(t, stable.Spec.Template)
	if evidence.CurrentRevision != wantCurrent || evidence.PreviousStableRevision != wantStable {
		t.Fatalf("revisions = %q/%q, want %q/%q", evidence.CurrentRevision, evidence.PreviousStableRevision, wantCurrent, wantStable)
	}
	if !evidence.RolloutAt.Equal(current.CreationTimestamp.Time) {
		t.Fatalf("rollout time = %s, want %s", evidence.RolloutAt, current.CreationTimestamp.Time)
	}
}

func TestRollbackPatchReplacesWholeTemplate(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar", Image: "unsafe:v1"})
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	current.Spec.Template = deployment.Spec.Template
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}
	currentRevision := mustRevision(t, deployment.Spec.Template)
	targetRevision := mustRevision(t, stable.Spec.Template)
	now := time.Now()
	proposal, err := domain.NewRollbackProposal("action", "incident", "policy", target, currentRevision, targetRevision, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), now, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.AuthorizeRollback(proposal, decision, approval, domain.DeploymentEvidence{Target: target, CurrentRevision: currentRevision}, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ExecuteApprovedRollback(context.Background(), approved, "key"); err != nil {
		t.Fatal(err)
	}
	got, err := client.AppsV1().Deployments(target.Namespace()).Get(context.Background(), target.Deployment(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spec.Template.Spec.Containers) != 1 || got.Spec.Template.Spec.Containers[0].Image != "healthy:v1" {
		t.Fatalf("patched containers = %#v", got.Spec.Template.Spec.Containers)
	}
	actions := client.Actions()
	var patchBytes []byte
	for _, action := range actions {
		if patch, ok := action.(interface{ GetPatch() []byte }); ok {
			patchBytes = patch.GetPatch()
		}
	}
	if !containsReplaceDirective(patchBytes) {
		t.Fatalf("patch does not replace spec.template: %s", patchBytes)
	}
}

func mustTarget(t *testing.T) domain.DeploymentTarget {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func testDeployment(image string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-api", Namespace: "demo", UID: types.UID("deployment-uid"), ResourceVersion: "7", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "demo-api"}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}}}},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
}

func testReplicaSet(deployment *appsv1.Deployment, name, image string, stable bool, created time.Time) *appsv1.ReplicaSet {
	annotations := map[string]string{}
	if stable {
		annotations[StableRevisionAnnotation] = "true"
	}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deployment.Namespace, UID: types.UID(name), CreationTimestamp: metav1.NewTime(created), Labels: map[string]string{"app": "demo-api"}, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: boolPtr(true)}}},
		Spec:       appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api", appsv1.DefaultDeploymentUniqueLabelKey: name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}}}},
	}
}

func boolPtr(value bool) *bool { return &value }

func mustRevision(t *testing.T, template corev1.PodTemplateSpec) domain.Revision {
	t.Helper()
	revision, err := TemplateRevision(template)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}
