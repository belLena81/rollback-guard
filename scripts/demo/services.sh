#!/usr/bin/env bash
# Run the five Rollback Guardian processes against the kind demo cluster.
#
#   services.sh up | down | status
#
# `up` starts the processes in dependency order from prebuilt binaries
# (make services-build), waits for each /health/ready, and records a pid and
# log file per process under $DEMO_STATE_DIR. `down` stops them in reverse
# order. `status` prints liveness and readiness. The make targets
# services-up/services-down/services-status build, migrate, and generate the
# service-account kubeconfigs first (docs/local-demo-env.md).
#
# Every process binds loopback (the config defaults). With DEMO_AUTH=github
# (make e2e, make services-up DEMO_AUTH=github) a sixth process, the fake
# GitHub (test/tools/fake-github), is started first and the orchestrator runs
# AUTH_MODE=github against it; once ready, auth-login.sh signs the fake users
# in. The client secret and state secret are generated per start, passed only
# through the child environment, and never written or printed. The default
# DEMO_AUTH=local keeps the unauthenticated local actor. Database URLs and
# kubeconfig contents are never printed.
set -euo pipefail

action="${1:?usage: services.sh up|down|status}"

state_dir="${DEMO_STATE_DIR:-.demo}"
bin_dir="${SERVICES_BIN_DIR:-bin/demo}"
ready_timeout="${SERVICES_READY_TIMEOUT:-60}"
stop_timeout="${SERVICES_STOP_TIMEOUT:-15}"
auth_mode="${DEMO_AUTH:-local}"
fake_port="${FAKE_GITHUB_PORT:-9180}"
auth_mode_file="$state_dir/auth-mode"
pid_dir="$state_dir/pids"
log_dir="$state_dir/logs"

# Start order; `down` walks it backwards. name:port
services=(
  kubernetes-mcp-readonly:8091
  kubernetes-mcp-rollback:8092
  observability-mcp:8093
  investigator:8094
  orchestrator:8080
)

fail() {
  echo "services: $*" >&2
  exit 1
}

# binary_for maps a process name to the binary that runs it.
binary_for() {
  case "$1" in
  kubernetes-mcp-*) echo "$bin_dir/kubernetes-mcp" ;;
  *) echo "$bin_dir/$1" ;;
  esac
}

# running reports whether the pid file names a live process started from our
# binary, so a recycled pid is never signalled.
running() {
  local name="$1" pid_file="$pid_dir/$1.pid" pid args
  [[ -f "$pid_file" ]] || return 1
  pid="$(cat "$pid_file")"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  args="$(ps -p "$pid" -o args= 2>/dev/null)" || return 1
  [[ "$args" == *"$(basename "$(binary_for "$name")")"* ]]
}

# ready_code NAME PORT prints 200 when the process is ready. The fake GitHub
# has no health endpoint; an authorize request without parameters answers 400
# once it serves.
ready_code() {
  local code
  if [[ "$1" == fake-github ]]; then
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$2/login/oauth/authorize" 2>/dev/null || true)"
    if [[ "$code" == 400 ]]; then echo 200; else echo "$code"; fi
    return 0
  fi
  curl -s -o /dev/null -w '%{http_code}' --max-time 3 "http://127.0.0.1:$2/health/ready" 2>/dev/null || true
}

# start_one launches one process with its own environment. Only the variables
# listed here are passed (plus PATH/HOME), so the admin KUBECONFIG of the
# calling shell never reaches a service.
start_one() {
  local name="$1" port="$2"
  shift 2
  local binary
  binary="$(binary_for "$name")"
  [[ -x "$binary" ]] || fail "$binary is missing; run make services-build"
  if running "$name"; then
    echo "==> $name already running (pid $(cat "$pid_dir/$name.pid"))"
    return 0
  fi
  echo "==> starting $name on 127.0.0.1:$port"
  local telemetry=()
  local var
  for var in OTEL_EXPORTER_OTLP_ENDPOINT OTEL_TRACES_SAMPLE_RATIO LOG_LEVEL; do
    if [[ -n "${!var:-}" ]]; then telemetry+=("$var=${!var}"); fi
  done
  nohup env -i PATH="$PATH" HOME="${HOME:-/tmp}" \
    HTTP_ADDR="127.0.0.1:$port" \
    "${telemetry[@]}" "$@" "$binary" >"$log_dir/$name.log" 2>&1 </dev/null &
  echo "$!" >"$pid_dir/$name.pid"
  wait_ready "$name" "$port"
}

wait_ready() {
  local name="$1" port="$2" deadline=$((SECONDS + ready_timeout))
  while ((SECONDS < deadline)); do
    if ! running "$name"; then
      echo "services: $name exited during start-up; last log lines:" >&2
      tail -n 20 "$log_dir/$name.log" >&2 || true
      return 1
    fi
    if [[ "$(ready_code "$name" "$port")" == 200 ]]; then
      echo "    $name ready"
      return 0
    fi
    sleep 1
  done
  echo "services: $name not ready after ${ready_timeout}s; last log lines:" >&2
  tail -n 20 "$log_dir/$name.log" >&2 || true
  return 1
}

stop_one() {
  local name="$1" pid_file="$pid_dir/$1.pid" pid deadline
  if ! running "$name"; then
    rm -f "$pid_file"
    return 0
  fi
  pid="$(cat "$pid_file")"
  echo "==> stopping $name (pid $pid)"
  kill -TERM "$pid" 2>/dev/null || true
  deadline=$((SECONDS + stop_timeout))
  while ((SECONDS < deadline)) && kill -0 "$pid" 2>/dev/null; do
    sleep 0.5
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "    $name did not stop in ${stop_timeout}s; sending SIGKILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$pid_file"
}

# The fake GitHub's users and OAuth client (docs/phase7-auth.md). Only the
# bootstrap administrator is passed to the orchestrator; the others start as
# viewers (the outsider is outside the allowed organization).
fake_org=demo-org
fake_client_id=rollback-guard-e2e
fake_users="demo-administrator:1001:$fake_org;demo-operator:1002:$fake_org;demo-viewer:1003:$fake_org;demo-outsider:1004:other-org"
fake_client_secret=""

random_secret() {
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}

start_fake_github() {
  fake_client_secret="$(random_secret)"
  start_one fake-github "$fake_port" FAKE_GITHUB_ADDR="127.0.0.1:$fake_port" \
    FAKE_GITHUB_CLIENT_ID="$fake_client_id" FAKE_GITHUB_CLIENT_SECRET="$fake_client_secret" \
    FAKE_GITHUB_USERS="$fake_users"
}

up() {
  : "${DATABASE_URL:?DATABASE_URL is required}"
  : "${PROMETHEUS_URL:?PROMETHEUS_URL is required}"
  local readonly_kubeconfig="$state_dir/kubeconfig-readonly" rollback_kubeconfig="$state_dir/kubeconfig-rollback"
  [[ -f "$readonly_kubeconfig" && -f "$rollback_kubeconfig" ]] || fail "service-account kubeconfigs missing; run make kubeconfigs"
  mkdir -p "$pid_dir" "$log_dir"
  chmod 700 "$state_dir"
  [[ "$auth_mode" == local || "$auth_mode" == github ]] || fail "DEMO_AUTH must be local or github"
  if running orchestrator && [[ "$(cat "$auth_mode_file" 2>/dev/null || true)" != "$auth_mode" ]]; then
    fail "the orchestrator is running in another auth mode; run make services-down first"
  fi
  echo "$auth_mode" >"$auth_mode_file"
  if [[ "$auth_mode" == local ]]; then rm -f "$state_dir/auth-tokens"; fi

  local common=(ALLOWED_TARGETS="${ALLOWED_TARGETS:-local/rollback-demo/demo-api/demo-api}")
  if [[ -n "${METRICS_WINDOW:-}" ]]; then common+=("METRICS_WINDOW=$METRICS_WINDOW"); fi
  local readonly_url="http://127.0.0.1:8091/mcp" rollback_url="http://127.0.0.1:8092/mcp"
  local observability_url="http://127.0.0.1:8093/mcp" investigator_url="http://127.0.0.1:8094"

  trap 'echo "services: start-up failed; stopping what was started" >&2; down' ERR

  # The orchestrator defaults to AUTH_MODE=github and refuses to start without
  # the GitHub settings, so the no-login demo opts in to local explicitly.
  local auth_env=(AUTH_MODE=local)
  if [[ "$auth_mode" == github ]]; then
    start_fake_github
    auth_env=(AUTH_MODE=github AUTH_GITHUB_CLIENT_ID="$fake_client_id" AUTH_GITHUB_CLIENT_SECRET="$fake_client_secret"
      AUTH_STATE_SECRET="$(random_secret)" AUTH_GITHUB_ALLOWED_ORG="$fake_org"
      AUTH_GITHUB_REDIRECT_URL="http://127.0.0.1:8080/v1/auth/github/callback"
      AUTH_GITHUB_BASE_URL="http://127.0.0.1:$fake_port" AUTH_GITHUB_API_URL="http://127.0.0.1:$fake_port"
      AUTH_BOOTSTRAP_ADMINS="demo-administrator:1001" SESSION_COOKIE_SECURE=false
      # Every demo user signs in from 127.0.0.1, and make e2e followed by
      # make e2e-decline reuses one stack; the production default (10 per
      # client) would throttle that. Loopback demo only.
      AUTH_RATE_LIMIT_PER_MINUTE=600 AUTH_RATE_LIMIT_BURST=100)
  fi

  start_one kubernetes-mcp-readonly 8091 "${common[@]}" \
    KUBERNETES_MCP_MODE=readonly CLUSTER_ID="${CLUSTER_ID:-local}" \
    KUBECONFIG="$(realpath "$readonly_kubeconfig")"
  # The rollback MCP should use the dedicated database role from
  # docs/phase3-mcp.md; ROLLBACK_MCP_DATABASE_URL selects it.
  if [[ -z "${ROLLBACK_MCP_DATABASE_URL:-}" ]]; then
    echo "services: WARNING: ROLLBACK_MCP_DATABASE_URL is not set; the rollback MCP uses DATABASE_URL's role, which can write approvals (demo only, see docs/phase3-mcp.md)" >&2
  fi
  # The rollback MCP re-checks the approver against the same AUTH_MODE as the
  # orchestrator (it has no default and refuses to start without one).
  start_one kubernetes-mcp-rollback 8092 "${common[@]}" \
    KUBERNETES_MCP_MODE=rollback AUTH_MODE="$auth_mode" CLUSTER_ID="${CLUSTER_ID:-local}" \
    KUBECONFIG="$(realpath "$rollback_kubeconfig")" \
    DATABASE_URL="${ROLLBACK_MCP_DATABASE_URL:-$DATABASE_URL}"
  start_one observability-mcp 8093 "${common[@]}" PROMETHEUS_URL="$PROMETHEUS_URL"
  local investigator_env=(DATABASE_URL="$DATABASE_URL" INVESTIGATOR_PUBLIC_URL="$investigator_url"
    KUBERNETES_READ_MCP_URL="$readonly_url" OBSERVABILITY_MCP_URL="$observability_url"
    DIAGNOSIS_STRATEGY="${DIAGNOSIS_STRATEGY:-rule_based}")
  local var
  for var in LLM_BASE_URL LLM_MODEL LLM_API_KEY LLM_TIMEOUT A2A_TASK_TIMEOUT; do
    if [[ -n "${!var:-}" ]]; then investigator_env+=("$var=${!var}"); fi
  done
  start_one investigator 8094 "${common[@]}" "${investigator_env[@]}"
  local orchestrator_env=(DATABASE_URL="$DATABASE_URL" INVESTIGATOR_URL="$investigator_url"
    KUBERNETES_READ_MCP_URL="$readonly_url" KUBERNETES_ROLLBACK_MCP_URL="$rollback_url"
    OBSERVABILITY_MCP_URL="$observability_url")
  for var in A2A_TASK_TIMEOUT RECOVERY_TIMEOUT RECOVERY_POLL_INTERVAL; do
    if [[ -n "${!var:-}" ]]; then orchestrator_env+=("$var=${!var}"); fi
  done
  start_one orchestrator 8080 "${common[@]}" "${orchestrator_env[@]}" "${auth_env[@]}"
  if [[ "$auth_mode" == github ]]; then
    ORCHESTRATOR_URL="http://127.0.0.1:8080" DEMO_STATE_DIR="$state_dir" bash "$(dirname "$0")/auth-login.sh" all
  fi
  trap - ERR
  echo "all services ready; logs in $log_dir"
}

down() {
  local index name
  for ((index = ${#services[@]} - 1; index >= 0; index--)); do
    name="${services[index]%%:*}"
    stop_one "$name"
  done
  stop_one fake-github
}

status() {
  local entry name port state code failed=0
  printf '%-26s %-8s %-8s %s\n' SERVICE PID STATE READY
  for entry in "${services[@]}"; do
    name="${entry%%:*}"
    port="${entry##*:}"
    if running "$name"; then
      state=running
      code="$(ready_code "$name" "$port")"
      printf '%-26s %-8s %-8s %s\n' "$name" "$(cat "$pid_dir/$name.pid")" "$state" "$code"
      [[ "$code" == 200 ]] || failed=1
    else
      printf '%-26s %-8s %-8s %s\n' "$name" - stopped -
      failed=1
    fi
  done
  if [[ "$(cat "$auth_mode_file" 2>/dev/null || true)" == github ]]; then
    if running fake-github; then
      code="$(ready_code fake-github "$fake_port")"
      printf '%-26s %-8s %-8s %s\n' fake-github "$(cat "$pid_dir/fake-github.pid")" running "$code"
      [[ "$code" == 200 ]] || failed=1
    else
      printf '%-26s %-8s %-8s %s\n' fake-github - stopped -
      failed=1
    fi
  fi
  return "$failed"
}

case "$action" in
up) up ;;
down) down ;;
status) status ;;
*) fail "unknown action $action (want up, down, or status)" ;;
esac
