#!/usr/bin/env bash
# Asserts that the `ci-required` aggregator actually depends on every gate job
# in its workflow.
#
# ci-required-gate.sh reads `${{ toJSON(needs) }}`, so it can never ignore a
# dependency that IS listed. This covers the other direction: a gate job added
# to the workflow and never added to `needs:`. That job would run, could report
# red, and the merge would proceed — which is exactly the condition the
# aggregator exists to end, reintroduced one job at a time.
#
# It is the same failure shape as the ruleset list this replaces. The only
# reason it is better is that both halves now live in this repo, in one file,
# so a machine can check them against each other. This script is that machine.
#
# Usage: check-ci-required-coverage.sh [workflow.yml ...]
#        defaults to .github/workflows/go-ci.yml
#
# Exit 0 = every gate job is covered. Exit 1 = at least one is not.

set -uo pipefail

FILES=("$@")
if [ ${#FILES[@]} -eq 0 ]; then
  FILES=(".github/workflows/go-ci.yml")
fi

rc=0

for wf in "${FILES[@]}"; do
  if [ ! -f "$wf" ]; then
    echo "::error::${wf}: no such workflow file"
    rc=1
    continue
  fi

  python3 - "$wf" <<'PY' || rc=1
import sys, yaml

path = sys.argv[1]
doc = yaml.safe_load(open(path))
jobs = doc.get("jobs") or {}

AGG = "ci-required"

if AGG not in jobs:
    print(f"::error::{path}: no `{AGG}` job — this workflow feeds the single "
          f"required status check and must define the aggregator")
    sys.exit(1)

agg = jobs[AGG]
needs = agg.get("needs") or []
if isinstance(needs, str):
    needs = [needs]
needs = set(needs)

# Jobs that are plumbing rather than gates, and so are legitimately absent from
# `needs:`. Keep this list SHORT and justified — every entry is a hole.
#   changes   change detection feeding the gates' `if:`. It is depended on
#             transitively by the gates themselves; a failure there fails them.
EXEMPT = {AGG, "changes"}

gates = set(jobs) - EXEMPT
missing = sorted(gates - needs)
phantom = sorted(needs - set(jobs))

if missing:
    print(f"::error::{path}: `{AGG}` does not depend on: {', '.join(missing)}")
    print(f"::error::Those jobs run, can report red, and would not block a merge.")
    print(f"::error::Add them to `{AGG}`'s `needs:` list, or add a justified")
    print(f"::error::entry to EXEMPT in scripts/check-ci-required-coverage.sh.")

if phantom:
    # A stale name in `needs:` is not cosmetic: GitHub fails the workflow at
    # parse time, so this would present as an infrastructure error rather than
    # a gate failure.
    print(f"::error::{path}: `{AGG}` lists unknown job(s) in needs: {', '.join(phantom)}")

if missing or phantom:
    sys.exit(1)

print(f"ok  {path}: `{AGG}` covers all {len(gates)} gate job(s): "
      f"{', '.join(sorted(gates))}")
PY
done

exit $rc
