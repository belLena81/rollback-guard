package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"k8s.io/client-go/kubernetes/fake"
)

// A resumed pending mutation whose approver is no longer authorized is
// reconcile-only: an earlier attempt may have patched before crashing, so a
// deployment already at the target is recorded as applied, and anything else
// is refused without a patch.
func TestReconcileOnlyAttemptRecordsAnAppliedPatchButNeverPatches(t *testing.T) {
	target := mustTarget(t)

	t.Run("already at target", func(t *testing.T) {
		deployment := testDeployment("healthy:v1")
		current := testReplicaSet(deployment, "stable-current", "healthy:v1", true, time.Now())
		client := fake.NewClientset(deployment, current)
		approved := testApprovedRollback(t, target, "revision-bad", mustRevision(t, deployment.Spec.Template))
		guard := newSharedMutationGuard(approved)
		guard.reconcileOnly = true
		service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: guard})
		if err != nil {
			t.Fatal(err)
		}
		result, err := service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "reconcile-key")
		if err != nil {
			t.Fatalf("reconcile-only at target error = %v", err)
		}
		if !result.AlreadyApplied || result.ToRevision != approved.Proposal().TargetRevision() {
			t.Fatalf("result = %#v", result)
		}
		if got := patchActionCount(client.Actions()); got != 0 {
			t.Fatalf("patch actions = %d, want 0", got)
		}
	})

	t.Run("not at target", func(t *testing.T) {
		deployment := testDeployment("broken:v2")
		current := testReplicaSet(deployment, "current", "broken:v2", false, time.Now())
		stable := testReplicaSet(deployment, "stable", "healthy:v1", true, time.Now().Add(-time.Minute))
		client := fake.NewClientset(deployment, current, stable)
		approved := testApprovedRollback(t, target, mustRevision(t, deployment.Spec.Template), mustRevision(t, stable.Spec.Template))
		guard := newSharedMutationGuard(approved)
		guard.reconcileOnly = true
		service, err := NewService(client, Config{ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, MutationGuard: guard})
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.ExecuteApprovedRollback(context.Background(), approved.Proposal().Hash(), "reconcile-key")
		var refused *domain.ApproverNotAuthorizedError
		if !errors.As(err, &refused) {
			t.Fatalf("reconcile-only off target error = %v, want ApproverNotAuthorizedError", err)
		}
		if got := patchActionCount(client.Actions()); got != 0 {
			t.Fatalf("patch actions = %d, want 0", got)
		}
		if held, _ := guard.heldLeases(); held != 0 {
			t.Fatalf("held leases after refusal = %d, want 0 (released)", held)
		}
		if guard.records["reconcile-key"].State() != domain.RollbackMutationPending {
			t.Fatal("refusal changed the pending ledger record")
		}
	})
}
