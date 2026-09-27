#!/usr/bin/env bash
# e2e-summary.sh — decide whether one `go test` pass of the e2e suites passed.
#
# Usage: e2e-summary.sh <title> <go-test-log> <go-test-exit-status>
#
# Why (setec#22, setec#298)
# -------------------------
# `go test` exits 0 when every scenario SKIPS, and when the -run pattern
# matches no test at all. Both read as a green e2e run that verified
# nothing. The old job printed a warning on a SKIP, and the warning was read
# as a pass. So this script fails the pass in three cases:
#
#   1. go test exited non-zero.
#   2. Any scenario or subtest reports `--- SKIP`. A skipped scenario is not
#      a verified one. Exclude a known gap with `go test -skip`, where the
#      workflow names it, never by tolerating a SKIP here.
#   3. No scenario reports `--- PASS`. The pattern matched nothing, or the
#      suite died before its first test.
#
# It also writes the PASS/FAIL/SKIP list to the job summary, so a reader
# sees what ran without opening the log.
#
# Tested by scripts/__tests__/e2e-summary.test.sh.

set -uo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <title> <go-test-log> <go-test-exit-status>" >&2
  exit 2
fi

title="$1"
log="$2"
status="$3"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

if [ ! -f "$log" ]; then
  echo "::error::${title}: no go test log at ${log}"
  exit 1
fi

results_re='^[[:space:]]*--- (PASS|FAIL|SKIP)'

{
  echo "### ${title}"
  echo ''
  echo '```'
  grep -E "$results_re" "$log" || echo '(no test results in the log)'
  echo '```'
} >>"$summary"

rc=0

if [ "$status" -ne 0 ]; then
  echo "::error::${title}: go test exited ${status}"
  rc=1
fi

if grep -Eq '^[[:space:]]*--- SKIP' "$log"; then
  echo "::error::${title}: one or more scenarios SKIPPED. A skipped scenario is not verified:"
  grep -E '^[[:space:]]*--- SKIP' "$log"
  rc=1
fi

if ! grep -Eq '^[[:space:]]*--- PASS' "$log"; then
  echo "::error::${title}: no scenario passed. The -run pattern matched nothing, or the suite died before its first test."
  rc=1
fi

exit "$rc"
