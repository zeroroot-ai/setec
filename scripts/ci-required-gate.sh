#!/usr/bin/env bash
# Aggregation logic for the `ci-required` job — the single required status
# check for this repo.
#
# Why an aggregator at all
# ------------------------
# The merge queue's required_status_checks used to name two jobs out of the
# twenty that run. `Unit + envtest`, `Proto WIRE-breaking gate`, `Manifests
# up-to-date`, `Helm lint`, `Docker build`, the examples suites and CodeQL all
# ran, could all report red, and none of them could stop a merge. The same
# class was demonstrated live in sibling repos: gibson-executor#369 (a PR
# merged with lint red) and deploy#1526 (merged with its own `validate` job
# red).
#
# Widening the required list is not the fix — the list lives in a DIFFERENT
# repo (zeroroot-ai/.github `rulesets/repo/*.json`) from the workflows it
# names, so it silently falls behind every time a gate is added here. Instead
# one job `needs:` every gate, and that one job is what the ruleset requires.
# Adding a gate now means adding it to `needs:` in the same PR as the gate.
#
# Why this is a script and not five lines of inline YAML
# -----------------------------------------------------
# Because the semantics are easy to get wrong in ways that produce a guard that
# cannot fail, and a script can be mutation-tested (see
# scripts/__tests__/ci-required-gate.test.sh). Specifically:
#
#   * The job MUST carry `if: always()`. GitHub skips a `needs:` job when ANY
#     dependency is skipped, so without it the aggregator vanishes precisely
#     when a gate was skipped — and an absent required context freezes
#     merge-queue entry rather than failing it.
#
#   * `always()` with no result evaluation is a job that passes whenever it
#     runs. That is the "guard that cannot fail" class -- the check exists, is
#     described as authoritative, and is arranged so it never decides anything.
#     The results must be read.
#
#   * `skipped` counts as PASS. That is deliberate and load-bearing: gates are
#     lane-split (`fast` on pull_request, `heavy`/`test` on merge_group), so on
#     any given run most gates legitimately do not execute.
#     GitHub reports a skipped job as a *skipped check run*, which satisfies a
#     ruleset — the aggregator must agree with that, or every PR fails.
#
#   * `failure` and `cancelled` count as FAIL. Cancelled matters: a queue
#     eviction or a timeout cancels sibling jobs, and treating that as "not a
#     failure" would let a half-run suite report green.
#
# Input: $GATE_RESULTS, the workflow's `${{ toJSON(needs) }}`. Reading the
# needs context wholesale — rather than restating each job name here — is what
# makes it impossible for this script to silently ignore a dependency that IS
# listed in `needs:`. The complementary risk, a gate job that is not listed in
# `needs:` at all, is covered by scripts/check-ci-required-coverage.sh.
#
# Exit 0 = every gate passed or was skipped. Exit 1 = at least one did not.

set -uo pipefail

RESULTS="${GATE_RESULTS:-}"

if [ -z "$RESULTS" ]; then
  echo "::error::GATE_RESULTS is empty — expected \${{ toJSON(needs) }}."
  echo "An aggregator with nothing to aggregate must fail, not pass."
  exit 1
fi

if ! jq -e 'type == "object"' >/dev/null 2>&1 <<<"$RESULTS"; then
  echo "::error::GATE_RESULTS is not a JSON object: ${RESULTS}"
  exit 1
fi

count="$(jq -r 'length' <<<"$RESULTS")"
if [ "$count" -eq 0 ]; then
  echo "::error::GATE_RESULTS has no entries — the ci-required job has an empty"
  echo "::error::\`needs:\` list. That would report green while gating nothing."
  exit 1
fi

failed=0
unknown=0

# Sorted for stable, diffable logs.
while IFS=$'\t' read -r job result; do
  case "$result" in
    success|skipped)
      printf '  %-40s %s\n' "$job" "$result"
      ;;
    failure|cancelled)
      printf '  %-40s %s   <-- BLOCKING\n' "$job" "$result"
      failed=$((failed + 1))
      ;;
    *)
      # Not a result value GitHub documents. Fail closed rather than guess:
      # a silently-ignored novel state is how a gate stops gating.
      printf '  %-40s %s   <-- UNRECOGNISED, failing closed\n' "$job" "$result"
      unknown=$((unknown + 1))
      ;;
  esac
done < <(jq -r 'to_entries | sort_by(.key)[] | "\(.key)\t\(.value.result)"' <<<"$RESULTS")

echo
echo "Gates evaluated: ${count}"

if [ "$unknown" -gt 0 ]; then
  echo "::error::${unknown} gate(s) reported a result this script does not recognise."
  exit 1
fi

if [ "$failed" -gt 0 ]; then
  echo "::error::${failed} of ${count} gate(s) failed or were cancelled — blocking the merge."
  exit 1
fi

echo "All ${count} gate(s) passed or were skipped (skipped counts as pass)."
