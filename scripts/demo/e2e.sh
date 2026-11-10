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

echo "$SCRIPT_NAME: mode $mode, threshold $threshold, metrics window $METRICS_WINDOW, orchestrator $ORCHESTRATOR_URL"

step "check services"
for port in 8091 8092 8093 8094; do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$port/health/ready" || true)" == 200 ]] ||
    die "service on 127.0.0.1:$port is not ready; run make services-up"
done
[[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$ORCHESTRATOR_URL/health/ready" || true)" == 200 ]] ||
  die "orchestrator at $ORCHESTRATOR_URL is not ready; run make services-up"
echo "    all five services ready"

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

  step "approve the exact proposal (execute, watch rollout, verify recovery)"
  bash "$script_dir/approve-latest-action.sh"

  step "verify"
  VERIFY_REQUIRE_TRAFFIC=1 bash "$script_dir/verify.sh" resolved
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
fi

step "done"
echo
echo "$SCRIPT_NAME: timings (mode $mode)"
printf '    %s\n' "${timings[@]}"
echo "$SCRIPT_NAME: PASSED in $((SECONDS - start))s (mode $mode, incident $id)"
