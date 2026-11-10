#!/usr/bin/env bash
# Writes a kubeconfig that authenticates as one Rollback Guardian service
# account, using a time-bound TokenRequest token (no long-lived Secret). The
# API server address and CA are taken from the operator's current context.
# The file is created with mode 0600 and the token is never printed.
#
# Usage: sa-kubeconfig.sh <namespace> <service-account> <duration> <output-file>
# Env:   KUBECTL (default kubectl), KUBE_CONTEXT (required)
set -euo pipefail
umask 077

namespace=${1:?namespace required}
service_account=${2:?service account required}
duration=${3:?token duration required}
output=${4:?output file required}
context=${KUBE_CONTEXT:?KUBE_CONTEXT required}
k=("${KUBECTL:-kubectl}" --context "$context")

server=$("${k[@]}" config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')
ca_data=$("${k[@]}" config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
if [[ -z "$server" || -z "$ca_data" ]]; then
  echo "sa-kubeconfig: context $context needs a server and embedded certificate-authority-data" >&2
  exit 1
fi
token=$("${k[@]}" -n "$namespace" create token "$service_account" --duration="$duration")

mkdir -p "$(dirname "$output")"
tmp=$(mktemp "$output.XXXXXX")
trap 'rm -f "$tmp"' EXIT
cat >"$tmp" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: demo
    cluster:
      server: $server
      certificate-authority-data: $ca_data
users:
  - name: $service_account
    user:
      token: $token
contexts:
  - name: $service_account
    context:
      cluster: demo
      namespace: $namespace
      user: $service_account
current-context: $service_account
EOF
mv "$tmp" "$output"
trap - EXIT
echo "sa-kubeconfig: wrote $output for $namespace/$service_account (token valid $duration)"
