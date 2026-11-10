#!/usr/bin/env bash
# Sends steady GET /work traffic to demo-api so Prometheus has request-rate
# series to evaluate. Non-2xx responses are expected for broken versions and
# are counted, not treated as failures. Fails only if nothing answered at all.
#
# Usage: generate-load.sh <base-url> <duration-seconds> <concurrency>
set -euo pipefail

base_url=${1:?base url required}
duration=${2:?duration seconds required}
concurrency=${3:?concurrency required}
curl=${CURL:-curl}

out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
# Stopping the script (make e2e stops it when the scenario ends) must stop the
# background workers too; otherwise they keep sending load until their own
# deadline.
stop_workers() {
  local pids
  pids=$(jobs -p)
  if [[ -n "$pids" ]]; then
    # shellcheck disable=SC2086 # one PID per word
    kill $pids 2>/dev/null || true
  fi
  exit 143
}
trap stop_workers TERM INT

worker() {
  local deadline=$((SECONDS + duration))
  while ((SECONDS < deadline)); do
    "$curl" -s -o /dev/null --max-time 5 -w '%{http_code}\n' "$base_url/work" || echo 000
    sleep 0.05
  done >"$out/$1"
}

echo "generate-load: $concurrency workers for ${duration}s against $base_url/work"
for ((i = 0; i < concurrency; i++)); do
  worker "$i" &
done
wait

summary=$(cat "$out"/* | sort | uniq -c | sort -rn)
echo "generate-load: responses by HTTP status (000 = no response):"
echo "$summary"
if ! grep -qv '^000$' <(cat "$out"/*); then
  echo "generate-load: demo-api did not answer at $base_url" >&2
  exit 1
fi
