#!/usr/bin/env bash
# check-runtime-pins.sh — kernel/kernel.env and firecracker.env are the only
# places the guest kernel and Firecracker releases are pinned.
#
# Why (setec#26, setec#191)
# -------------------------
# A version that lives in two files drifts. Each pin file is the source, and
# every consumer reads it. This guard fails when any consumer grows a literal
# of its own. The kata and gVisor pins left with their backends (setec#198).
#
# Rules (all keyed by CONTENT, never by line number)
# --------------------------------------------------
#   1. kernel/kernel.env has exactly KERNEL_VERSION (x.y.z) and KERNEL_SHA256
#      (64 hex), and no other tracked file names the version on a code line
#      (setec#191). The build script and the workflows read kernel.env.
#
#   2. firecracker.env has exactly FIRECRACKER_VERSION (vX.Y.Z) and
#      FIRECRACKER_SHA256 (64 hex). Dockerfile.launcher declares both ARGs with
#      no default, and no other tracked file names the version on a code line.
#
# Usage: check-runtime-pins.sh [repo-root]     exit 1 on a violation
#        check-runtime-pins.sh --selftest      prove every rule can fire
set -uo pipefail

# code_lines <file>: the file without comments (# and // to end of line).
code_lines() { sed -E 's/(^|[[:space:]])(#|\/\/).*$//' "$1"; }

# check_kernel applies rule 1 to the guest kernel pin.
check_kernel() {
  local root="$1" rc=0 env="$1/kernel/kernel.env"
  fail() { echo "❌ $*"; rc=1; }
  [ -f "$env" ] || { echo "❌ $env is missing"; return 1; }
  local keys ver sha f
  keys="$(grep -vE '^\s*(#|$)' "$env" | cut -d= -f1 | sort | paste -sd,)"
  [ "$keys" = "KERNEL_SHA256,KERNEL_VERSION" ] || fail "kernel/kernel.env must hold exactly KERNEL_VERSION and KERNEL_SHA256, found: ${keys:-none}"
  ver="$(grep -E '^KERNEL_VERSION=' "$env" | cut -d= -f2-)"
  sha="$(grep -E '^KERNEL_SHA256=' "$env" | cut -d= -f2-)"
  [[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "KERNEL_VERSION must be x.y.z, found '${ver}'"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || fail "KERNEL_SHA256 must be 64 lowercase hex, found '${sha}'"
  if [ -n "$ver" ]; then
    for f in $(cd "$root" && git ls-files 2>/dev/null); do
      case "$f" in kernel/kernel.env|CHANGELOG.md|*/check-runtime-pins.sh|go.sum) continue ;; esac
      [ -f "$root/$f" ] || continue
      if code_lines "$root/$f" | grep -qE "(^|[^0-9.])${ver//./\\.}([^0-9]|$)"; then
        fail "$f names the guest kernel version ${ver}; read kernel/kernel.env instead"
      fi
    done
  fi
  [ $rc -eq 0 ] && echo "✅ kernel/kernel.env is the only guest kernel pin (version ${ver})"
  return $rc
}

# check_firecracker applies rule 2 to the Firecracker pin of the launcher.
check_firecracker() {
  local root="$1" rc=0 env="$1/firecracker.env" d="$1/Dockerfile.launcher"
  fail() { echo "❌ $*"; rc=1; }
  [ -f "$env" ] || { echo "❌ $env is missing"; return 1; }
  local keys ver sha f
  keys="$(grep -vE '^\s*(#|$)' "$env" | cut -d= -f1 | sort | paste -sd,)"
  [ "$keys" = "FIRECRACKER_SHA256,FIRECRACKER_VERSION" ] || fail "firecracker.env must hold exactly FIRECRACKER_VERSION and FIRECRACKER_SHA256, found: ${keys:-none}"
  ver="$(grep -E '^FIRECRACKER_VERSION=' "$env" | cut -d= -f2-)"
  sha="$(grep -E '^FIRECRACKER_SHA256=' "$env" | cut -d= -f2-)"
  [[ "$ver" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "FIRECRACKER_VERSION must be vX.Y.Z, found '${ver}'"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || fail "FIRECRACKER_SHA256 must be 64 lowercase hex, found '${sha}'"
  if [ -f "$d" ]; then
    grep -qE '^ARG FIRECRACKER_VERSION\s*$' "$d" || fail "Dockerfile.launcher must declare 'ARG FIRECRACKER_VERSION' with no default"
    grep -qE '^ARG FIRECRACKER_SHA256\s*$' "$d" || fail "Dockerfile.launcher must declare 'ARG FIRECRACKER_SHA256' with no default"
  fi
  if [ -n "$ver" ]; then
    for f in $(cd "$root" && git ls-files 2>/dev/null); do
      case "$f" in firecracker.env|CHANGELOG.md|*/check-runtime-pins.sh|go.sum) continue ;; esac
      [ -f "$root/$f" ] || continue
      if code_lines "$root/$f" | grep -qF -- "$ver"; then
        fail "$f names the Firecracker version ${ver}; read firecracker.env instead"
      fi
    done
  fi
  [ $rc -eq 0 ] && echo "✅ firecracker.env is the only Firecracker pin (version ${ver})"
  return $rc
}

selftest() {
  local src t rc=0
  src="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  SELFTEST_TMP="$(mktemp -d)"; t="$SELFTEST_TMP"; trap 'rm -rf "$SELFTEST_TMP"' EXIT
  copy() { rm -rf "$t/r"; mkdir -p "$t/r/.github/workflows" "$t/r/kernel"; for f in kernel/kernel.env firecracker.env Dockerfile.launcher; do cp "$src/$f" "$t/r/$f"; done; (cd "$t/r" && git init -q . >/dev/null 2>&1 && git add -A >/dev/null 2>&1); }
  expect() { # <pass|fail> <name> — both checks must pass for a pass
    local want="$1" name="$2"; shift 2
    if check_kernel "$t/r" >/dev/null 2>&1 && check_firecracker "$t/r" >/dev/null 2>&1; then got=pass; else got=fail; fi
    if [ "$got" = "$want" ]; then echo "PASS: $name"; else echo "FAIL: $name (expected $want, got $got)"; rc=1; fi
  }

  copy; expect pass "pristine tree passes"

  local kver
  kver="$(grep -E '^KERNEL_VERSION=' "$src/kernel/kernel.env" | cut -d= -f2-)"
  copy; sed -i '/^KERNEL_SHA256=/d' "$t/r/kernel/kernel.env"; expect fail "kernel.env without the sha256"
  copy; sed -i 's/^KERNEL_VERSION=.*/KERNEL_VERSION=latest/' "$t/r/kernel/kernel.env"; expect fail "kernel.env version that is not x.y.z"
  copy; printf 'curl https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-%s.tar.xz\n' "$kver" > "$t/r/.github/workflows/k.yml"; (cd "$t/r" && git add -A >/dev/null 2>&1); expect fail "a second place names the guest kernel version"
  copy; printf '# the kernel %s in a comment is fine\n' "$kver" > "$t/r/.github/workflows/k.yml"; (cd "$t/r" && git add -A >/dev/null 2>&1); expect pass "the guest kernel version in a comment is ignored"

  local fver
  fver="$(grep -E '^FIRECRACKER_VERSION=' "$src/firecracker.env" | cut -d= -f2-)"
  copy; sed -i '/^FIRECRACKER_SHA256=/d' "$t/r/firecracker.env"; expect fail "firecracker.env without the sha256"
  copy; sed -i "s/^ARG FIRECRACKER_VERSION\$/ARG FIRECRACKER_VERSION=${fver}/" "$t/r/Dockerfile.launcher"; expect fail "a Firecracker ARG default re-added"
  copy; printf 'RUN echo %s\n' "$fver" > "$t/r/Dockerfile.other"; (cd "$t/r" && git add -A >/dev/null 2>&1); expect fail "a second place names the Firecracker version"

  [ $rc -eq 0 ] && echo "✅ self-test: every rule fires and the pristine tree passes"
  return $rc
}

if [ "${1:-}" = "--selftest" ]; then selftest; exit $?; fi
rc=0
check_kernel "${1:-.}" || rc=1
check_firecracker "${1:-.}" || rc=1
exit $rc
