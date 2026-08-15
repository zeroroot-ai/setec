#!/usr/bin/env bash
# Mutation tests for scripts/ci-required-gate.sh and
# scripts/check-ci-required-coverage.sh.
#
# `ci-required` is THE required status check for this repo — the single thing
# standing between a red gate and `main`. The defect class it exists to fix is
# a check that ran, reported, and decided nothing.
# So the aggregator's own tests are written as mutations: each one breaks
# something specific and REQUIRES a red. The pass-cases are only here to prove
# it is not failing indiscriminately.
#
# Run by the `ci-required-selftest` job in .github/workflows/go-ci.yml, and
# locally with `bash scripts/__tests__/ci-required-gate.test.sh`.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="${HERE}/ci-required-gate.sh"
COVERAGE="${HERE}/check-ci-required-coverage.sh"

PASS=0
FAIL=0

# results '<job>:<result>' ... -> the JSON shape GitHub's toJSON(needs) produces.
results() {
  local out="{}" pair job res
  for pair in "$@"; do
    job="${pair%%:*}"; res="${pair##*:}"
    out="$(jq -c --arg j "$job" --arg r "$res" '. + {($j): {result: $r, outputs: {}}}' <<<"$out")"
  done
  printf '%s' "$out"
}

assert_gate() {
  local desc="$1" expect="$2" json="$3" result
  if GATE_RESULTS="$json" bash "$GATE" >/dev/null 2>&1; then result="pass"; else result="fail"; fi
  if [ "$result" = "$expect" ]; then
    echo "✅ ${desc} → ${result} (expected ${expect})"
    PASS=$((PASS + 1))
  else
    echo "❌ ${desc} → ${result} (expected ${expect})"
    GATE_RESULTS="$json" bash "$GATE" 2>&1 | sed 's/^/      /'
    FAIL=$((FAIL + 1))
  fi
}

echo "--- aggregation semantics (scripts/ci-required-gate.sh) ---"

# Controls: the shapes that must NOT block a merge.
assert_gate "all gates succeeded" pass \
  "$(results fast:success lint:success heavy:success)"

# Lane splitting means most gates are skipped on any given run: `fast` is
# pull_request-only, `heavy` is merge_group-only. GitHub reports a skipped job
# as a skipped check run, which satisfies a ruleset, so the aggregator must
# agree — otherwise every single PR fails.
assert_gate "mixed success and skipped (normal PR lane)" pass \
  "$(results fast:success heavy:skipped lint:success binary-smoke-test:success)"

assert_gate "every gate skipped" pass \
  "$(results fast:skipped heavy:skipped lint:skipped)"

# Mutations: each must turn it red.
assert_gate "MUTATION one gate failed" fail \
  "$(results fast:success lint:failure heavy:skipped)"

assert_gate "MUTATION the only non-skipped gate failed" fail \
  "$(results fast:failure heavy:skipped lint:skipped)"

# A queue eviction or timeout cancels siblings. Treating cancelled as "not a
# failure" would let a half-run suite report green.
assert_gate "MUTATION one gate cancelled" fail \
  "$(results fast:success lint:cancelled)"

assert_gate "MUTATION every gate failed" fail \
  "$(results fast:failure lint:failure)"

# An empty needs: list is the purest form of "guard that cannot fail" — it
# reports green having gated nothing at all.
assert_gate "MUTATION empty needs (no gates at all)" fail "{}"

# An unset GATE_RESULTS means the workflow wiring was changed and toJSON(needs)
# is no longer being passed. Fail closed.
if bash "$GATE" >/dev/null 2>&1; then r="pass"; else r="fail"; fi
if [ "$r" = "fail" ]; then
  echo "✅ MUTATION GATE_RESULTS unset → fail (expected fail)"; PASS=$((PASS + 1))
else
  echo "❌ MUTATION GATE_RESULTS unset → pass (expected fail)"; FAIL=$((FAIL + 1))
fi

# A result string GitHub does not document (or a typo in a hand-built map)
# must fail closed rather than fall through a case statement as "not a failure".
assert_gate "MUTATION unrecognised result value" fail \
  "$(results fast:success lint:borked)"

echo
echo "--- needs: coverage (scripts/check-ci-required-coverage.sh) ---"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

write_wf() { cat > "${WORK}/wf.yml"; }

assert_cov() {
  local desc="$1" expect="$2" result
  if bash "$COVERAGE" "${WORK}/wf.yml" >/dev/null 2>&1; then result="pass"; else result="fail"; fi
  if [ "$result" = "$expect" ]; then
    echo "✅ ${desc} → ${result} (expected ${expect})"
    PASS=$((PASS + 1))
  else
    echo "❌ ${desc} → ${result} (expected ${expect})"
    bash "$COVERAGE" "${WORK}/wf.yml" 2>&1 | sed 's/^/      /'
    FAIL=$((FAIL + 1))
  fi
}

write_wf <<'YAML'
on: {pull_request: null, merge_group: null}
jobs:
  changes: {runs-on: ubuntu-latest}
  fast: {runs-on: ubuntu-latest}
  lint: {runs-on: ubuntu-latest}
  ci-required:
    needs: [fast, lint]
    runs-on: ubuntu-latest
YAML
assert_cov "every gate covered (changes exempt)" pass

# THE regression this script exists for: someone adds a gate and forgets the
# aggregator. The gate runs, can go red, and gates nothing.
write_wf <<'YAML'
on: {pull_request: null, merge_group: null}
jobs:
  fast: {runs-on: ubuntu-latest}
  lint: {runs-on: ubuntu-latest}
  brand-new-gate: {runs-on: ubuntu-latest}
  ci-required:
    needs: [fast, lint]
    runs-on: ubuntu-latest
YAML
assert_cov "MUTATION a gate job missing from needs" fail

# A stale name in needs: fails the whole workflow at parse time on GitHub,
# presenting as infrastructure breakage rather than as a gate failure.
write_wf <<'YAML'
on: {pull_request: null, merge_group: null}
jobs:
  fast: {runs-on: ubuntu-latest}
  ci-required:
    needs: [fast, deleted-job]
    runs-on: ubuntu-latest
YAML
assert_cov "MUTATION needs names a job that does not exist" fail

write_wf <<'YAML'
on: {pull_request: null, merge_group: null}
jobs:
  fast: {runs-on: ubuntu-latest}
  lint: {runs-on: ubuntu-latest}
YAML
assert_cov "MUTATION aggregator job deleted entirely" fail

write_wf <<'YAML'
on: {pull_request: null, merge_group: null}
jobs:
  fast: {runs-on: ubuntu-latest}
  ci-required:
    needs: []
    runs-on: ubuntu-latest
YAML
assert_cov "MUTATION needs emptied" fail

echo
echo "passed: ${PASS}  failed: ${FAIL}"
[ "$FAIL" -eq 0 ]
