#!/usr/bin/env bash
# check-kata-pin.sh — kata.env is the only place the kata release is pinned.
#
# Why (setec#26, epic zeroroot-ai/.github#20)
# ------------------------------------------
# The kata version and tarball sha256 used to live in three files by hand:
# Dockerfile.installer, development/k3s/scripts/20-install-kata.sh and
# packer/eks-kata-fc-ami/eks-kata-fc.pkr.hcl. A README sentence was the only
# thing that kept them equal. Now kata.env is the source and the three
# consumers read it. This guard fails when any consumer grows a literal of
# its own again.
#
# Rules (all keyed by CONTENT, never by line number)
# --------------------------------------------------
#   1. kata.env has exactly KATA_VERSION (x.y.z) and KATA_SHA256 (64 hex).
#   2. On a code line of any consumer (comments stripped) the literal value
#      of KATA_VERSION or KATA_SHA256 from kata.env never appears.
#   3. Dockerfile.installer: `ARG KATA_VERSION` and `ARG KATA_SHA256` exist
#      and carry no default (no `=`).
#   4. The dev script assigns KATA_VERSION only from kata.env: no
#      `KATA_VERSION=` line whose right side holds a digit or `:-`.
#   5. The packer recipe: the `kata_version` and `kata_sha256` variable
#      blocks carry no `default`.
#   6. Any code line that names kata and a bare x.y.z version is a literal.
#
# Usage: check-kata-pin.sh [repo-root]     exit 1 on a violation
#        check-kata-pin.sh --selftest      prove every rule can fire
set -uo pipefail

DOCKERFILE=Dockerfile.installer
DEVSCRIPT=development/k3s/scripts/20-install-kata.sh
PACKER=packer/eks-kata-fc-ami/eks-kata-fc.pkr.hcl

# code_lines <file>: the file without comments (# and // to end of line).
code_lines() { sed -E 's/(^|[[:space:]])(#|\/\/).*$//' "$1"; }

check() {
  local root="$1" rc=0 env="$1/kata.env"
  fail() { echo "❌ $*"; rc=1; }

  [ -f "$env" ] || { echo "❌ $env is missing"; return 1; }
  local keys
  keys="$(grep -vE '^\s*(#|$)' "$env" | cut -d= -f1 | sort | paste -sd,)"
  [ "$keys" = "KATA_SHA256,KATA_VERSION" ] || fail "kata.env must hold exactly KATA_VERSION and KATA_SHA256, found: ${keys:-none}"
  local ver sha
  ver="$(grep -E '^KATA_VERSION=' "$env" | cut -d= -f2-)"
  sha="$(grep -E '^KATA_SHA256=' "$env" | cut -d= -f2-)"
  [[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "KATA_VERSION must be x.y.z, found '${ver}'"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || fail "KATA_SHA256 must be 64 lowercase hex, found '${sha}'"

  local f
  for f in "$DOCKERFILE" "$DEVSCRIPT" "$PACKER"; do
    [ -f "$root/$f" ] || { fail "$f is missing"; continue; }
    if [ -n "$ver" ] && code_lines "$root/$f" | grep -nF -- "$ver" | grep -qE "(^|[^0-9.])${ver//./\\.}([^0-9.]|$)"; then
      fail "$f names the kata version literal ${ver}; read kata.env instead"
    fi
    if [ -n "$sha" ] && code_lines "$root/$f" | grep -qF -- "$sha"; then
      fail "$f names the kata sha256 literal; read kata.env instead"
    fi
    if code_lines "$root/$f" | grep -iE 'kata' | grep -qE '(^|[^0-9.])[0-9]+\.[0-9]+\.[0-9]+([^0-9.]|$)'; then
      fail "$f has a code line that names kata next to a bare x.y.z version"
    fi
  done

  if [ -f "$root/$DOCKERFILE" ]; then
    local d="$root/$DOCKERFILE"
    grep -qE '^ARG KATA_VERSION\s*$' "$d" || fail "$DOCKERFILE must declare 'ARG KATA_VERSION' with no default"
    grep -qE '^ARG KATA_SHA256\s*$'  "$d" || fail "$DOCKERFILE must declare 'ARG KATA_SHA256' with no default"
    grep -qE '^ARG KATA_(VERSION|SHA256)=' "$d" && fail "$DOCKERFILE gives a kata ARG a default; the value comes from kata.env"
  fi
  if [ -f "$root/$DEVSCRIPT" ]; then
    if code_lines "$root/$DEVSCRIPT" | grep -E '^\s*(export\s+)?KATA_VERSION=' | grep -qE '[0-9]|:-'; then
      fail "$DEVSCRIPT assigns KATA_VERSION a value of its own; source kata.env instead"
    fi
    grep -qE '^\s*(\.|source)\s+.*kata\.env' "$root/$DEVSCRIPT" || fail "$DEVSCRIPT must source kata.env"
  fi
  if [ -f "$root/$PACKER" ]; then
    local v
    for v in kata_version kata_sha256; do
      if awk -v v="$v" '$0 ~ "^variable \""v"\"" {inb=1} inb && /^}/ {inb=0} inb && /^[[:space:]]*default[[:space:]]*=/ {found=1} END {exit !found}' "$root/$PACKER"; then
        fail "$PACKER gives variable $v a default; bake.sh passes it from kata.env"
      fi
    done
  fi
  [ $rc -eq 0 ] && echo "✅ kata.env is the only kata pin (version ${ver})"
  return $rc
}

selftest() {
  local src t rc=0
  src="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  SELFTEST_TMP="$(mktemp -d)"; t="$SELFTEST_TMP"; trap 'rm -rf "$SELFTEST_TMP"' EXIT
  copy() { rm -rf "$t/r"; mkdir -p "$t/r/$(dirname "$DEVSCRIPT")" "$t/r/$(dirname "$PACKER")"; for f in kata.env "$DOCKERFILE" "$DEVSCRIPT" "$PACKER"; do cp "$src/$f" "$t/r/$f"; done; }
  expect() { # <pass|fail> <name>
    local want="$1" name="$2"; shift 2
    if check "$t/r" >/dev/null 2>&1; then got=pass; else got=fail; fi
    if [ "$got" = "$want" ]; then echo "PASS: $name"; else echo "FAIL: $name (expected $want, got $got)"; rc=1; fi
  }
  local ver sha
  ver="$(grep -E '^KATA_VERSION=' "$src/kata.env" | cut -d= -f2-)"
  sha="$(grep -E '^KATA_SHA256=' "$src/kata.env" | cut -d= -f2-)"

  copy; expect pass "pristine tree passes"
  copy; sed -i "s/^ARG KATA_VERSION\$/ARG KATA_VERSION=${ver}/" "$t/r/$DOCKERFILE"; expect fail "Dockerfile ARG default re-added"
  copy; sed -i "s/^ARG KATA_SHA256\$/ARG KATA_SHA256=${sha}/" "$t/r/$DOCKERFILE"; expect fail "Dockerfile sha ARG default re-added"
  copy; printf 'KATA_VERSION="${KATA_VERSION:-%s}"\n' "$ver" >> "$t/r/$DEVSCRIPT"; expect fail "dev script default re-added"
  copy; sed -i '/kata\.env/d' "$t/r/$DEVSCRIPT"; expect fail "dev script no longer sources kata.env"
  copy; sed -i "/^variable \"kata_version\" {/a\\  default     = \"${ver}\"" "$t/r/$PACKER"; expect fail "packer kata_version default re-added"
  copy; sed -i "/^variable \"kata_sha256\" {/a\\  default     = \"${sha}\"" "$t/r/$PACKER"; expect fail "packer kata_sha256 default re-added"
  copy; printf 'RUN echo kata %s\n' "$ver" >> "$t/r/$DOCKERFILE"; expect fail "version literal on a Dockerfile code line"
  copy; printf 'RUN echo kata 9.9.9\n' >> "$t/r/$DOCKERFILE"; expect fail "a different bare x.y.z next to kata on a code line"
  copy; printf '# kata %s in a comment is fine\n' "$ver" >> "$t/r/$DEVSCRIPT"; expect pass "version literal in a comment is ignored"
  copy; sed -i '/^KATA_SHA256=/d' "$t/r/kata.env"; expect fail "kata.env without the sha256"
  copy; sed -i 's/^KATA_VERSION=.*/KATA_VERSION=latest/' "$t/r/kata.env"; expect fail "kata.env version that is not x.y.z"
  copy; printf 'EXTRA=1\n' >> "$t/r/kata.env"; expect fail "kata.env with an extra key"
  [ $rc -eq 0 ] && echo "✅ self-test: every rule fires and the pristine tree passes"
  return $rc
}

if [ "${1:-}" = "--selftest" ]; then selftest; exit $?; fi
check "${1:-.}"
