#!/usr/bin/env bash
# Refuses two leftovers of the kubebuilder scaffold (setec#173).
#
#   1. A `TODO(user)` line in any tracked file. The scaffold writes these
#      notes for the first author of a project. A note that stays tells a
#      reader that the file is not finished.
#   2. `insecureSkipVerify: true` in a tracked YAML file. A metrics monitor
#      that skips the certificate check trusts any endpoint that answers.
#
# The guard reads `git ls-files`, so it sees what a clone holds and nothing
# else. It does not read itself or CHANGELOG.md.
#
#   check-no-scaffold-notes.sh             check the tree
#   check-no-scaffold-notes.sh --selftest  prove the guard can fail
set -euo pipefail

GUARD_NAME="scaffold-notes"
SELF="scripts/check-no-scaffold-notes.sh"

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
    if grep -nF 'TODO(user)' "${root}/${file}" | sed "s#^#${file}:#"; then found=1; fi
    case "$file" in
      *.yaml|*.yml|*.tpl)
        if grep -nE 'insecureSkipVerify:[[:space:]]*true' "${root}/${file}" | sed "s#^#${file}:#"; then found=1; fi
        ;;
    esac
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
  mkdir -p "$tmp/config" "$tmp/scripts"
  printf 'kind: ServiceMonitor\nspec:\n  tlsConfig:\n    insecureSkipVerify: false\n' >"$tmp/config/monitor.yaml"
  printf 'package main\n\n// TODO: a plain note is not a scaffold note.\n' >"$tmp/main.go"
  # The guard never reads itself, so its own text cannot make it fail.
  printf '# TODO(user) and insecureSkipVerify: true\n' >"$tmp/$SELF"
  git -C "$tmp" add -A

  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a clean tree should pass, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  printf 'spec:\n  # TODO(user): Add fields here\n' >"$tmp/config/sample.yaml"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"config/sample.yaml:2:"* ]]; then
    echo "selftest FAILED: a TODO(user) line should fail and name its file and line, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi
  git -C "$tmp" rm --quiet -f config/sample.yaml

  printf 'spec:\n  tlsConfig:\n    insecureSkipVerify:   true\n' >"$tmp/config/monitor.yaml"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"config/monitor.yaml:3:"* ]]; then
    echo "selftest FAILED: insecureSkipVerify: true should fail and name its file and line, got rc=${rc}: ${out}"
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
  echo "PASS: ${GUARD_NAME} selftest (4 cases)"
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
    echo "::error::${GUARD_NAME}: a scaffold note or a TLS skip is in the tree:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    echo "Delete the TODO(user) line. Make the monitor verify the certificate of its target." >&2
    return 1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "PASS: ${GUARD_NAME}: no TODO(user) line and no insecureSkipVerify: true"
}

main "$@"
