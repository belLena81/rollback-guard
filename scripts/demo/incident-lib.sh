# Shared by the REST demo scripts (create-incident, approve-latest-action,
# verify, e2e). Source it; it defines functions and variables only.
#
# State: $DEMO_STATE_DIR/last-incident holds KEY=VALUE lines (INCIDENT_ID,
# CREATE_TRACE_ID, APPROVE_TRACE_ID, ...). It holds no secrets. Values are
# read with a fixed parser, never sourced.
# shellcheck shell=bash

DEMO_STATE_DIR=${DEMO_STATE_DIR:-.demo}
ORCHESTRATOR_URL=${ORCHESTRATOR_URL:-http://127.0.0.1:8080}
STATE_FILE="$DEMO_STATE_DIR/last-incident"
TARGET_CLUSTER=${TARGET_CLUSTER:-local}
DEMO_NAMESPACE=${DEMO_NAMESPACE:-rollback-demo}
DEMO_DEPLOYMENT=${DEMO_DEPLOYMENT:-demo-api}
TARGET_SERVICE=${TARGET_SERVICE:-demo-api}
METRICS_WINDOW=${METRICS_WINDOW:-30s}

die() {
  echo "$SCRIPT_NAME: FAIL: $*" >&2
  exit 1
}

require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
  done
}

# Identifier-shaped values only, so nothing read back from the API or the
# state file can smuggle shell or URL syntax into a later request.
valid_id() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$ ]]
}

state_get() {
  [[ -f "$STATE_FILE" ]] || return 0
  local key="$1" line
  while IFS= read -r line; do
    if [[ "$line" == "$key="* ]]; then
      echo "${line#*=}"
      return 0
    fi
  done <"$STATE_FILE"
}

state_set() {
  local key="$1" value="$2" tmp
  valid_id "$value" || die "refusing to store a non-identifier value for $key"
  mkdir -p "$DEMO_STATE_DIR"
  tmp="$(mktemp "$DEMO_STATE_DIR/.state.XXXXXX")"
  if [[ -f "$STATE_FILE" ]]; then
    grep -v "^$key=" "$STATE_FILE" >"$tmp" || true
  fi
  echo "$key=$value" >>"$tmp"
  mv "$tmp" "$STATE_FILE"
}

# incident_id: INCIDENT_ID from the environment, else the last created one.
incident_id() {
  local id="${INCIDENT_ID:-$(state_get INCIDENT_ID)}"
  [[ -n "$id" ]] || die "no incident: run make create-incident or set INCIDENT_ID"
  valid_id "$id" || die "INCIDENT_ID is not an identifier"
  echo "$id"
}

# new_trace_id prints 32 random lowercase hex characters (a W3C trace ID).
new_trace_id() {
  od -An -N16 -tx1 /dev/urandom | tr -d ' \n'
}

new_span_id() {
  od -An -N8 -tx1 /dev/urandom | tr -d ' \n'
}

# api METHOD PATH [BODY] [TRACE_ID]: sets API_STATUS and API_BODY. A trace ID
# is sent as a sampled W3C traceparent so the request's trace can be read
# back from the trace backend by ID.
API_STATUS=""
API_BODY=""
api() {
  local method="$1" path="$2" body="${3:-}" trace_id="${4:-}" out
  local args=(-sS -X "$method" -H 'Accept: application/json' --max-time "${API_TIMEOUT:-30}" -o - -w '\n%{http_code}')
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' --data-binary "$body")
  fi
  if [[ -n "$trace_id" ]]; then
    args+=(-H "traceparent: 00-$trace_id-$(new_span_id)-01")
  fi
  out="$(curl "${args[@]}" "$ORCHESTRATOR_URL$path")" || die "$method $path: orchestrator unreachable at $ORCHESTRATOR_URL"
  API_STATUS="${out##*$'\n'}"
  API_BODY="${out%$'\n'*}"
}

get_incident() {
  api GET "/v1/incidents/$1"
  [[ "$API_STATUS" == 200 ]] || die "GET incident $1 returned $API_STATUS: $(error_summary)"
  echo "$API_BODY"
}

# error_summary prints the fixed error code and message of an API error body.
error_summary() {
  jq -r '"\(.error // "unknown"): \(.message // "")"' <<<"$API_BODY" 2>/dev/null || echo "unparseable body"
}

# prom_query EXPR prints the first sample value of an instant query, or
# nothing when the result is empty.
prom_query() {
  : "${PROMETHEUS_URL:?PROMETHEUS_URL is required}"
  curl -sS --max-time 10 -G "$PROMETHEUS_URL/api/v1/query" --data-urlencode "query=$1" |
    jq -r 'if .status == "success" then (.data.result[0].value[1] // empty) else error("query failed") end'
}

demo_selector() {
  echo "cluster=\"$TARGET_CLUSTER\",namespace=\"$DEMO_NAMESPACE\",deployment=\"$DEMO_DEPLOYMENT\",service=\"$TARGET_SERVICE\""
}

# The observability MCP's error-rate query (docs/phase6-kind.md, "Verification").
error_rate_query() {
  local selector total errors
  selector="$(demo_selector)"
  total="sum(rate(rollback_guardian_http_requests_total{$selector}[$METRICS_WINDOW]))"
  errors="sum(rate(rollback_guardian_http_requests_total{$selector,code=~\"5..\"}[$METRICS_WINDOW]))"
  echo "($errors or ($total * 0)) / ($total > 0)"
}

request_rate_query() {
  echo "sum(rate(rollback_guardian_http_requests_total{$(demo_selector)}[$METRICS_WINDOW]))"
}

k() {
  "${KUBECTL:-kubectl}" --context "${KUBE_CONTEXT:?KUBE_CONTEXT is required}" -n "$DEMO_NAMESPACE" "$@"
}
