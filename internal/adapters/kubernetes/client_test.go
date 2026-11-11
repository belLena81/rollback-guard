package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPodEventsFindsOwnedPodBeyondListLimit(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("healthy:v1")
	replicaSet := testReplicaSet(deployment, "current", "healthy:v1", false, time.Now())
	objects := []runtime.Object{deployment, replicaSet}
	for index := range 101 {
		objects = append(objects, testPod(replicaSet, fmt.Sprintf("pod-%03d", index), types.UID(fmt.Sprintf("pod-%03d-uid", index))))
	}
	wanted := testPod(replicaSet, "pod-zzz", "pod-zzz-uid")
	event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "pod-zzz-event", Namespace: target.Namespace()}, InvolvedObject: corev1.ObjectReference{UID: wanted.UID}, Reason: "Ready", Type: corev1.EventTypeNormal, Message: "pod became ready", Count: 1, LastTimestamp: metav1.NewTime(time.Now())}
	objects = append(objects, wanted, event)
	client := fake.NewClientset(objects...)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}

	events, truncated, err := service.PodEvents(context.Background(), target, wanted.Name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(events) != 1 || events[0].Reason != "Ready" {
		t.Fatalf("PodEvents() = %#v, truncated %t", events, truncated)
	}
	var podSelector fields.Selector
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			listAction, ok := action.(k8stesting.ListAction)
			if !ok {
				t.Fatalf("pod list action type = %T", action)
			}
			podSelector = listAction.GetListRestrictions().Fields
		}
	}
	if podSelector == nil || podSelector.String() != "metadata.name=pod-zzz" {
		t.Fatalf("pod field selector = %v", podSelector)
	}
}

func TestPodEventsRejectsMissingAndForeignPodsBeforeListingEvents(t *testing.T) {
	target := mustTarget(t)
	for _, test := range []struct {
		name       string
		includePod bool
	}{
		{name: "missing"},
		{name: "foreign", includePod: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			deployment := testDeployment("healthy:v1")
			replicaSet := testReplicaSet(deployment, "current", "healthy:v1", false, time.Now())
			objects := []runtime.Object{deployment, replicaSet}
			if test.includePod {
				foreign := testPod(replicaSet, "requested-pod", "foreign-pod-uid")
				foreign.OwnerReferences[0].UID = "foreign-replica-set"
				objects = append(objects, foreign)
			}
			client := fake.NewClientset(objects...)
			service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
			if err != nil {
				t.Fatal(err)
			}

			_, _, err = service.PodEvents(context.Background(), target, "requested-pod", 10)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("PodEvents() error = %v", err)
			}
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "events" {
					t.Fatalf("events accessed for %s pod: %#v", test.name, action)
				}
			}
		})
	}
}

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

func TestGetRolloutStatusWaitsForOldReplicasToTerminate(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("healthy:v1")
	deployment.Status.Replicas = 2
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.ReadyReplicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UnavailableReplicas = 0
	client := fake.NewClientset(deployment)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}})
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.GetRolloutStatus(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if status.Complete {
		t.Fatalf("rollout complete with total replicas %d and desired replicas %d", deployment.Status.Replicas, status.DesiredReplicas)
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
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ExecuteApprovedRollback(context.Background(), proposal.Hash(), "key"); err != nil {
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

func TestPrepareRollbackAllowsExpiredApprovedProposalPreflight(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	currentRevision := mustRevision(t, deployment.Spec.Template)
	targetRevision := mustRevision(t, stable.Spec.Template)
	startedAt := time.Now().UTC()
	proposal, err := domain.NewRollbackProposal("action", "incident", "policy", target, currentRevision, targetRevision, startedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(client, Config{
		ClusterID:      "local",
		AllowedTargets: []domain.DeploymentTarget{target},
		Now:            func() time.Time { return startedAt.Add(2 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := service.PrepareRollback(context.Background(), proposal); err != nil {
		t.Fatalf("PrepareRollback() error = %v", err)
	}
}

func TestRollbackIdempotencySurvivesServiceReplacement(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	guard := newSharedMutationGuard(approved)
	first, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: guard})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: guard})
	if err != nil {
		t.Fatal(err)
	}

	firstResult, err := first.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	patches := patchActionCount(client.Actions())
	secondResult, err := second.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "shared-key")
	if err != nil {
		t.Fatal(err)
	}
	if firstResult != secondResult {
		t.Fatalf("replayed result = %#v, want %#v", secondResult, firstResult)
	}
	if got := patchActionCount(client.Actions()); got != patches || got != 1 {
		t.Fatalf("patch actions after replay = %d, want 1", got)
	}

	if _, err := second.ExecuteApprovedRollback(context.Background(), "different-hash", "shared-key"); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	if got := patchActionCount(client.Actions()); got != 1 {
		t.Fatalf("patch actions after conflict = %d, want 1", got)
	}
}

func TestExecuteApprovedRollbackUsesLedgerSafeCompletionTimeAfterPatch(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	startedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	currentRevision := mustRevision(t, deployment.Spec.Template)
	targetRevision := mustRevision(t, stable.Spec.Template)
	proposal, err := domain.NewRollbackProposal("action", "incident", "policy", target, currentRevision, targetRevision, startedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), startedAt, startedAt.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.AuthorizeRollback(proposal, decision, approval, domain.DeploymentEvidence{Target: target, CurrentRevision: currentRevision}, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	times := []time.Time{
		startedAt,
		startedAt,
		startedAt.Add(-time.Second),
	}
	now := func() time.Time {
		if len(times) == 0 {
			return startedAt
		}
		next := times[0]
		times = times[1:]
		return next
	}
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved), Now: now})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.ExecuteApprovedRollback(context.Background(), proposal.Hash(), "clock-skew-key")
	if err != nil {
		t.Fatal(err)
	}
	if result.CompletedAt != result.StartedAt {
		t.Fatalf("completed at = %s, want started at %s", result.CompletedAt, result.StartedAt)
	}
	if result.AlreadyApplied {
		t.Fatalf("already applied = true, want false after patch")
	}
	if got := patchActionCount(client.Actions()); got != 1 {
		t.Fatalf("patch actions = %d, want 1", got)
	}
}

func TestExecuteApprovedRollbackRevalidatesAtMutationStartTime(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	startedAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	currentRevision := mustRevision(t, deployment.Spec.Template)
	targetRevision := mustRevision(t, stable.Spec.Template)
	proposal, err := domain.NewRollbackProposal("action", "incident", "policy", target, currentRevision, targetRevision, startedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := domain.NewPolicyDecision("policy", "incident", []domain.RuleResult{{Name: "allowed", Passed: true}}, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := domain.NewActor("alice", domain.ActorHuman)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewApproval("approval", actor, domain.ApprovalApproved, proposal.ActionID(), proposal.Hash(), startedAt, startedAt.Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.AuthorizeRollback(proposal, decision, approval, domain.DeploymentEvidence{Target: target, CurrentRevision: currentRevision}, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	times := []time.Time{
		startedAt,
		startedAt.Add(2 * time.Second),
		startedAt.Add(2 * time.Second),
	}
	now := func() time.Time {
		if len(times) == 0 {
			return startedAt.Add(2 * time.Second)
		}
		next := times[0]
		times = times[1:]
		return next
	}
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved), Now: now})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.ExecuteApprovedRollback(context.Background(), proposal.Hash(), "expired-revalidation-key")
	if err != nil {
		t.Fatal(err)
	}
	if result.FromRevision != currentRevision || result.ToRevision != targetRevision {
		t.Fatalf("mutation result = %#v", result)
	}
	if got := patchActionCount(client.Actions()); got != 1 {
		t.Fatalf("patch actions = %d, want 1", got)
	}
}

func TestExecuteApprovedRollbackRejectsAmbiguousStableReplicaSetsBeforePatch(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	otherStable := testReplicaSet(deployment, "other-stable", "healthy:v0", true, time.Now().Add(-2*time.Minute))
	client := fake.NewClientset(deployment, current, stable, otherStable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved)})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "ambiguous-stable-key")
	if !errors.Is(err, ErrStableRevision) {
		t.Fatalf("ExecuteApprovedRollback() error = %v, want %v", err, ErrStableRevision)
	}
	if got := patchActionCount(client.Actions()); got != 0 {
		t.Fatalf("patch actions = %d, want 0", got)
	}
}

func TestExecuteApprovedRollbackReleasesPendingMutationThroughGuardPort(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	otherStable := testReplicaSet(deployment, "other-stable", "healthy:v0", true, time.Now().Add(-2*time.Minute))
	client := fake.NewClientset(deployment, current, stable, otherStable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	guard := newSharedMutationGuard(approved)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: decoratedMutationGuard{guard}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "release-key")
	if !errors.Is(err, ErrStableRevision) {
		t.Fatalf("ExecuteApprovedRollback() error = %v, want %v", err, ErrStableRevision)
	}
	released := guard.releasedKeys()
	if len(released) != 1 || released[0] != "release-key" {
		t.Fatalf("released mutation keys = %v, want [release-key]", released)
	}
}

func TestExecuteApprovedRollbackReportsGuardReleaseFailure(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	otherStable := testReplicaSet(deployment, "other-stable", "healthy:v0", true, time.Now().Add(-2*time.Minute))
	client := fake.NewClientset(deployment, current, stable, otherStable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	guard := newSharedMutationGuard(approved)
	guard.releaseErr = errors.New("unlock failed")
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: decoratedMutationGuard{guard}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "release-failure-key")
	if !errors.Is(err, ErrStableRevision) || !errors.Is(err, guard.releaseErr) {
		t.Fatalf("ExecuteApprovedRollback() error = %v, want both %v and %v", err, ErrStableRevision, guard.releaseErr)
	}
}

func TestExecuteApprovedRollbackReleasesBeforeAndAfterReconcilingPatchConflict(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	client.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, deployment.Name, errors.New("resource version changed"))
	})
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	guard := newSharedMutationGuard(approved)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: decoratedMutationGuard{guard}})
	if err != nil {
		t.Fatal(err)
	}

	// The patch conflicts and the deployment still runs the bad revision, so
	// reconciliation reports drift: one release before it and one after.
	_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "conflict-key")
	if !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("ExecuteApprovedRollback() error = %v, want %v", err, ErrRevisionDrift)
	}
	released := guard.releasedKeys()
	if len(released) != 2 || released[0] != "conflict-key" || released[1] != "conflict-key" {
		t.Fatalf("released mutation keys = %v, want [conflict-key conflict-key]", released)
	}
}

func TestRollbackRejectsUntrustedCommandBeforeKubernetesAccess(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.ExecuteApprovedRollback(context.Background(), "untrusted-hash", "untrusted-key"); !errors.Is(err, domain.ErrApprovalMismatch) {
		t.Fatalf("untrusted command error = %v", err)
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("Kubernetes actions before trusted authorization = %#v", actions)
	}
}

func TestRollbackRecoversPendingMutationWhenTargetIsAlreadyApplied(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("healthy:v1")
	current := testReplicaSet(deployment, "stable-current", "healthy:v1", true, time.Now())
	client := fake.NewClientset(deployment, current)
	approved := testApprovedRollback(t, target, "revision-bad", mustRevision(t, deployment.Spec.Template))
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: newSharedMutationGuard(approved)})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "crash-window-key")
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyApplied || result.ToRevision != approved.Proposal().TargetRevision() {
		t.Fatalf("reconciled mutation = %#v", result)
	}
	if got := patchActionCount(client.Actions()); got != 0 {
		t.Fatalf("patch actions during crash recovery = %d, want 0", got)
	}
}

// After the patch is applied, a failure to record it in the ledger must not
// surface as a domain rejection (which the client would read as "nothing was
// mutated" and record as a failed execution). It is ErrMutationNotRecorded,
// which the MCP server reports as internal so the client reconciles.
func TestExecuteApprovedRollbackReportsLedgerFailureAfterPatchAsNotRecorded(t *testing.T) {
	for name, completeErr := range map[string]error{
		"idempotency conflict": &domain.IdempotencyConflictError{Key: "recording-key"},
		"validation":           &domain.ValidationError{Field: "rollback_mutation", Reason: "completion precedes start"},
		"lease not held":       ports.ErrRollbackMutationLeaseNotHeld,
	} {
		t.Run(name, func(t *testing.T) {
			target := mustTarget(t)
			deployment := testDeployment("broken:v2")
			current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
			stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
			client := fake.NewClientset(deployment, current, stable)
			approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
			guard := newSharedMutationGuard(approved)
			guard.completeErr = completeErr
			service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: decoratedMutationGuard{guard}})
			if err != nil {
				t.Fatal(err)
			}

			_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "recording-key")

			if !errors.Is(err, ErrMutationNotRecorded) {
				t.Fatalf("error = %v, want ErrMutationNotRecorded", err)
			}
			for _, rejection := range []error{domain.ErrIdempotencyConflict, domain.ErrValidation, ports.ErrRollbackMutationLeaseNotHeld} {
				if errors.Is(err, rejection) {
					t.Fatalf("error = %v still matches %v after the patch was applied", err, rejection)
				}
			}
			if got := patchActionCount(client.Actions()); got != 1 {
				t.Fatalf("patch actions = %d, want 1", got)
			}
			if held, _ := guard.heldLeases(); held != 0 {
				t.Fatalf("held leases = %d, want the lock released", held)
			}
		})
	}
}

// The service must hand the guard the exact attempt Begin returned: the fake
// guard, like the Postgres one, completes and releases only for the holding
// lease, so a dropped or rebuilt attempt fails these paths.
func TestExecuteApprovedRollbackCompletesAndReleasesWithTheAttemptLease(t *testing.T) {
	target := mustTarget(t)
	deployment := testDeployment("broken:v2")
	current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
	stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
	client := fake.NewClientset(deployment, current, stable)
	approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
	guard := newSharedMutationGuard(approved)
	service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: decoratedMutationGuard{guard}})
	if err != nil {
		t.Fatal(err)
	}

	first, err := service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "lease-key")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "lease-key")
	if err != nil {
		t.Fatal(err)
	}

	if first != replay || first.AlreadyApplied {
		t.Fatalf("first = %#v, replay = %#v, want one recorded mutation replayed", first, replay)
	}
	if got := patchActionCount(client.Actions()); got != 1 {
		t.Fatalf("patch actions = %d, want 1", got)
	}
	if held, stale := guard.heldLeases(); held != 0 || stale != 0 {
		t.Fatalf("held leases = %d, stale releases = %d, want 0/0", held, stale)
	}
}

func testApprovedRollback(t *testing.T, target domain.DeploymentTarget, currentRevision, targetRevision domain.Revision) domain.ApprovedRollback {
	t.Helper()
	now := time.Now().UTC()
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
	return approved
}

type sharedMutationGuard struct {
	mu       sync.Mutex
	approved map[domain.ProposalHash]domain.ApprovedRollback
	records  map[domain.IdempotencyKey]domain.RollbackMutation
	// leases holds the lease of the attempt that holds each key's lock, as
	// the Postgres guard does: Complete and Release act only for that lease.
	leases    map[domain.IdempotencyKey]ports.RollbackMutationLease
	nextLease int
	released  []domain.IdempotencyKey
	// staleReleases counts Release calls whose lease no longer held the lock
	// (harmless: such a release must touch nothing).
	staleReleases int
	// releaseErr is returned by every Release call when set.
	releaseErr error
	// reconcileOnly marks every pending attempt reconcile-only, as the Postgres
	// guard does when a resumed approval's approver is no longer authorized.
	reconcileOnly bool
	// completeErr is returned by Complete, after the lease check, when set.
	completeErr error
}

func newSharedMutationGuard(approved domain.ApprovedRollback) *sharedMutationGuard {
	return &sharedMutationGuard{
		approved: map[domain.ProposalHash]domain.ApprovedRollback{approved.Proposal().Hash(): approved},
		records:  make(map[domain.IdempotencyKey]domain.RollbackMutation),
		leases:   make(map[domain.IdempotencyKey]ports.RollbackMutationLease),
	}
}

func (g *sharedMutationGuard) lease(key domain.IdempotencyKey) ports.RollbackMutationLease {
	g.nextLease++
	lease := ports.RollbackMutationLease(fmt.Sprintf("lease-%d", g.nextLease))
	g.leases[key] = lease
	return lease
}

func (g *sharedMutationGuard) heldLeases() (held, stale int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.leases), g.staleReleases
}

func (g *sharedMutationGuard) Begin(_ context.Context, key domain.IdempotencyKey, hash domain.ProposalHash, now time.Time) (ports.RollbackMutationAttempt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.records[key]; ok {
		if existing.ProposalHash() != hash {
			return ports.RollbackMutationAttempt{}, &domain.IdempotencyConflictError{Key: key}
		}
		if existing.State() == domain.RollbackMutationCompleted {
			return ports.RollbackMutationAttempt{Mutation: existing, Approved: g.approved[hash]}, nil
		}
		return ports.RollbackMutationAttempt{Mutation: existing, Approved: g.approved[hash], Lease: g.lease(key), ReconcileOnly: g.reconcileOnly}, nil
	}
	approved, ok := g.approved[hash]
	if !ok {
		return ports.RollbackMutationAttempt{}, &domain.ApprovalMismatchError{Reason: "trusted execution record is missing"}
	}
	proposal := approved.Proposal()
	record, err := domain.NewPendingRollbackMutation(key, hash, domain.ExecutionID("mutation-"+string(key)), proposal.CurrentRevision(), proposal.TargetRevision(), now)
	if err != nil {
		return ports.RollbackMutationAttempt{}, err
	}
	g.records[key] = record
	return ports.RollbackMutationAttempt{Mutation: record, Approved: approved, Lease: g.lease(key), ReconcileOnly: g.reconcileOnly}, nil
}

func (g *sharedMutationGuard) Complete(_ context.Context, attempt ports.RollbackMutationAttempt, alreadyApplied bool, completedAt time.Time) (domain.RollbackMutation, error) {
	pending := attempt.Mutation
	g.mu.Lock()
	defer g.mu.Unlock()
	if attempt.Lease == "" || g.leases[pending.Key()] != attempt.Lease {
		return domain.RollbackMutation{}, ports.ErrRollbackMutationLeaseNotHeld
	}
	delete(g.leases, pending.Key())
	if g.completeErr != nil {
		return domain.RollbackMutation{}, g.completeErr
	}
	existing, ok := g.records[pending.Key()]
	if !ok || existing.ProposalHash() != pending.ProposalHash() {
		return domain.RollbackMutation{}, &domain.IdempotencyConflictError{Key: pending.Key()}
	}
	if existing.State() == domain.RollbackMutationCompleted {
		return existing, nil
	}
	completed, err := existing.Complete(alreadyApplied, completedAt)
	if err != nil {
		return domain.RollbackMutation{}, err
	}
	g.records[pending.Key()] = completed
	return completed, nil
}

func (g *sharedMutationGuard) Release(_ context.Context, attempt ports.RollbackMutationAttempt) error {
	pending := attempt.Mutation
	g.mu.Lock()
	defer g.mu.Unlock()
	g.released = append(g.released, pending.Key())
	switch held := g.leases[pending.Key()]; {
	case attempt.Lease != "" && held == attempt.Lease:
		delete(g.leases, pending.Key())
	case attempt.Lease != "":
		g.staleReleases++
	}
	return g.releaseErr
}

func (g *sharedMutationGuard) releasedKeys() []domain.IdempotencyKey {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]domain.IdempotencyKey(nil), g.released...)
}

// decoratedMutationGuard exposes exactly the port's method set, as a metrics
// or retry decorator around the real guard would.
type decoratedMutationGuard struct {
	ports.RollbackMutationGuard
}

func patchActionCount(actions []k8stesting.Action) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == "patch" {
			count++
		}
	}
	return count
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deployment.Namespace, UID: types.UID(name), CreationTimestamp: metav1.NewTime(created), Labels: map[string]string{"app": "demo-api"}, Annotations: annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: new(true)}}},
		Spec:       appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "demo-api", appsv1.DefaultDeploymentUniqueLabelKey: name}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}}}},
	}
}

func testPod(replicaSet *appsv1.ReplicaSet, name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: replicaSet.Namespace,
			UID:       uid,
			Labels:    map[string]string{"app": "demo-api"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       replicaSet.Name,
				UID:        replicaSet.UID,
				Controller: new(true),
			}},
		},
	}
}

func mustRevision(t *testing.T, template corev1.PodTemplateSpec) domain.Revision {
	t.Helper()
	revision, err := TemplateRevision(template)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}
