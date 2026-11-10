#!/usr/bin/env bash
# Runs `go test -count=1 -v "$@"` and fails if any test fails, if any test or
# subtest is skipped, or if no test ran at all. A suite that skips proves
# nothing, so a gate must not report it as passed.
#
#   scripts/ci/test-no-skips.sh -tags=integration ./internal/adapters/postgres/...
#
# REQUIRED_TESTS (optional, space-separated top-level test names) must each
# have passed. Use it when the package also holds tests that run without the
# build tag, so a missing tag or deleted file cannot pass on those alone.
set -uo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: [REQUIRED_TESTS='TestA TestB'] $0 <go test arguments>" >&2
  exit 2
fi

log="$(mktemp)"
trap 'rm -f "$log"' EXIT

go test -count=1 -v "$@" 2>&1 | tee "$log"
status="${PIPESTATUS[0]}"

# `grep -c` prints the count and exits 1 when it is zero.
skips="$(grep -Ec '^[[:space:]]*--- SKIP' "$log" || true)"
passes="$(grep -Ec '^[[:space:]]*--- PASS' "$log" || true)"

if [ "$status" -ne 0 ]; then
  echo "test-no-skips: go test failed (exit $status)" >&2
  exit "$status"
fi
if [ "$skips" -ne 0 ]; then
  echo "test-no-skips: $skips skipped test(s) count as a failure:" >&2
  grep -E '^[[:space:]]*--- SKIP' "$log" >&2
  exit 1
fi
if [ "$passes" -eq 0 ]; then
  echo "test-no-skips: no test ran" >&2
  exit 1
fi
missing=0
for name in ${REQUIRED_TESTS:-}; do
  if ! grep -Eq "^--- PASS: ${name} " "$log"; then
    echo "test-no-skips: required test ${name} did not run and pass" >&2
    missing=1
  fi
done
if [ "$missing" -ne 0 ]; then
  exit 1
fi
echo "test-no-skips: $passes passed, 0 skipped"
