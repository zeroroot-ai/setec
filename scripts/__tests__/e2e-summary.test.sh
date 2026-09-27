#!/usr/bin/env bash
# Mutation tests for scripts/e2e-summary.sh.
#
# The script decides whether an e2e pass is green. The defect it exists to
# stop is a pass that reported green and verified nothing: every scenario
# skipped, or the -run pattern matched no test. Each case below feeds it one
# such log and REQUIRES a red. The pass cases prove it does not fail every
# log it sees.
#
# Run by the `ci-required-selftest` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/e2e-summary.test.sh`.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUMMARY="${HERE}/e2e-summary.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0

# assert <description> <expect pass|fail> <go test exit status> <log text>
assert() {
  local desc="$1" expect="$2" status="$3" text="$4" result
  printf '%s\n' "$text" >"${TMP}/log"
  if GITHUB_STEP_SUMMARY="${TMP}/summary" bash "$SUMMARY" "t" "${TMP}/log" "$status" >/dev/null 2>&1; then
    result="pass"
  else
    result="fail"
  fi
  if [ "$result" = "$expect" ]; then
    echo "ok   ${desc} -> ${result}"
    PASS=$((PASS + 1))
  else
    echo "FAIL ${desc} -> ${result} (expected ${expect})"
    FAIL=$((FAIL + 1))
  fi
}

# Controls: the logs that must pass.
assert "every scenario passed" pass 0 "=== RUN   TestA
--- PASS: TestA (1.00s)
=== RUN   TestB
--- PASS: TestB (2.00s)
PASS"

assert "subtests passed" pass 0 "=== RUN   TestA
=== RUN   TestA/backend=kata-fc
    --- PASS: TestA/backend=kata-fc (1.00s)
--- PASS: TestA (1.00s)
PASS"

# Mutations: each one must fail.
assert "go test exited non-zero" fail 1 "--- PASS: TestA (1.00s)
FAIL"

assert "a scenario failed" fail 1 "--- PASS: TestA (1.00s)
--- FAIL: TestB (1.00s)
FAIL"

assert "a scenario skipped, go test exited 0" fail 0 "--- PASS: TestA (1.00s)
--- SKIP: TestB (0.00s)
PASS"

assert "a subtest skipped, go test exited 0" fail 0 "=== RUN   TestA
    --- SKIP: TestA/backend=kata-fc (0.00s)
--- PASS: TestA (1.00s)
PASS"

assert "every scenario skipped" fail 0 "--- SKIP: TestA (0.00s)
--- SKIP: TestB (0.00s)
PASS"

assert "the -run pattern matched nothing" fail 0 "testing: warning: no tests to run
PASS"

assert "the suite died in TestMain" fail 1 "e2e: preflight failed: RuntimeClass \"kata-fc\" not found
FAIL"

# A missing log is a failure, not a pass.
if GITHUB_STEP_SUMMARY="${TMP}/summary" bash "$SUMMARY" "t" "${TMP}/absent" 0 >/dev/null 2>&1; then
  echo "FAIL a missing log -> pass (expected fail)"
  FAIL=$((FAIL + 1))
else
  echo "ok   a missing log -> fail"
  PASS=$((PASS + 1))
fi

echo "${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
