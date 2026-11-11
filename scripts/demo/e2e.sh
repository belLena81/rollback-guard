#!/usr/bin/env bash
# Unattended end-to-end demo run against the kind cluster and the running
# services (make services-up).
#
#   e2e.sh rollback   Phase 6 exit gate (make e2e):
#                     reset to v1-healthy -> load -> v2-bad-error-storm ->
#                     error rate crosses INCIDENT_THRESHOLD in Prometheus ->
#                     create incident -> awaiting_approval -> approve the exact
#                     proposal -> verify resolved (API, cluster, audit, trace)
#   e2e.sh decline    policy-decline scenario (make e2e-decline):
#                     reset to v1-healthy -> load -> v2-bad-crashloop (stalls;
#                     v1 keeps serving, so the error rate stays at 0) ->
#                     incident reporting an error spike -> investigation finds
#                     no breached metric -> policy declines -> verify declined
#                     and the Deployment unmutated
#
# With DEMO_AUTH=github (set by make e2e / e2e-decline) the whole run goes
# through real sessions against the fake GitHub: the operator creates the
# incident, the operator and a viewer are refused (403) on an awaiting
# proposal, the administrator approves, a viewer cannot create, a non-member
# gets no session, logout ends a session, and the audit timeline shows the
# github:<id> actors. With DEMO_AUTH=local the auth steps are skipped.
#
# Exits non-zero with a FAIL line naming the step on any failure. Load runs
# for the whole scenario so detection and recovery see real traffic.
set -euo pipefail
SCRIPT_NAME=e2e
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools curl jq od awk

mode="${1:?usage: e2e.sh rollback|decline}"
[[ "$mode" == rollback || "$mode" == decline ]] || die "mode must be rollback or decline"
make_cmd="${MAKE:-make}"
threshold="${INCIDENT_THRESHOLD:-0.1}"
demo_url="${DEMO_URL:?DEMO_URL is required}"
load_concurrency="${LOAD_CONCURRENCY:-4}"
breach_timeout="${E2E_BREACH_TIMEOUT:-180}"
investigation_timeout="${E2E_INVESTIGATION_TIMEOUT:-180}"
script_dir="$(dirname "$0")"

start=$SECONDS
step_start=$SECONDS
timings=()
current_step=""
step() {
  if [[ -n "$current_step" ]]; then
    timings+=("$(printf '%4ds  %s' $((SECONDS - step_start)) "$current_step")")
  fi
  current_step="$1"
  step_start=$SECONDS
  echo
  echo "[$(date -u +%H:%M:%S) +$((SECONDS - start))s] ==> $1"
}

load_pid=""
cleanup() {
  local status=$?
  if [[ -n "$load_pid" ]] && kill -0 "$load_pid" 2>/dev/null; then
    kill "$load_pid" 2>/dev/null || true
    wait "$load_pid" 2>/dev/null || true
  fi
  if ((status != 0)); then
    echo "$SCRIPT_NAME: FAIL during step '$current_step' after $((SECONDS - start))s (mode $mode)" >&2
    echo "$SCRIPT_NAME: service logs are in $DEMO_STATE_DIR/logs" >&2
  fi
}
trap cleanup EXIT

# number_ge A B: A >= B for decimal strings.
number_ge() {
  awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 >= b + 0) }'
}

# wait_for_metric DESCRIPTION TIMEOUT PREDICATE: polls Prometheus every 3s
# until PREDICATE (a function taking error rate and request rate) holds.
wait_for_metric() {
  local description="$1" timeout="$2" predicate="$3" deadline rate requests
  deadline=$((SECONDS + timeout))
  while :; do
    rate="$(prom_query "$(error_rate_query)" || true)"
    requests="$(prom_query "$(request_rate_query)" || true)"
    if [[ -n "$rate" && -n "$requests" ]] && "$predicate" "$rate" "$requests"; then
      echo "    $description: error rate $rate, request rate ${requests}/s over $METRICS_WINDOW"
      return 0
    fi
    if ((SECONDS >= deadline)); then
      die "timed out after ${timeout}s waiting for $description (last error rate '${rate:-none}', request rate '${requests:-none}')"
    fi
    sleep 3
  done
}
baseline_healthy() { number_ge "$2" 0.5 && ! number_ge "$1" "$threshold"; }
breached() { number_ge "$2" 0.5 && number_ge "$1" "$threshold"; }

wait_for_status() {
  local id="$1" want="$2" timeout="$3" deadline status summary
  deadline=$((SECONDS + timeout))
  while :; do
    summary="$(get_incident "$id")"
    status="$(jq -r '.incident.status' <<<"$summary")"
    if [[ "$status" == "$want" ]]; then
      echo "    incident $id is $status (investigation $(jq -r '.investigation.state // "-"' <<<"$summary"), diagnosis $(jq -r '.conclusion.recommendation // "-"' <<<"$summary") at confidence $(jq -r '.conclusion.confidence // "-"' <<<"$summary"))"
      return 0
    fi
    case "$status" in
    resolved | canceled | declined | rejected | expired | failed)
      die "incident $id ended $status while waiting for $want (investigation $(jq -r '.investigation.state // "-"' <<<"$summary") $(jq -r '.investigation.failureCode // ""' <<<"$summary"), diagnosis $(jq -r '.conclusion.recommendation // "-"' <<<"$summary"))"
      ;;
    esac
    if ((SECONDS >= deadline)); then
      die "timed out after ${timeout}s waiting for incident $id to reach $want (status $status)"
    fi
    sleep 1
  done
}

auth_mode="${DEMO_AUTH:-local}"
[[ "$auth_mode" == local || "$auth_mode" == github ]] || die "DEMO_AUTH must be local or github"
if [[ "$auth_mode" == github ]]; then
  [[ -f "$AUTH_TOKENS_FILE" ]] || die "no session tokens in $AUTH_TOKENS_FILE; start the stack with make services-up DEMO_AUTH=github"
fi

# expect_api DESCRIPTION WANT_STATUS AS METHOD PATH [BODY]: one request as the
# session AS ("" sends no credentials) that must answer WANT_STATUS.
expect_api() {
  local description="$1" want="$2" as="$3"
  shift 3
  API_AS="$as" api "$@"
  [[ "$API_STATUS" == "$want" ]] || die "$description: got HTTP $API_STATUS, want $want ($(error_summary))"
  echo "    ok: $description (HTTP $API_STATUS)"
}

# check_actor NAME ROLE ID: GET /v1/me for the session NAME names ROLE and the
# actor github:ID, so the login and the role path were real.
check_actor() {
  expect_api "demo-$1 session is valid" 200 "$1" GET /v1/me
  [[ "$(jq -r '"\(.role) \(.actorId)"' <<<"$API_BODY")" == "$2 github:$3" ]] ||
    die "demo-$1 should be $2 as github:$3, the API says $(jq -r '"\(.role) \(.actorId)"' <<<"$API_BODY")"
}

# check_authentication runs the identity and access checks that need no
# incident: anonymous 401, each role's identity, a non-member's refused login,
# a viewer's refused create, and logout ending a session.
check_authentication() {
  local probe body
  expect_api "no credentials is rejected" 401 "" GET /v1/me
  check_actor administrator administrator 1001
  check_actor operator operator 1002
  check_actor viewer viewer 1003
  probe="$(bash "$script_dir/auth-login.sh" probe outsider)"
  [[ "$probe" == "403 nosession" ]] || die "a non-member's login should be 403 without a session, got '$probe'"
  echo "    ok: demo-outsider (not in the allowed organization) is refused and gets no session (HTTP 403)"
  body="$(jq -nc --arg c "$TARGET_CLUSTER" --arg n "$DEMO_NAMESPACE" --arg d "$DEMO_DEPLOYMENT" --arg s "$TARGET_SERVICE" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{target: {cluster: $c, namespace: $n, deployment: $d, service: $s},
      symptom: {kind: "http_error_rate", observed: 0.5, threshold: 0.1, unit: "ratio", startedAt: $at}}')"
  expect_api "a viewer cannot create an incident" 403 viewer POST /v1/incidents "$body"
  bash "$script_dir/auth-login.sh" login viewer viewer-logout >/dev/null
  expect_api "a second viewer session is valid" 200 viewer-logout GET /v1/me
  expect_api "logout revokes the session" 204 viewer-logout POST /v1/auth/logout
  expect_api "the logged-out session is rejected on the next request" 401 viewer-logout GET /v1/me
  expect_api "a logged-out session cannot create an incident" 401 viewer-logout POST /v1/incidents "$body"
  expect_api "logging out one session leaves the other valid" 200 viewer GET /v1/me
}

# check_operator_cannot_approve ID: the operator and the viewer are refused on
# an incident that awaits approval, and it stays awaiting_approval.
check_approval_refused() {
  local id="$1" summary action hash body
  summary="$(get_incident "$id")"
  action="$(jq -r '.proposal.actionId // empty' <<<"$summary")"
  hash="$(jq -r '.proposal.proposalHash // empty' <<<"$summary")"
  valid_id "$action" && valid_id "$hash" || die "incident $id has no usable proposal"
  body="$(jq -nc --arg a "$action" --arg h "$hash" '{actionId: $a, proposalHash: $h}')"
  expect_api "an operator cannot approve the rollback" 403 operator POST "/v1/incidents/$id/approve" "$body"
  expect_api "a viewer cannot approve the rollback" 403 viewer POST "/v1/incidents/$id/approve" "$body"
  expect_api "an anonymous approval is rejected" 401 "" POST "/v1/incidents/$id/approve" "$body"
  body="$(jq -nc --arg a "$action" --arg h "$hash" '{actionId: $a, proposalHash: $h, reason: "e2e: viewer must not reject"}')"
  expect_api "a viewer cannot reject" 403 viewer POST "/v1/incidents/$id/reject" "$body"
  [[ "$(jq -r '.incident.status' <<<"$(get_incident "$id")")" == awaiting_approval ]] ||
    die "incident $id left awaiting_approval after refused requests"
}

# check_audit_actors ID CREATOR [APPROVER]: the audit timeline names the real
# GitHub actors (github:<id>), not the local actor.
check_audit_actors() {
  local id="$1" creator="$2" approver="${3:-}" events
  API_AS=viewer api GET "/v1/incidents/$id/events"
  [[ "$API_STATUS" == 200 ]] || die "events returned $API_STATUS"
  events="$API_BODY"
  [[ "$(jq -r '[.events[] | select(.kind == "incident.created") | .actor.id] | join(",")' <<<"$events")" == "$creator" ]] ||
    die "incident.created should be attributed to $creator"
  echo "    ok: incident.created is attributed to $creator"
  if [[ -n "$approver" ]]; then
    [[ "$(jq -r '[.events[] | select(.kind == "rollback.approved") | "\(.actor.kind) \(.actor.id)"] | join(",")' <<<"$events")" == "human $approver" ]] ||
      die "rollback.approved should be attributed to the human $approver"
    echo "    ok: rollback.approved is attributed to the human $approver"
  else
    [[ "$(jq -r '[.events[] | select(.kind | startswith("rollback.approved") or startswith("rollback.execution")) | .kind] | length' <<<"$events")" == 0 ]] ||
      die "a declined incident must have no approval or execution events"
  fi
}

echo "$SCRIPT_NAME: mode $mode, auth $auth_mode, threshold $threshold, metrics window $METRICS_WINDOW, orchestrator $ORCHESTRATOR_URL"

step "check services"
for port in 8091 8092 8093 8094; do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/health/ready" || true)" == 200 ]] ||
    die "service on 127.0.0.1:$port is not ready; run make services-up"
done
[[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$ORCHESTRATOR_URL/health/ready" || true)" == 200 ]] ||
  die "orchestrator at $ORCHESTRATOR_URL is not ready; run make services-up"
echo "    all five services ready"
if [[ "$auth_mode" == github ]]; then
  step "authentication and roles (real sessions, fake GitHub)"
  check_authentication
fi

step "deploy healthy (demo-reset: v1-healthy, stable mark, prune old ReplicaSets)"
"$make_cmd" --no-print-directory -s demo-reset

step "generate load"
mkdir -p "$DEMO_STATE_DIR/logs"
bash "$script_dir/generate-load.sh" "$demo_url" "${E2E_LOAD_SECONDS:-900}" "$load_concurrency" >"$DEMO_STATE_DIR/logs/e2e-load.log" 2>&1 &
load_pid=$!
wait_for_metric "healthy baseline (traffic flowing, error rate below $threshold)" 120 baseline_healthy

if [[ "$mode" == rollback ]]; then
  step "deploy broken (v2-bad-error-storm)"
  "$make_cmd" --no-print-directory -s demo-v2-broken DEMO_BROKEN=error-storm

  step "wait for error rate >= $threshold in Prometheus"
  wait_for_metric "threshold crossed" "$breach_timeout" breached

  step "create incident"
  bash "$script_dir/create-incident.sh"
  id="$(incident_id)"

  step "wait for awaiting_approval (A2A investigation, MCP evidence, policy)"
  wait_for_status "$id" awaiting_approval "$investigation_timeout"

  if [[ "$auth_mode" == github ]]; then
    step "approval is administrator-only (operator and viewer get 403)"
    check_approval_refused "$id"
  fi

  step "approve the exact proposal as the administrator (execute, watch rollout, verify recovery)"
  bash "$script_dir/approve-latest-action.sh"

  step "verify"
  VERIFY_REQUIRE_TRAFFIC=1 bash "$script_dir/verify.sh" resolved
  if [[ "$auth_mode" == github ]]; then
    step "audit names the real GitHub actors"
    check_audit_actors "$id" github:1002 github:1001
  fi
else
  step "deploy broken (v2-bad-crashloop; rollout stalls, v1 keeps serving)"
  "$make_cmd" --no-print-directory -s demo-v2-broken DEMO_BROKEN=crashloop
  # Wait until the new pod has crashed at least once, so the rollout is
  # visibly unhealthy, and record the generation the decline must not change.
  deadline=$((SECONDS + 90))
  until [[ "$(k get pods -l app.kubernetes.io/name=demo-api -o jsonpath='{range .items[*]}{.spec.containers[0].image} {.status.containerStatuses[0].restartCount}{"\n"}{end}' |
    awk '$1 ~ /crashloop/ && $2 >= 1 { print "yes" }')" == yes ]]; do
    ((SECONDS < deadline)) || die "the crashloop pod did not restart within 90s"
    sleep 2
  done
  echo "    crashloop pod restarting; v1 pod still serving"
  wait_for_metric "error rate still below $threshold (users are not affected)" 60 baseline_healthy
  generation="$(k get deployment "$DEMO_DEPLOYMENT" -o jsonpath='{.metadata.generation}')"

  step "create incident (alert reports an error spike the live metrics do not show)"
  # The reported observation is the alert's claim, not Prometheus' current
  # value; the investigation must decide from its own evidence.
  INCIDENT_OBSERVED="${DECLINE_REPORTED_ERROR_RATE:-0.5}" bash "$script_dir/create-incident.sh"
  state_set PRE_INCIDENT_GENERATION "$generation"
  id="$(incident_id)"

  step "wait for declined (A2A investigation, MCP evidence, policy)"
  wait_for_status "$id" declined "$investigation_timeout"

  step "verify"
  VERIFY_REQUIRE_POLICY_RULES=1 bash "$script_dir/verify.sh" declined
  if [[ "$auth_mode" == github ]]; then
    step "audit names the real GitHub actor"
    check_audit_actors "$id" github:1002
  fi
fi

step "done"
echo
echo "$SCRIPT_NAME: timings (mode $mode)"
printf '    %s\n' "${timings[@]}"
echo "$SCRIPT_NAME: PASSED in $((SECONDS - start))s (mode $mode, incident $id)"
