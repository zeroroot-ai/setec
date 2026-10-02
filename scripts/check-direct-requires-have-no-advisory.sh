#!/usr/bin/env bash
# check-direct-requires-have-no-advisory.sh — a direct require must not name a
# release that OSV reports as affected.
#
# Why (setec#97)
# --------------
# Dependabot moved `examples/ci-sandbox` from grpc v1.83.2 to v1.84.0 (#81).
# v1.84.0 is newer, and it is also inside a LATER affected range of
# GO-2026-6443, whose ranges read:
#
#   introduced 0        fixed 1.82.2
#   introduced 1.83.0   fixed 1.83.2
#   introduced 1.84.0-dev   fixed 1.85.0-dev.0.20260825072537-93e31b48545e
#
# So v1.83.2 is fixed and the newer v1.84.0 is affected again. "Bump to the
# latest" walked back into the advisory, and every gate stayed green:
#
#   - `examples-govulncheck` passed, because govulncheck is reachability-based.
#     The example never calls the affected server path, so the vulnerable
#     version is present and not reported.
#   - OSSF Scorecard DID see it and scored it, but a Scorecard finding lands as
#     a code-scanning alert. It is a report, not a gate, so it blocked nothing.
#
# A published example is code a reader copies into their own project, where
# their code may well reach the symbol. The version matters there even when it
# is unreachable here. This guard is version-based on purpose, so it catches
# what a reachability scanner is designed to stay quiet about.
#
# What it does
# ------------
# 1. Every tracked `go.mod` in the repo. `go mod edit -json` reports each
#    requirement with its `Indirect` flag, so the direct set is read from the
#    toolchain rather than guessed with a regex.
# 2. One OSV `querybatch` request for the direct requires, keyed by module path
#    and selected version.
# 3. Any affected version is an error naming the module, the dependency, the
#    version and the advisory ids.
#
# Direct requires only, by design. The full module graph carries advisories in
# code nothing calls, which is govulncheck's job and would red-wall this gate.
# A direct require is a deliberate choice by someone in this repo.
#
# Usage: check-direct-requires-have-no-advisory.sh [--root .]  exit 1 on a hit
#   OSV_ENDPOINT  override the API (the fixture points this at a local stub)
set -uo pipefail

ROOT="."
while [ $# -gt 0 ]; do
  case "$1" in
    --root) ROOT="$2"; shift 2 ;;
    *) echo "usage: $0 [--root DIR]" >&2; exit 2 ;;
  esac
done
cd "$ROOT" || exit 2

OSV_ENDPOINT="${OSV_ENDPOINT:-https://api.osv.dev/v1/querybatch}"

command -v go >/dev/null || { echo "❌ go not on PATH"; exit 2; }
command -v jq >/dev/null || { echo "❌ jq not on PATH"; exit 2; }

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  mapfile -t mods < <(git ls-files -- '*go.mod' 'go.mod' | sort -u)
else
  mapfile -t mods < <(find . -name go.mod -type f | sed 's|^\./||' | sort)
fi
[ "${#mods[@]}" -gt 0 ] || { echo "❌ no go.mod found under $(pwd)"; exit 1; }

# One line per direct require: "<go.mod>\t<module>\t<version>".
rows="$(mktemp)"; req=""; res=""
trap 'rm -f "$rows" "$req" "$res"' EXIT
for m in "${mods[@]}"; do
  if ! json="$(go mod edit -json "$m" 2>&1)"; then
    echo "❌ $m: go mod edit -json failed"
    printf '%s\n' "$json" | sed 's/^/   /'
    exit 1
  fi
  printf '%s' "$json" \
    | jq -r --arg m "$m" '(.Require // [])[] | select((.Indirect // false) == false) | [$m, .Path, .Version] | @tsv' \
    >> "$rows"
done

count=$(wc -l < "$rows" | tr -d ' ')
if [ "$count" = "0" ]; then
  echo "❌ parsed 0 direct requires from ${#mods[@]} go.mod file(s); the parser is broken, not the tree"
  exit 1
fi

req="$(mktemp)"; res="$(mktemp)"
jq -Rn --rawfile rows "$rows" \
  '{queries: ($rows | rtrimstr("\n") | split("\n") | map(split("\t") | {package: {name: .[1], ecosystem: "Go"}, version: .[2]}))}' \
  > "$req"

if ! curl -sSf --max-time 60 -H 'Content-Type: application/json' --data @"$req" "$OSV_ENDPOINT" > "$res"; then
  echo "❌ OSV query failed against ${OSV_ENDPOINT}"
  echo "   This gate reads a live advisory database. A failed query is a failed"
  echo "   check, never a pass: a network error must not read as 'no advisories'."
  exit 1
fi

# results[] is positional and must line up with the rows we sent.
got=$(jq '(.results // []) | length' "$res" 2>/dev/null || echo -1)
if [ "$got" != "$count" ]; then
  echo "❌ OSV returned ${got} result(s) for ${count} quer(ies); refusing to map them"
  exit 1
fi

hits=0
while IFS=$'\t' read -r mod dep ver ids; do
  [ -n "$ids" ] || continue
  echo "❌ $mod: $dep $ver is affected by $ids"
  hits=$((hits+1))
done < <(paste "$rows" <(jq -r '(.results // [])[] | ((.vulns // []) | map(.id) | join(","))' "$res"))

if [ "$hits" -gt 0 ]; then
  echo "❌ direct-require advisory guard: ${hits} affected version(s) across ${#mods[@]} module(s)"
  echo "   Move each one to a release OSV reports as fixed. The newest release is"
  echo "   not always one of them: an advisory can open a later affected range."
  exit 1
fi
echo "✅ direct-require advisory guard: ${count} direct require(s) across ${#mods[@]} module(s), none affected"
