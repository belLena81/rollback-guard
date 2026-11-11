#!/usr/bin/env bash
# Signs the fake GitHub users in through the orchestrator's real OAuth login
# and keeps their session tokens (docs/phase7-auth.md, "End-to-end with a real
# session").
#
#   auth-login.sh all             administrator, operator and viewer sessions
#                                 (the administrator is the bootstrap admin;
#                                 it then promotes the operator)
#   auth-login.sh login NAME [KEY]  log NAME in and store the session as KEY
#                                 (default NAME); NAME is administrator,
#                                 operator, viewer or outsider
#   auth-login.sh probe NAME      log NAME in and print "STATUS session|nosession"
#                                 without storing anything
#
# Each login follows the real flow: GET /v1/auth/github/login (302 to the fake
# GitHub with the state cookie), GET that authorize URL with the fake's login
# hint (302 back with a code), GET the callback with the state cookie. The
# tokens go to $AUTH_TOKENS_FILE (0600, under the gitignored .demo/). Nothing
# secret is printed: URLs carrying state or a code and the response body go to
# curl through stdin or a variable, and only HTTP status codes are reported.
#
# NAME maps to the fake GitHub login demo-NAME (users in services.sh).
set -euo pipefail
SCRIPT_NAME=auth-login
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools curl jq awk mktemp

umask 077
jar=""
cleanup() { if [[ -n "$jar" ]]; then rm -f "$jar"; fi; }
trap cleanup EXIT

# secret_curl URL [curl args...]: the URL (state, code) is read from stdin.
secret_curl() {
  local url="$1"
  shift
  [[ "$url" =~ ^http://127\.0\.0\.1:[0-9]+/[A-Za-z0-9._~:/?\&=%+-]*$ ]] || die "refusing a redirect that is not a loopback URL"
  printf 'url = "%s"\n' "$url" | curl -sS --max-time 20 -K - "$@"
}

# location_of prints the Location header of a curl -D - response.
location_of() {
  awk 'tolower($1) == "location:" { sub(/\r$/, "", $2); print $2; exit }'
}

LOGIN_STATUS=""
LOGIN_TOKEN=""
# oauth_login NAME sets LOGIN_STATUS (the callback's HTTP status) and, on 200,
# LOGIN_TOKEN.
oauth_login() {
  local name="$1" authorize_url callback_url out body
  [[ "$name" =~ ^[a-z]+$ ]] || die "invalid user name"
  jar="$(mktemp)"
  authorize_url="$(curl -sS --max-time 20 -o /dev/null -D - -c "$jar" "$ORCHESTRATOR_URL/v1/auth/github/login" | location_of)"
  [[ -n "$authorize_url" ]] || die "the login endpoint did not redirect; is the stack running with DEMO_AUTH=github?"
  callback_url="$(secret_curl "$authorize_url&login=demo-$name" -o /dev/null -D - | location_of)"
  [[ -n "$callback_url" ]] || die "the fake GitHub did not redirect back for demo-$name"
  out="$(secret_curl "$callback_url" -b "$jar" -c "$jar" -o - -w '\n%{http_code}')"
  LOGIN_STATUS="${out##*$'\n'}"
  body="${out%$'\n'*}"
  LOGIN_TOKEN=""
  if [[ "$LOGIN_STATUS" == 200 ]]; then
    LOGIN_TOKEN="$(jq -r '.token // empty' <<<"$body")"
  fi
  rm -f "$jar"
  jar=""
}

# store_token KEY TOKEN: replaces KEY in the 0600 token file atomically.
store_token() {
  local key="$1" token="$2" tmp
  [[ "$key" =~ ^[a-z-]+$ ]] || die "invalid token key"
  mkdir -p "$DEMO_STATE_DIR"
  chmod 700 "$DEMO_STATE_DIR"
  tmp="$(mktemp "$DEMO_STATE_DIR/.tokens.XXXXXX")"
  if [[ -f "$AUTH_TOKENS_FILE" ]]; then
    grep -v "^$key=" "$AUTH_TOKENS_FILE" >"$tmp" || true
  fi
  echo "$key=$token" >>"$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$AUTH_TOKENS_FILE"
}

login_and_store() {
  local name="$1" key="${2:-$1}"
  oauth_login "$name"
  [[ "$LOGIN_STATUS" == 200 && -n "$LOGIN_TOKEN" ]] || die "login of demo-$name returned $LOGIN_STATUS, want 200 with a session"
  store_token "$key" "$LOGIN_TOKEN"
  LOGIN_TOKEN=""
  echo "$SCRIPT_NAME: demo-$name signed in (session stored as '$key')"
}

action="${1:?usage: auth-login.sh all|login NAME [KEY]|probe NAME}"
case "$action" in
all)
  rm -f "$AUTH_TOKENS_FILE"
  login_and_store administrator
  login_and_store operator
  login_and_store viewer
  # New organization members are viewers; the bootstrap administrator grants
  # the operator role through the real admin endpoint.
  API_AS=operator api GET /v1/me
  [[ "$API_STATUS" == 200 ]] || die "GET /v1/me as the operator returned $API_STATUS"
  actor_id="$(jq -r '.actorId' <<<"$API_BODY")"
  valid_id "$actor_id" || die "the API returned no usable actor ID"
  API_AS=administrator api PATCH "/v1/users/$actor_id/role" '{"role":"operator"}'
  [[ "$API_STATUS" == 200 ]] || die "promoting demo-operator returned $API_STATUS: $(error_summary)"
  echo "$SCRIPT_NAME: demo-operator promoted to operator by the bootstrap administrator"
  ;;
login) login_and_store "${2:?NAME is required}" "${3:-}" ;;
probe)
  oauth_login "${2:?NAME is required}"
  if [[ -n "$LOGIN_TOKEN" ]]; then echo "$LOGIN_STATUS session"; else echo "$LOGIN_STATUS nosession"; fi
  ;;
*) die "unknown action $action" ;;
esac
