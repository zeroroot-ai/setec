#!/usr/bin/env bash
# Refuses an ADR citation in a Go file (setec#240).
#
# The ADRs are internal. A Go comment in setec points to a public design
# page under docs/ instead, for example docs/design/isolation.md. The guard
# reads `git ls-files '*.go'`, so it sees what a clone holds.
#
#   check-no-adr-citation.sh             check the tree
#   check-no-adr-citation.sh --selftest  prove the guard can fail
set -euo pipefail

GUARD_NAME="no-adr-citation"
# "ADR" with an optional dash and two to four digits, as a whole word.
PATTERN='(^|[^A-Za-z0-9_])ADR-?[0-9]{2,4}([^0-9]|$)'

# scan <root> prints one "file:line:text" for each citation and returns 1
# when it printed one. It returns 2 when it read no Go file, because a
# guard that looked at nothing must not report a clean tree.
scan() {
  local root="$1" file found=0 files=0
  while IFS= read -r file; do
    [ -f "${root}/${file}" ] || continue
    files=$((files + 1))
    if grep -nIE "$PATTERN" "${root}/${file}" | sed "s#^#${file}:#"; then found=1; fi
  done < <(git -C "$root" ls-files '*.go')
  if [ "$files" -eq 0 ]; then
    echo "::error::${GUARD_NAME}: read no tracked Go file under ${root}" >&2
    return 2
  fi
  return "$found"
}

selftest() {
  local tmp out rc failures=0
  tmp="$(mktemp -d)"
  git init --quiet -b main "$tmp"
  mkdir -p "$tmp/internal/x" "$tmp/docs"
  printf '// The ceiling is in docs/design/lifecycles.md.\npackage x\n' >"$tmp/internal/x/x.go"
  # A word that only holds the letters, and a file that is not Go, pass.
  printf '// QUADR-1234 is a part number, not a citation.\npackage x\n' >"$tmp/internal/x/y.go"
  printf 'See ADR-0145.\n' >"$tmp/docs/notes.md"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a clean tree should pass, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  printf 'package x\n\n// Memory has a ceiling (ADR-0146).\nvar _ = 1\n' >"$tmp/internal/x/x.go"
  printf '// See ADR 12 and ADR0164.\npackage x\n' >"$tmp/internal/x/z.go"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"internal/x/x.go:3:"* ]] || [[ "$out" != *"internal/x/z.go:1:"* ]]; then
    echo "selftest FAILED: two citations should fail with file and line, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi

  local empty
  empty="$(mktemp -d)"
  git init --quiet -b main "$empty"
  set +e; out="$(scan "$empty" 2>&1)"; rc=$?; set -e
  rm -rf "$empty"
  if [ "$rc" -ne 2 ]; then
    echo "selftest FAILED: a tree with no tracked Go file should fail with rc=2, got rc=${rc}"; failures=$((failures + 1))
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
    echo "::error::${GUARD_NAME}: a Go file cites an ADR:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    echo "Point the comment to a design page under docs/ instead." >&2
    return 1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "PASS: ${GUARD_NAME}: no Go file cites an ADR"
}

main "$@"
