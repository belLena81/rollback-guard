#!/usr/bin/env bash
# Fails if any session token from the e2e token file appears in the given
# files or directories (service logs, captured e2e output). Run it after
# make e2e / e2e-decline, before logs are uploaded as artifacts.
#
#   check-no-tokens.sh PATH...
#
# The tokens are handed to grep through a private temporary pattern file,
# never on a command line, and are never printed; only matching file names are.
set -euo pipefail
SCRIPT_NAME=check-no-tokens
# shellcheck source=scripts/demo/incident-lib.sh
source "$(dirname "$0")/incident-lib.sh"
require_tools grep cut mktemp

(($# > 0)) || die "usage: check-no-tokens.sh PATH..."
[[ -s "$AUTH_TOKENS_FILE" ]] || die "no session tokens in $AUTH_TOKENS_FILE; nothing to check against"

umask 077
patterns="$(mktemp)"
trap 'rm -f "$patterns"' EXIT
cut -d= -f2- "$AUTH_TOKENS_FILE" >"$patterns"
count="$(grep -c . "$patterns" || true)"
((count > 0)) || die "the token file holds no tokens"

existing=()
for path in "$@"; do
  if [[ -e "$path" ]]; then existing+=("$path"); fi
done
((${#existing[@]} > 0)) || die "none of the given paths exist"

if leaks="$(grep -rlFf "$patterns" -- "${existing[@]}")"; then
  die "a session token appears in: $leaks"
fi
echo "$SCRIPT_NAME: none of $count session tokens appears in ${existing[*]}"
