#!/usr/bin/env bash
# Marks the ReplicaSet behind the Deployment's current, fully rolled-out
# revision as the rollback target (rollback-guardian.io/stable=true) and removes
# the mark from every other ReplicaSet the Deployment owns, so exactly one
# stable ReplicaSet exists.
#
# This is an operator action ("this release is verified good"), run with the
# operator's kubeconfig. The Rollback Guardian service accounts cannot annotate
# ReplicaSets. Readiness alone does not prove a release is good (error-storm and
# high-latency builds pass /healthz), so the caller names the image it vouches
# for and the script refuses to mark anything else.
#
# Usage: mark-stable.sh <namespace> <deployment> <expected-image>
# Env:   KUBECTL (default kubectl), KUBE_CONTEXT (required)
set -euo pipefail

namespace=${1:?namespace required}
deployment=${2:?deployment required}
expected_image=${3:?expected image required}
context=${KUBE_CONTEXT:?KUBE_CONTEXT required}
k=("${KUBECTL:-kubectl}" --context "$context" -n "$namespace")
annotation=rollback-guardian.io/stable

fail() {
  echo "mark-stable: $*" >&2
  exit 1
}

deployment_revision() {
  "${k[@]}" get deployment "$deployment" -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}'
}

# Refuse to bless a rollout that has not completed.
"${k[@]}" rollout status "deployment/$deployment" --timeout=10s >/dev/null

uid=$("${k[@]}" get deployment "$deployment" -o jsonpath='{.metadata.uid}')
revision=$(deployment_revision)
[[ -n "$uid" && -n "$revision" ]] || fail "deployment $namespace/$deployment has no uid or revision"

# name, controller-owner UID, deployment revision, stable annotation, image, desired, ready
rows=$("${k[@]}" get replicasets -o go-template='{{range .items}}{{$owner := ""}}{{range .metadata.ownerReferences}}{{if .controller}}{{$owner = .uid}}{{end}}{{end}}{{.metadata.name}} {{$owner}} {{index .metadata.annotations "deployment.kubernetes.io/revision"}} {{or (index .metadata.annotations "rollback-guardian.io/stable") "-"}} {{(index .spec.template.spec.containers 0).image}} {{or .spec.replicas 0}} {{or .status.readyReplicas 0}}{{"\n"}}{{end}}')

current=""
others=()
while read -r name owner rs_revision stable image desired ready; do
  [[ -z "${name:-}" || "$owner" != "$uid" ]] && continue
  if [[ "$rs_revision" == "$revision" ]]; then
    [[ -z "$current" ]] || fail "several ReplicaSets carry revision $revision"
    [[ "$image" == "$expected_image" ]] || fail "current ReplicaSet $name runs $image, not $expected_image"
    [[ "$desired" -gt 0 && "$ready" == "$desired" ]] || fail "current ReplicaSet $name has $ready/$desired ready replicas"
    current=$name
  elif [[ "$stable" == "true" ]]; then
    others+=("$name")
  fi
done <<<"$rows"

[[ -n "$current" ]] || fail "no ReplicaSet found for $namespace/$deployment revision $revision"

"${k[@]}" annotate replicaset "$current" "$annotation=true" --overwrite >/dev/null
for name in "${others[@]}"; do
  "${k[@]}" annotate replicaset "$name" "$annotation-" >/dev/null
done

# A rollout that started after the checks above would leave the mark on a
# ReplicaSet that is no longer current; undo it rather than bless the wrong one.
if [[ "$(deployment_revision)" != "$revision" ]]; then
  "${k[@]}" annotate replicaset "$current" "$annotation-" >/dev/null
  fail "deployment moved past revision $revision while marking; no ReplicaSet is stable, rerun once the rollout settles"
fi
echo "mark-stable: replicaset/$current ($expected_image, deployment revision $revision) is the stable rollback target"
