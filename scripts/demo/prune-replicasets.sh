#!/usr/bin/env bash
# Deletes the scaled-down ReplicaSets of earlier rollouts of one Deployment,
# keeping the current one and the one marked rollback-guardian.io/stable.
#
# Why: Kubernetes reuses an old ReplicaSet when a Deployment returns to a Pod
# template it has run before. Rollback Guardian dates a rollout by the current
# ReplicaSet's creation time, so re-deploying demo-api:v2-bad-* would look like
# a rollout from the first run, outside RULE_CORRELATION_WINDOW, and the policy
# would (correctly, fail-safe) decline it as not temporally correlated. Pruning
# the history makes every demo run start a fresh broken ReplicaSet.
#
# This is an operator demo-reset step run with the operator's kubeconfig. It
# only deletes ReplicaSets that the Deployment controls, that are scaled to
# zero, and that are neither current nor stable. The Rollback Guardian service
# accounts cannot delete anything.
#
# Usage: prune-replicasets.sh <namespace> <deployment>
# Env:   KUBECTL (default kubectl), KUBE_CONTEXT (required)
set -euo pipefail

namespace=${1:?namespace required}
deployment=${2:?deployment required}
context=${KUBE_CONTEXT:?KUBE_CONTEXT required}
k=("${KUBECTL:-kubectl}" --context "$context" -n "$namespace")

uid=$("${k[@]}" get deployment "$deployment" -o jsonpath='{.metadata.uid}')
revision=$("${k[@]}" get deployment "$deployment" -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')
if [[ -z "$uid" || -z "$revision" ]]; then
  echo "prune-replicasets: deployment $namespace/$deployment has no uid or revision" >&2
  exit 1
fi

# name, controller-owner UID, deployment revision, stable annotation, desired, current replicas
rows=$("${k[@]}" get replicasets -o go-template='{{range .items}}{{$owner := ""}}{{range .metadata.ownerReferences}}{{if .controller}}{{$owner = .uid}}{{end}}{{end}}{{.metadata.name}} {{$owner}} {{index .metadata.annotations "deployment.kubernetes.io/revision"}} {{or (index .metadata.annotations "rollback-guardian.io/stable") "-"}} {{or .spec.replicas 0}} {{or .status.replicas 0}}{{"\n"}}{{end}}')

pruned=0
while read -r name owner rs_revision stable desired current; do
  if [[ -z "${name:-}" || "$owner" != "$uid" || "$rs_revision" == "$revision" || "$stable" == "true" ]]; then
    continue
  fi
  if [[ "$desired" != 0 || "$current" != 0 ]]; then
    continue
  fi
  "${k[@]}" delete replicaset "$name" --wait=true >/dev/null
  echo "prune-replicasets: deleted scaled-down replicaset/$name (revision $rs_revision)"
  pruned=$((pruned + 1))
done <<<"$rows"
echo "prune-replicasets: $pruned old ReplicaSet(s) removed from $namespace/$deployment"
