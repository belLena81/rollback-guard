#!/usr/bin/env bash
# Approves the rollback proposal of the latest incident (or INCIDENT_ID) as
# the local human actor, through POST /v1/incidents/{id}/approve.
#
# The action ID and proposal hash are taken verbatim from the incident's
# current proposal as the API returns it. This script never computes,
# derives, or guesses a hash: if there is no proposal, it stops.
#
# The orchestrator executes and verifies within the same request, so this
# waits for the rollout watch and recovery check (API_TIMEOUT, default 900s, the server write timeout).
#
# Exit codes: 0 approved (incident resolved or failed: see the printed
# status), 1 refused or failed, 3 rollback outcome unknown (retry the same
# approval; see the message).
set -euo pipefail
SCRIPT_NAME=approve-latest-action
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools curl jq od

id="$(incident_id)"
summary="$(get_incident "$id")"
status="$(jq -r '.incident.status' <<<"$summary")"
case "$status" in
awaiting_approval) ;;
executing | verifying)
  # A previous approval of this proposal was interrupted: the rollback
  # outcome was unknown (executing) or recovery verification did not finish
  # (verifying). Re-sending the same approval reuses its idempotency key, so
  # the orchestrator reconciles the attempt instead of mutating twice, and
  # then resumes verification.
  echo "$SCRIPT_NAME: incident $id is $status; re-sending the recorded approval to resume it"
  ;;
*) die "incident $id is $status, not awaiting_approval; nothing to approve" ;;
esac

action_id="$(jq -r '.proposal.actionId // empty' <<<"$summary")"
proposal_hash="$(jq -r '.proposal.proposalHash // empty' <<<"$summary")"
[[ -n "$action_id" && -n "$proposal_hash" ]] || die "incident $id has no rollback proposal"
valid_id "$action_id" || die "the proposal action ID is not an identifier"
valid_id "$proposal_hash" || die "the proposal hash is not an identifier"
if [[ "$status" != awaiting_approval ]]; then
  [[ "$(jq -r '.approval.actionId // empty' <<<"$summary")" == "$action_id" &&
    "$(jq -r '.approval.proposalHash // empty' <<<"$summary")" == "$proposal_hash" ]] ||
    die "incident $id is $status but its recorded approval does not match the proposal"
fi

echo "$SCRIPT_NAME: approving incident $id action $action_id proposal $proposal_hash"
echo "$SCRIPT_NAME: proposal expires $(jq -r '.proposal.expiresAt' <<<"$summary"); current $(jq -r '.proposal.currentRevision' <<<"$summary") -> target $(jq -r '.proposal.targetRevision' <<<"$summary")"

body="$(jq -nc --arg actionId "$action_id" --arg proposalHash "$proposal_hash" '{actionId: $actionId, proposalHash: $proposalHash}')"
trace_id="$(new_trace_id)"
state_set APPROVE_TRACE_ID "$trace_id"
API_AS="${APPROVE_AS-$(auth_user administrator)}" API_TIMEOUT="${API_TIMEOUT:-900}" api POST "/v1/incidents/$id/approve" "$body" "$trace_id"

if [[ "$API_STATUS" == 200 ]]; then
  echo "$SCRIPT_NAME: approved; incident $id is now $(jq -r '.incident.status' <<<"$API_BODY") (execution $(jq -r '.execution.outcome // "none"' <<<"$API_BODY"), verification passed=$(jq -r 'if .verification then .verification.passed else "none" end' <<<"$API_BODY"))"
  exit 0
fi

code="$(jq -r '.error // empty' <<<"$API_BODY" 2>/dev/null || true)"
if [[ "$code" == rollback_outcome_unknown ]]; then
  cat >&2 <<EOF
$SCRIPT_NAME: the rollback outcome is UNKNOWN (HTTP $API_STATUS, request $(jq -r '.requestId // "-"' <<<"$API_BODY")).
  The rollback request may or may not have reached the cluster. Incident $id
  stays 'executing' with its execution attempt pending; nothing was recorded
  as failed. Do NOT create a new incident or roll back by hand.
  Retry the same approval; it reuses the same idempotency key, so the
  orchestrator reconciles the attempt and mutates at most once:

    make approve-latest-action INCIDENT_ID=$id
EOF
  exit 3
fi
die "approve returned $API_STATUS: $(error_summary)"
