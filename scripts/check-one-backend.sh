#!/usr/bin/env bash
# Refuses a second isolation backend (setec#198, D46).
#
# setec has one backend: each Sandbox is a Firecracker machine in a
# launcher Pod (docs/design/runtime.md). The kata-fc, kata-qemu, gvisor and
# runc backends left in one cutover. This guard fails when a second backend
# comes back into the tree. Its rules read content, never line numbers:
#
#   1. No code line names a removed backend or its runtime (kata, gvisor,
#      runsc, runc). Comments and Markdown may tell the history. The one
#      code home of the removed names is internal/runtime/backend.go, which
#      refuses them, with its test. A proto `reserved` line keeps a retired
#      field name and passes.
#   2. internal/runtime/backend.go declares exactly one Backend constant.
#   3. A SandboxClass in the chart names the launcher as runtime.backend.
#
# The guard reads `git ls-files`, so it sees what a clone holds. It does not
# read itself or CHANGELOG.md, which release-please owns.
#
#   check-one-backend.sh             check the tree
#   check-one-backend.sh --selftest  prove the guard can fail
set -euo pipefail

GUARD_NAME="one-backend"
SELF="scripts/check-one-backend.sh"
BACKEND_GO="internal/runtime/backend.go"
BACKEND_TEST="internal/runtime/backend_test.go"
# The removed names. The guard builds the pattern from parts, so a search
# for a removed name does not find this file first.
REMOVED="kata|g""visor|run""sc|ru""nc"
PATTERN="(^|[^[:alnum:]_])(${REMOVED})([^[:alnum:]_]|$)"

# code_lines prints each line of a file without its comment, with the line
# number in front. A comment starts at # or // after a blank or at the
# start of the line. A proto `reserved` line is not code.
code_lines() {
  sed -E 's/(^|[[:space:]])(#|\/\/).*$//' "$1" | awk '{ print NR ":" $0 }' |
    grep -vE '^[0-9]+:[[:space:]]*reserved[[:space:]]' || true
}

# scan <root> prints one "file:line:text" for each violation and returns 1
# when it printed one. It returns 2 when it read no file, because a guard
# that looked at nothing must not report a clean tree.
scan() {
  local root="$1" file found=0 files=0 hits consts
  while IFS= read -r file; do
    case "$file" in
      "$SELF"|CHANGELOG.md|go.sum|go.mod|*.md|"$BACKEND_GO"|"$BACKEND_TEST") continue ;;
    esac
    [ -f "${root}/${file}" ] || continue
    files=$((files + 1))
    # A binary file has no code line.
    grep -qI . "${root}/${file}" || continue
    # Rule 1: a removed backend name on a code line.
    hits="$(code_lines "${root}/${file}" | grep -iE "^[0-9]+:.*${PATTERN}" || true)"
    if [ -n "$hits" ]; then
      printf '%s\n' "$hits" | sed "s#^#${file}:#"
      found=1
    fi
    # Rule 3: runtime.backend of a SandboxClass in the chart.
    case "$file" in
      charts/*.yaml|charts/*.yml)
        hits="$(awk '
          /runtime:[[:space:]]*\{[[:space:]]*backend:/ {
            v = $0; sub(/.*runtime:[[:space:]]*\{[[:space:]]*backend:[[:space:]]*/, "", v); sub(/[,}].*/, "", v)
            gsub(/["'"'"'[:space:]]/, "", v)
            if (v != "launcher") print NR ": runtime.backend is \"" v "\", want launcher"
            next
          }
          /^[[:space:]]*runtime:[[:space:]]*$/ { rt = 1; next }
          rt && /^[[:space:]]*backend:/ {
            v = $0; sub(/^[[:space:]]*backend:[[:space:]]*/, "", v); gsub(/["'"'"'[:space:]]/, "", v)
            if (v != "launcher") print NR ": runtime.backend is \"" v "\", want launcher"
          }
          { rt = 0 }' "${root}/${file}")"
        if [ -n "$hits" ]; then
          printf '%s\n' "$hits" | sed "s#^#${file}:#"
          found=1
        fi
        ;;
    esac
  done < <(git -C "$root" ls-files)
  if [ "$files" -eq 0 ]; then
    echo "::error::${GUARD_NAME}: read no tracked file under ${root}" >&2
    return 2
  fi
  # Rule 2: one Backend constant.
  if [ ! -f "${root}/${BACKEND_GO}" ]; then
    echo "${BACKEND_GO}:0: the file that names the one backend is missing"
    return 1
  fi
  consts="$(code_lines "${root}/${BACKEND_GO}" | grep -cE '^[0-9]+:[[:space:]]*(const[[:space:]]+)?Backend[[:alnum:]_]*([[:space:]]+[[:alnum:]_.]+)?[[:space:]]*=' || true)"
  if [ "$consts" -ne 1 ]; then
    echo "${BACKEND_GO}:0: declares ${consts} Backend constants, want exactly one (the launcher)"
    found=1
  fi
  return "$found"
}

# fixture <dir> makes a clean tree: the one backend, a chart class of the
# launcher, and a comment that tells the history.
fixture() {
  local dir="$1"
  [ -d "$dir/.git" ] || git init --quiet -b main "$dir"
  mkdir -p "$dir/internal/runtime" "$dir/charts/setec" "$dir/scripts" "$dir/api"
  printf 'package runtime\n\nconst BackendLauncher = "launcher"\n\nvar RemovedBackends = []string{"kata-fc", "gvisor"}\n' \
    >"$dir/${BACKEND_GO}"
  printf 'classes:\n  - name: standard\n    spec:\n      runtime:\n        backend: launcher\n' >"$dir/charts/setec/values.yaml"
  printf '// The kata-fc and gvisor backends left in setec#198.\npackage main\n' >"$dir/main.go"
  printf 'message M {\n  reserved "kata_socket_target";\n}\n' >"$dir/api/m.proto"
  printf 'The %s backend left.\n' "kata-fc" >"$dir/README.md"
  printf '# not the guard\n' >"$dir/$SELF"
  git -C "$dir" add -A
}

selftest() {
  local tmp out rc failures=0
  tmp="$(mktemp -d)"
  fixture "$tmp"
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 0 ]; then
    echo "selftest FAILED: a clean tree should pass, got rc=${rc}: ${out}"; failures=$((failures + 1))
  fi

  # Rule 1: a removed backend name on a code line.
  printf 'package main\n\nvar class = "%s"\n' "g""visor" >"$tmp/main.go"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"main.go:3:"* ]]; then
    echo "selftest FAILED: a removed backend in code should fail with file and line, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi
  fixture "$tmp"

  # Rule 2: a second Backend constant.
  printf 'package runtime\n\nconst (\n\tBackendLauncher = "launcher"\n\tBackendQEMU = "qemu"\n)\n' >"$tmp/${BACKEND_GO}"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"declares 2 Backend constants"* ]]; then
    echo "selftest FAILED: a second Backend constant should fail, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi
  fixture "$tmp"

  # Rule 3: a chart class with another backend, here one with no removed name.
  printf 'classes:\n  - name: fast\n    spec:\n      runtime:\n        backend: "qemu"\n' >"$tmp/charts/setec/values.yaml"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"values.yaml:5:"* ]]; then
    echo "selftest FAILED: a chart class of another backend should fail, got rc=${rc}: ${out}"
    failures=$((failures + 1))
  fi

  fixture "$tmp"
  printf 'classes:\n  - name: fast\n    spec:\n      runtime: {backend: qemu}\n' >"$tmp/charts/setec/values.yaml"
  git -C "$tmp" add -A
  set +e; out="$(scan "$tmp")"; rc=$?; set -e
  if [ "$rc" -ne 1 ] || [[ "$out" != *"values.yaml:4:"* ]]; then
    echo "selftest FAILED: a chart class of another backend in flow form should fail, got rc=${rc}: ${out}"
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
  echo "PASS: ${GUARD_NAME} selftest (6 cases)"
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
    echo "::error::${GUARD_NAME}: a second isolation backend is in the tree:" >&2
    printf '%s\n' "$out" | sed 's/^/  /' >&2
    echo "setec has one backend, the launcher (setec#198, docs/design/runtime.md)." >&2
    return 1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  echo "PASS: ${GUARD_NAME}: the launcher is the one backend"
}

main "$@"
