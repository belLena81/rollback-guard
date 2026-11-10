#!/usr/bin/env bash
# Least-privilege proof against the live API server: asks `kubectl auth can-i`
# as each Rollback Guardian service account and fails if any answer differs
# from the expectation pinned here (and in test/contract/rbac_test.go).
#
# Usage: rbac-check.sh <namespace>
# Env:   KUBECTL (default kubectl), KUBE_CONTEXT (required)
set -euo pipefail

namespace=${1:?namespace required}
context=${KUBE_CONTEXT:?KUBE_CONTEXT required}
kubectl=("${KUBECTL:-kubectl}" --context "$context")
readonly_sa="system:serviceaccount:$namespace:rollback-guardian-kubernetes-readonly"
rollback_sa="system:serviceaccount:$namespace:rollback-guardian-kubernetes-rollback"

failures=0
check() {
  local expected=$1 who=$2 ns=$3
  shift 3
  local got
  got=$("${kubectl[@]}" auth can-i "$@" -n "$ns" --as="$who" 2>/dev/null || true)
  local verdict=ok
  if [[ "$got" != "$expected" ]]; then
    verdict=FAIL
    failures=$((failures + 1))
  fi
  printf '%-4s %-9s can-i %-38s -n %-14s => %-3s (want %s)\n' \
    "$verdict" "${who##*kubernetes-}" "$*" "$ns" "$got" "$expected"
}

# Read-only identity: inspect demo-api, never mutate, never read secrets.
check yes "$readonly_sa" "$namespace" get deployments/demo-api
check yes "$readonly_sa" "$namespace" list replicasets
check yes "$readonly_sa" "$namespace" list pods
check yes "$readonly_sa" "$namespace" list events
check no "$readonly_sa" "$namespace" patch deployments
check no "$readonly_sa" "$namespace" patch deployments/demo-api
check no "$readonly_sa" "$namespace" update deployments/demo-api
check no "$readonly_sa" "$namespace" get secrets
check no "$readonly_sa" "$namespace" list secrets
check no "$readonly_sa" "$namespace" create pods/exec
check no "$readonly_sa" default get deployments/demo-api

# Rollback identity: get and patch exactly deployments/demo-api, list replicasets.
check yes "$rollback_sa" "$namespace" get deployments/demo-api
check yes "$rollback_sa" "$namespace" patch deployments/demo-api
check yes "$rollback_sa" "$namespace" list replicasets
check no "$rollback_sa" "$namespace" patch deployments/other
check no "$rollback_sa" "$namespace" get deployments/other
check no "$rollback_sa" "$namespace" list deployments
check no "$rollback_sa" "$namespace" update deployments/demo-api
check no "$rollback_sa" "$namespace" delete deployments/demo-api
check no "$rollback_sa" "$namespace" create deployments
check no "$rollback_sa" "$namespace" patch replicasets
check no "$rollback_sa" "$namespace" list pods
check no "$rollback_sa" "$namespace" get secrets
check no "$rollback_sa" "$namespace" list secrets
check no "$rollback_sa" "$namespace" create pods/exec
check no "$rollback_sa" default patch deployments/demo-api

# Neither identity may mint tokens, impersonate, escalate, watch, or reach
# other namespaces.
for sa in "$readonly_sa" "$rollback_sa"; do
  check no "$sa" "$namespace" patch replicasets
  check no "$sa" "$namespace" create pods
  check no "$sa" "$namespace" watch deployments
  check no "$sa" "$namespace" create serviceaccounts/token
  check no "$sa" "$namespace" impersonate serviceaccounts
  check no "$sa" "$namespace" bind roles
  check no "$sa" "$namespace" escalate roles
  check no "$sa" "$namespace" update roles
  check no "$sa" kube-system list deployments
  check no "$sa" kube-system get secrets
done

if ((failures > 0)); then
  echo "rbac-check: $failures unexpected answer(s)" >&2
  exit 1
fi
echo "rbac-check: all answers match least-privilege expectations"
