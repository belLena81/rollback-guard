#!/usr/bin/env bash
# Creates an incident for the demo target through POST /v1/incidents.
#
# The symptom is the demo-api HTTP error rate that Prometheus reports right
# now over METRICS_WINDOW (the observability MCP's own query), unless
# INCIDENT_OBSERVED overrides it. The request carries a fresh sampled W3C
# traceparent; its trace ID is saved so `make verify` can read the incident's
# REST -> A2A -> MCP trace back from the trace backend.
#
# Env: ORCHESTRATOR_URL, PROMETHEUS_URL, INCIDENT_THRESHOLD (default 0.1),
#      INCIDENT_OBSERVED, INCIDENT_STARTED_AT (RFC 3339, default now),
#      DEMO_STATE_DIR, METRICS_WINDOW, DEMO_NAMESPACE, DEMO_DEPLOYMENT.
set -euo pipefail
SCRIPT_NAME=create-incident
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools curl jq od

threshold="${INCIDENT_THRESHOLD:-0.1}"
observed="${INCIDENT_OBSERVED:-}"
if [[ -z "$observed" ]]; then
  observed="$(prom_query "$(error_rate_query)")" || die "Prometheus query failed at PROMETHEUS_URL"
  [[ -n "$observed" ]] || die "Prometheus has no demo-api request series over $METRICS_WINDOW; run make generate-load"
fi
started_at="${INCIDENT_STARTED_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

body="$(jq -nc \
  --arg cluster "$TARGET_CLUSTER" --arg namespace "$DEMO_NAMESPACE" \
  --arg deployment "$DEMO_DEPLOYMENT" --arg service "$TARGET_SERVICE" \
  --argjson observed "$observed" --argjson threshold "$threshold" --arg startedAt "$started_at" \
  '{target: {cluster: $cluster, namespace: $namespace, deployment: $deployment, service: $service},
    symptom: {kind: "http_error_rate", observed: $observed, threshold: $threshold, unit: "ratio", startedAt: $startedAt}}')"

trace_id="$(new_trace_id)"
api POST /v1/incidents "$body" "$trace_id"
[[ "$API_STATUS" == 202 ]] || die "POST /v1/incidents returned $API_STATUS: $(error_summary)"

id="$(jq -r '.incident.id' <<<"$API_BODY")"
valid_id "$id" || die "the API returned no usable incident ID"
mkdir -p "$DEMO_STATE_DIR"
rm -f "$STATE_FILE"
state_set INCIDENT_ID "$id"
state_set CREATE_TRACE_ID "$trace_id"

echo "$SCRIPT_NAME: incident $id created (status $(jq -r '.incident.status' <<<"$API_BODY"))"
echo "$SCRIPT_NAME: symptom http_error_rate observed=$observed threshold=$threshold startedAt=$started_at"
echo "$SCRIPT_NAME: trace $trace_id; poll $ORCHESTRATOR_URL/v1/incidents/$id"
