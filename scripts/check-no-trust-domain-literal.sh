#!/usr/bin/env bash
# Refuses a SPIFFE ID or trust domain literal of a real install (setec#169).
#
# Each install has its own SPIFFE trust domain (ADR-0164). One config value
# names it: credentials.spiffe.trustDomain in the chart, and the enrolled
# client IDs of the frontend. No code, chart, script or document holds the
# trust domain of the SaaS install as a literal. Examples use example.org.
#
# The guard reads `git ls-files`, so it sees what a clone holds. It does not
# read itself or CHANGELOG.md, which release-please owns.
#
#   check-no-trust-domain-literal.sh             check the tree
#   check-no-trust-domain-literal.sh --selftest  prove the guard can fail
set -euo pipefail

GUARD_NAME="trust-domain-literal"
SELF="scripts/check-no-trust-domain-literal.sh"
# The literal is built from two parts, so this file never holds it whole.
DOMAIN="zeroroot"".ai"
PATTERN="spiffe://${DOMAIN//./\\.}([/\"' ]|$)"

# scan <root> prints one "file:line:text" for each violation and returns 1
# when it printed one. It returns 2 when it read no file, because a guard
# that looked at nothing must not report a clean tree.
scan() {
  local root="$1" file found=0 files=0
  while IFS= read -r file; do
    case "$file" in
      "$SELF"|CHANGELOG.md) continue ;;
    esac
    [ -f "${root}/${file}" ] || continue
    files=$((files + 1))
    if grep -nIE "$PATTERN" "${root}/${file}" | sed "s#^#${file}:#"; then found=1; fi
  done < <(git -C "$root" ls-files)
  if [ "$files" -eq 0 ]; then
    echo "::error::${GUARD_NAME}: read no tracked file under ${root}" >&2
    return 2
  fi
  return "$found"
}

selftest() {
  local tmp out rc failures=0
  tmp="$(mktemp -d)"
  git init --quiet -b main "$tmp"
  mkdir -p "$tmp/scripts" "$tmp/charts"
  printf 'id := "spiffe://example.org/ns/gibson/sa/gibson-daemon"\n' >"$tmp/main.go"
  # A different name that only starts with the same word is not the literal.
  printf 'spiffeID: spiffe://%s-staging.example/ns/x\n' "zeroroot" >"$tmp/charts/values.yaml"
  printf '# spiffe://%s/ns/x in the guard itself\n' "$DOMAIN" >"$tmp/$SELF"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a clean tree should pass, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  printf 'x\n  # e.g. spiffe://%s/ns/gibson/sa/gibson-daemon\n' "$DOMAIN" >"$tmp/charts/values.yaml"
  printf 'const td = "spiffe://%s"\n' "$DOMAIN" >"$tmp/td.go"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"charts/values.yaml:2:"* ]] || [[ "$out" != *"td.go:1:"* ]]; then
    echo "selftest FAILED: two literals should fail with file and line, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi

  local empty
  empty="$(mktemp -d)"
  git init --quiet -b main "$empty"
  set +e; out="$(scan "$empty" 2>&1)"; rc=$?; set -e
  rm -rf "$empty"
  if [ "$rc" -ne 2 ]; then
    echo "selftest FAILED: a tree with no tracked file should fail with rc=2, got rc=${rc}"; failures=$((failures + 1))
  fi

  rm -rf "$tmp"
  if [ "$failures" -ne 0 ]; then
    echo "::error::${GUARD_NAME}: selftest FAILED (${failures})" >&2
    return 1
  fi
  echo "PASS: ${GUARD_NAME} selftest (3 cases)"
}

main() {
  case "${1:-}" in
    --selftest) selftest; return ;;
    "") ;;
    *) echo "::error::${GUARD_NAME}: unknown argument: $1" >&2; return 2 ;;
  esac
  local root out rc
  root="$(git rev-parse --show-toplevel)"
  set +e; out="$(scan "$root")"; rc=$?; set -e
  if [ "$rc" -eq 1 ]; then
    echo "::error::${GUARD_NAME}: the trust domain of a real install is in the tree:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    echo "Use example.org in an example. Read the trust domain from config everywhere else (ADR-0164)." >&2
    return 1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "PASS: ${GUARD_NAME}: no trust domain literal of a real install"
}

main "$@"
