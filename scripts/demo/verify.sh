#!/usr/bin/env bash
# Verifies the outcome of the latest incident (or INCIDENT_ID) from the API,
# the cluster, and (when tracing is configured) the trace backend.
#
#   verify.sh [resolved|declined]
#
# resolved (default), the Phase 6 exit gate:
#   - incident resolved; execution succeeded; verification passed with
#     replicas ready and the error rate below the symptom threshold
#   - the approval names the exact proposal (action ID and hash)
#   - the Deployment is back on the stable ReplicaSet, running STABLE_IMAGE,
#     at the proposal's target revision
#   - the audit timeline contains every major step, in order
#   - one trace links REST -> A2A -> MCP for the incident (and the approval's
#     trace reaches the rollback MCP), read back from TRACE_QUERY_URL whenever
#     OTEL_EXPORTER_OTLP_ENDPOINT is set (missing TRACE_QUERY_URL then fails);
#     without OTEL_EXPORTER_OTLP_ENDPOINT the trace check is reported as
#     SKIPPED, never as passed
#
# declined, the policy-decline scenario:
#   - incident declined after a policy decision with allowed=false
#   - no proposal, approval, execution, or verification exists
#   - no rollback event in the timeline
#   - the Deployment was not mutated (same generation as recorded before the
#     incident by e2e.sh) and is not on the stable ReplicaSet
#
# Every check is printed; the script exits 1 if any check failed.
set -euo pipefail
SCRIPT_NAME=verify
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools curl jq

expect="${1:-resolved}"
[[ "$expect" == resolved || "$expect" == declined ]] || die "expected outcome must be resolved or declined"

failures=0
checks=0
pass() {
  checks=$((checks + 1))
  echo "  PASS  $*"
}
fail() {
  checks=$((checks + 1))
  failures=$((failures + 1))
  echo "  FAIL  $*"
}
check() {
  local description="$1"
  shift
  if "$@"; then pass "$description"; else fail "$description"; fi
}
jq_true() {
  [[ "$(jq -r "$1" <<<"$2")" == true ]]
}

id="$(incident_id)"
summary="$(get_incident "$id")"
api GET "/v1/incidents/$id/events"
[[ "$API_STATUS" == 200 ]] || die "GET events returned $API_STATUS: $(error_summary)"
events="$API_BODY"

echo "verify: incident $id (expect $expect)"
echo "verify: timeline"
jq -r '.events[] | "    \(.sequence | tostring | (" " * (3 - length)) + .)  \(.occurredAt)  \(.kind)  \(.actor.kind):\(.actor.id)  \(.detail | to_entries | map("\(.key)=\(.value | if type == "object" then (.id // .cluster // tojson) else tostring end)") | join(" "))"' <<<"$events"

# Ordered audit trail. Each event becomes one token; the expected tokens must
# appear as an ordered subsequence and the sequence numbers must be strictly
# increasing from 1 without gaps.
tokens="$(jq -r '.events[] |
  if .kind == "incident.status_changed" then "status:\(.detail.to)"
  elif .kind == "investigation.evidence_collected" then "evidence:\(.detail.kind)"
  elif .kind == "investigation.diagnosis_completed" then "diagnosis:\(.detail.recommendation)"
  elif .kind == "policy.evaluated" then "policy:\(.detail.allowed)"
  elif .kind == "rollback.execution_completed" then "execution:\(.detail.outcome)"
  elif .kind == "rollback.verification_completed" then "verification:\(.detail.passed)"
  else .kind end' <<<"$events")"

ordered_subsequence() {
  local expected=("$@") index=0 token
  while IFS= read -r token; do
    if ((index < ${#expected[@]})) && [[ "$token" == "${expected[index]}" ]]; then
      index=$((index + 1))
    fi
  done <<<"$tokens"
  if ((index == ${#expected[@]})); then
    return 0
  fi
  echo "        first missing step (in order): ${expected[index]}"
  return 1
}

check "audit sequence numbers are 1..n without gaps" \
  jq_true '[.events[].sequence] == [range(1; (.events | length) + 1)]' "$events"
check "audit timestamps never go backwards" \
  jq_true '[.events[].occurredAt | sub("\\.[0-9]+"; "") | fromdateiso8601] as $t | [range(1; $t | length) | $t[.] >= $t[. - 1]] | all' "$events"
check "no incident.failed, incident.expired, or incident.canceled event" \
  jq_true '[.events[] | select(.kind == "incident.failed" or .kind == "incident.expired" or .kind == "incident.canceled")] == []' "$events"

# count_kind KIND: number of events of one kind.
count_kind() {
  jq -r --arg kind "$1" '[.events[] | select(.kind == $kind)] | length' <<<"$events"
}

# The conclusion may only reference evidence the timeline shows as collected
# for this incident (evidence persisted, not just counted).
evidence_referenced_was_collected() {
  jq -e --argjson events "$events" '
    ([$events.events[] | select(.kind == "investigation.evidence_collected") | .detail.evidenceId]) as $collected
    | (.conclusion.evidenceIds | length) >= 2
    and ([.conclusion.evidenceIds[] | select(. as $id | $collected | index($id) | not)] == [])' <<<"$summary" >/dev/null
}

if [[ "$expect" == resolved ]]; then
  check "incident status is resolved (got $(jq -r '.incident.status' <<<"$summary"))" \
    jq_true '.incident.status == "resolved"' "$summary"
  check "diagnosis recommends rollback" jq_true '.conclusion.recommendation == "rollback"' "$summary"
  check "the diagnosis references at least two pieces of evidence, all collected in the timeline" \
    evidence_referenced_was_collected
  for kind in rollback.proposal_created rollback.approved rollback.execution_completed rollback.verification_completed; do
    check "exactly one $kind event (got $(count_kind "$kind"))" test "$(count_kind "$kind")" = 1
  done
  check "approval is a human approval of the exact proposal (action ID and hash)" \
    jq_true '.approval.decision == "approved" and .approval.actor.kind == "human"
      and .approval.actionId == .proposal.actionId and .approval.proposalHash == .proposal.proposalHash' "$summary"
  check "execution succeeded from the proposal's current to its target revision" \
    jq_true '.execution.outcome == "succeeded" and .execution.fromRevision == .proposal.currentRevision
      and .execution.toRevision == .proposal.targetRevision' "$summary"
  check "recovery verification passed" jq_true '.verification.passed == true' "$summary"
  check "recovery criteria: target revision, all replicas ready and available" \
    jq_true '.verification.revision == .proposal.targetRevision
      and .verification.desiredReplicas > 0 and .verification.readyReplicas == .verification.desiredReplicas
      and .verification.availableReplicas == .verification.desiredReplicas' "$summary"
  check "recovery criteria passed: error rate $(jq -r '.verification.metricValue' <<<"$summary") below threshold $(jq -r '.incident.symptom.threshold' <<<"$summary")" \
    jq_true '(.verification.metricValue | type) == "number" and (.incident.symptom.threshold | type) == "number"
      and .verification.metricValue < .incident.symptom.threshold' "$summary"
  # The error-rate query reports 0 when there is no traffic at all, so a
  # recovery measured without load proves nothing. e2e.sh keeps load running
  # and sets VERIFY_REQUIRE_TRAFFIC=1.
  if [[ "${VERIFY_REQUIRE_TRAFFIC:-0}" == 1 ]]; then
    requests="$(prom_query "$(request_rate_query)" || true)"
    check "demo-api is serving traffic (${requests:-no} req/s over $METRICS_WINDOW), so the recovered error rate is not vacuous" \
      awk -v r="${requests:-0}" 'BEGIN { exit !(r + 0 > 0.5) }'
  fi
  hash="$(jq -r '.proposal.proposalHash' <<<"$summary")"
  proposal_hash_in_event() {
    jq -e --arg hash "$hash" '[.events[] | select(.kind == "rollback.proposal_created") | .detail.proposalHash] == [$hash]' <<<"$events" >/dev/null
  }
  check "the proposal_created event carries the approved proposal hash" proposal_hash_in_event
  check "the approval event is attributed to the human approver" \
    jq_true '[.events[] | select(.kind == "rollback.approved") | .actor.kind] == ["human"]' "$events"
  check "audit timeline contains every major step in order" ordered_subsequence \
    incident.created status:investigating evidence:deployment evidence:metric diagnosis:rollback policy:true \
    rollback.proposal_created status:awaiting_approval rollback.approved status:approved status:executing \
    execution:succeeded status:verifying verification:true status:resolved

  # Cluster: the Deployment is back on the stable ReplicaSet.
  if k rollout status "deployment/$DEMO_DEPLOYMENT" --timeout=30s >/dev/null 2>&1; then
    pass "deployment $DEMO_NAMESPACE/$DEMO_DEPLOYMENT rollout is complete"
  else
    fail "deployment $DEMO_NAMESPACE/$DEMO_DEPLOYMENT rollout is complete"
  fi
  revision="$(k get deployment "$DEMO_DEPLOYMENT" -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')"
  image="$(k get deployment "$DEMO_DEPLOYMENT" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  stable_rows="$(k get replicasets -o go-template='{{range .items}}{{if eq (index .metadata.annotations "rollback-guardian.io/stable") "true"}}{{.metadata.name}} {{index .metadata.annotations "deployment.kubernetes.io/revision"}} {{or .status.readyReplicas 0}}{{"\n"}}{{end}}{{end}}')"
  stable_count="$(grep -c . <<<"$stable_rows" || true)"
  read -r stable_name stable_revision stable_ready <<<"$stable_rows" || true
  check "exactly one stable ReplicaSet exists (found $stable_count)" test "$stable_count" = 1
  check "the deployment's current revision $revision is the stable ReplicaSet ${stable_name:-none}" \
    test "${stable_revision:-x}" = "$revision"
  check "the stable ReplicaSet has ready replicas (${stable_ready:-0})" test "${stable_ready:-0}" -ge 1
  check "the deployment runs $STABLE_IMAGE (got $image)" test "$image" = "${STABLE_IMAGE:-demo-api:v1-healthy}"
else
  check "incident status is declined (got $(jq -r '.incident.status' <<<"$summary"))" \
    jq_true '.incident.status == "declined"' "$summary"
  check "the diagnosis recommends no_action (got $(jq -r '.conclusion.recommendation // "none"' <<<"$summary") at confidence $(jq -r '.conclusion.confidence // "none"' <<<"$summary"))" \
    jq_true '.conclusion.recommendation == "no_action"' "$summary"
  check "the diagnosis references evidence collected in the timeline" evidence_referenced_was_collected
  check "exactly one policy.evaluated event, and it denied (got $(count_kind policy.evaluated))" \
    jq_true '[.events[] | select(.kind == "policy.evaluated") | .detail.allowed] == [false]' "$events"
  # Why the policy declined. The API does not expose the per-rule results yet,
  # so read them (read-only) from policy_decisions when the database is
  # reachable. e2e.sh requires it (VERIFY_REQUIRE_POLICY_RULES=1): the decline
  # must come from the missing metric breach, not from a stale rollout
  # timestamp (see docs/local-demo-env.md, "Reset and repeatability").
  rules=""
  if [[ -n "${DATABASE_URL:-}" ]] && command -v psql >/dev/null 2>&1; then
    decision_id="$(jq -r '[.events[] | select(.kind == "policy.evaluated") | .detail.decisionId][0] // empty' <<<"$events")"
    if valid_id "$decision_id"; then
      # psql interpolates :'id' (as a quoted literal) only in stdin input.
      rules="$(psql "$DATABASE_URL" -X -A -t -q -v ON_ERROR_STOP=1 -v id="$decision_id" 2>/dev/null \
        <<<"SELECT r->>'Name', r->>'Passed' FROM policy_decisions p, jsonb_array_elements(p.rules) r WHERE p.id = :'id';" || true)"
    fi
  fi
  if [[ -n "$rules" ]]; then
    echo "        failed policy rules: $(awk -F'|' '$2 == "false" { printf "%s ", $1 }' <<<"$rules")"
    check "policy rule current_metric_threshold_breached failed (no breached metric in the evidence)" \
      grep -qx 'current_metric_threshold_breached|false' <<<"$rules"
    check "policy rules rollout_temporally_correlated and rollout_recent passed (the decline is not a stale-rollout artifact)" \
      test "$(grep -cxE 'rollout_(temporally_correlated|recent)\|true' <<<"$rules")" = 2
  elif [[ "${VERIFY_REQUIRE_POLICY_RULES:-0}" == 1 ]]; then
    fail "policy rule results readable from policy_decisions (DATABASE_URL and psql required)"
  else
    echo "  SKIP  per-rule decline reasons (DATABASE_URL or psql not available; the API does not expose them)"
  fi
  check "no proposal, approval, execution, or verification exists" \
    jq_true '.proposal == null and .approval == null and .execution == null and .verification == null' "$summary"
  check "no rollback event in the timeline" \
    jq_true '[.events[] | select(.kind | startswith("rollback."))] == []' "$events"
  check "audit timeline records investigation, a denying policy decision, and the decline in order" ordered_subsequence \
    incident.created status:investigating evidence:deployment evidence:metric policy:false status:declined
  generation="$(k get deployment "$DEMO_DEPLOYMENT" -o jsonpath='{.metadata.generation}')"
  recorded="$(state_get PRE_INCIDENT_GENERATION)"
  if [[ -n "$recorded" ]]; then
    check "deployment not mutated: generation $generation equals the pre-incident generation $recorded" \
      test "$generation" = "$recorded"
  else
    echo "  SKIP  deployment generation unchanged (no PRE_INCIDENT_GENERATION recorded; run through make e2e-decline)"
  fi
  image="$(k get deployment "$DEMO_DEPLOYMENT" -o jsonpath='{.spec.template.spec.containers[0].image}')"
  check "the deployment still runs the declined rollout's image ($image), not $STABLE_IMAGE" \
    test "$image" != "${STABLE_IMAGE:-demo-api:v1-healthy}"
fi

# Trace check.
trace_result="skipped"
trace_reason=""
if [[ -z "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ]]; then
  trace_reason="OTEL_EXPORTER_OTLP_ENDPOINT is not set, so the services export no spans"
elif [[ -z "${TRACE_QUERY_URL:-}" ]]; then
  # Tracing is configured, so the trace must be proven; it cannot be.
  fail "trace check: OTEL_EXPORTER_OTLP_ENDPOINT is set but TRACE_QUERY_URL (a Jaeger query API) is not, so the trace cannot be read back"
  trace_result="failed"
elif [[ -z "$(state_get CREATE_TRACE_ID)" ]]; then
  fail "trace check: no CREATE_TRACE_ID recorded for this incident (create it with make create-incident)"
  trace_result="failed"
else
  # trace_services TRACE_ID: prints "service<TAB>operation" per span and, last,
  # "ROOTS <n>": the spans whose parent is not in the trace. The client's
  # traceparent span is never exported, so a fully linked trace has exactly
  # one such span: the REST server span.
  fetch_trace() {
    curl -sS --max-time 10 "$TRACE_QUERY_URL/api/traces/$1" 2>/dev/null |
      jq -r '(.data[0] // {spans: [], processes: {}}) as $t
        | ($t.spans | map(.spanID)) as $ids
        | ($t.spans[] | "\($t.processes[.processID].serviceName)\t\(.operationName)"),
          "ROOTS \([$t.spans[] | select(([.references[]? | select(.refType == "CHILD_OF") | .spanID] | map(. as $p | $ids | index($p)) | map(select(. != null)) | length) == 0)] | length)"' 2>/dev/null || true
  }
  has_span() {
    grep -q "^$1	$2\$" <<<"$3"
  }
  create_trace="$(state_get CREATE_TRACE_ID)"
  approve_trace="$(state_get APPROVE_TRACE_ID)"
  required=(
    "orchestrator	POST /v1/incidents"
    "orchestrator	investigation.dispatch"
    "investigator	investigation.execute"
    "kubernetes-mcp-readonly	mcp tool get_deployment"
    "observability-mcp	mcp tool get_http_error_rate"
  )
  # Spans are exported in batches; wait for the last ones to arrive.
  deadline=$((SECONDS + ${TRACE_WAIT_SECONDS:-30}))
  while :; do
    spans="$(fetch_trace "$create_trace")"
    missing=()
    for entry in "${required[@]}"; do
      has_span "${entry%%	*}" "${entry#*	}" "$spans" || missing+=("$entry")
    done
    if ((${#missing[@]} == 0)) || ((SECONDS >= deadline)); then break; fi
    sleep 2
  done
  # An empty or unreadable trace yields FAIL lines below, never an abort.
  roots="$({ grep '^ROOTS ' <<<"$spans" || true; } | cut -d' ' -f2)"
  span_count="$({ grep -v '^ROOTS ' <<<"$spans" || true; } | grep -c . || true)"
  services="$({ grep -v '^ROOTS ' <<<"$spans" || true; } | cut -f1 | sort -u | tr '\n' ' ')"
  echo "verify: incident trace $create_trace: $span_count spans across: $services"
  for entry in "${required[@]}"; do
    if has_span "${entry%%	*}" "${entry#*	}" "$spans"; then
      pass "trace $create_trace has span '${entry#*	}' from ${entry%%	*}"
    else
      fail "trace $create_trace has span '${entry#*	}' from ${entry%%	*}"
    fi
  done
  check "trace $create_trace is one connected tree (spans without a parent in the trace: ${roots:-none}, want 1)" \
    test "${roots:-0}" = 1
  if [[ "$expect" == resolved ]]; then
    if [[ -z "$approve_trace" ]]; then
      fail "approval trace recorded (approve with make approve-latest-action)"
    else
      approve_spans="$(fetch_trace "$approve_trace")"
      check "approval trace $approve_trace reaches the rollback MCP (execute_approved_rollback)" \
        has_span kubernetes-mcp-rollback "mcp tool execute_approved_rollback" "$approve_spans"
    fi
  fi
  trace_result="checked"
fi

echo
if [[ "$trace_result" == skipped ]]; then
  echo "verify: trace check SKIPPED (not passed): $trace_reason"
fi
if ((failures > 0)); then
  echo "verify: FAILED: $failures of $checks checks failed for incident $id" >&2
  exit 1
fi
if [[ "$trace_result" == skipped ]]; then
  echo "verify: PASSED: all $checks checks for incident $id ($expect); trace check SKIPPED"
else
  echo "verify: PASSED: all $checks checks for incident $id ($expect), including the trace check"
fi
