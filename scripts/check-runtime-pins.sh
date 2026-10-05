#!/usr/bin/env bash
# check-runtime-pins.sh — kata.env and gvisor.env are the only places the kata and
# gVisor releases are pinned.
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
#   7. gvisor.env has exactly GVISOR_VERSION (release-YYYYMMDD.N) and
#      GVISOR_SHA256 (64 hex). No consumer fetches gVisor from the release
#      bucket's `latest`: before gvisor.env existed, the e2e workflow did, so
#      CI could drift to a new gVisor on any run (setec#89).
#   8. Dockerfile.installer names no gVisor: no GVISOR_ build arg and no
#      gVisor release URL. gVisor left the installer image by owner decision
#      D70 (2026-10-05). The e2e workflow lays gVisor on its test node from
#      gvisor.env, as node preparation does on a real node.
#
#   9. kernel/kernel.env has exactly KERNEL_VERSION (x.y.z) and KERNEL_SHA256
#      (64 hex), and no other tracked file names the version on a code line
#      (setec#191). The build script and the workflows read kernel.env.
#
#  10. firecracker.env has exactly FIRECRACKER_VERSION (vX.Y.Z) and
#      FIRECRACKER_SHA256 (64 hex). Dockerfile.launcher declares both ARGs with
#      no default, and no other tracked file names the version on a code line.
#
# Usage: check-runtime-pins.sh [repo-root]     exit 1 on a violation
#        check-runtime-pins.sh --selftest      prove every rule can fire
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

# check_gvisor applies rules 7 and 8. Its version format is upstream's
# release-YYYYMMDD.N rather than x.y.z.
check_gvisor() {
  local root="$1" rc=0 env="$1/gvisor.env"
  fail() { echo "❌ $*"; rc=1; }

  [ -f "$env" ] || { echo "❌ $env is missing"; return 1; }
  local keys
  keys="$(grep -vE '^\s*(#|$)' "$env" | cut -d= -f1 | sort | paste -sd,)"
  [ "$keys" = "GVISOR_SHA256,GVISOR_VERSION" ] || fail "gvisor.env must hold exactly GVISOR_VERSION and GVISOR_SHA256, found: ${keys:-none}"
  local ver sha
  ver="$(grep -E '^GVISOR_VERSION=' "$env" | cut -d= -f2-)"
  sha="$(grep -E '^GVISOR_SHA256=' "$env" | cut -d= -f2-)"
  [[ "$ver" =~ ^release-[0-9]{8}\.[0-9]+$ ]] || fail "GVISOR_VERSION must be release-YYYYMMDD.N, found '${ver}'"
  [[ "$sha" =~ ^[0-9a-f]{64}$ ]] || fail "GVISOR_SHA256 must be 64 lowercase hex, found '${sha}'"

  # Rule 8: the installer image ships no gVisor (D70). The word `gvisor` may
  # stay in a comment that says so, and in the name of the gate mutation that
  # proves it. A build arg or a download is the payload coming back.
  if [ -f "$root/$DOCKERFILE" ]; then
    local d="$root/$DOCKERFILE"
    if code_lines "$d" | grep -qE 'GVISOR_[A-Z0-9_]+'; then
      fail "$DOCKERFILE names a GVISOR_ build arg; the installer image ships no gVisor (D70)"
    fi
    if grep -vE '^\s*#' "$d" | grep -qE 'github\.com/google/gvisor|storage\.googleapis\.com/gvisor'; then
      fail "$DOCKERFILE downloads gVisor; the installer image ships no gVisor (D70)"
    fi
  fi

  # The floating-tag trap this pin exists to close: no consumer may fetch gVisor
  # from the release bucket's `latest`, which is what the e2e workflow did until
  # setec#89 — so CI could drift onto a new gVisor on any run.
  #
  # NOT code_lines here. That helper strips from `//` to end of line, which eats
  # the rest of any URL after `https:` — so it would hide exactly the lines this
  # rule looks for. Comments are dropped with a `#`-only filter instead, which is
  # correct for YAML and shell, the two kinds of file scanned.
  #
  # This script is exempt: it names the pattern on purpose, to assert against it.
  local f
  for f in $(cd "$root" && git ls-files '.github/workflows/*.yml' 'development/*' 'scripts/*' 2>/dev/null); do
    [ -f "$root/$f" ] || continue
    case "$f" in */check-runtime-pins.sh) continue ;; esac
    if grep -vE '^\s*#' "$root/$f" | grep -qE 'gvisor/releases/release/latest'; then
      fail "$f fetches gVisor from release/latest; pin it from gvisor.env"
    fi
  done

  [ $rc -eq 0 ] && echo "✅ gvisor.env is the only gVisor pin (version ${ver}), and the installer image names no gVisor"
  return $rc
}

# check_kernel applies rule 9 to the guest kernel pin.
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

# check_firecracker applies rule 10 to the Firecracker pin of the launcher.
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
  copy() { rm -rf "$t/r"; mkdir -p "$t/r/$(dirname "$DEVSCRIPT")" "$t/r/$(dirname "$PACKER")" "$t/r/.github/workflows" "$t/r/kernel"; for f in kata.env gvisor.env kernel/kernel.env firecracker.env Dockerfile.launcher "$DOCKERFILE" "$DEVSCRIPT" "$PACKER"; do cp "$src/$f" "$t/r/$f"; done; (cd "$t/r" && git init -q . >/dev/null 2>&1 && git add -A >/dev/null 2>&1); }
  expect() { # <pass|fail> <name> — both halves must pass for a pass
    local want="$1" name="$2"; shift 2
    if check "$t/r" >/dev/null 2>&1 && check_gvisor "$t/r" >/dev/null 2>&1 && check_kernel "$t/r" >/dev/null 2>&1 && check_firecracker "$t/r" >/dev/null 2>&1; then got=pass; else got=fail; fi
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

  local gver
  gver="$(grep -E '^GVISOR_VERSION=' "$src/gvisor.env" | cut -d= -f2-)"
  copy; printf 'ARG GVISOR_VERSION\n' >> "$t/r/$DOCKERFILE"; expect fail "a gVisor build arg back in the installer Dockerfile"
  copy; printf 'RUN curl -fsSL -o /tmp/g.tar.zst https://github.com/google/gvisor/releases/download/%s/gvisor-x86_64.tar.zstd\n' "$gver" >> "$t/r/$DOCKERFILE"; expect fail "a gVisor download back in the installer Dockerfile"
  copy; printf '# gvisor left this image (D70).\n' >> "$t/r/$DOCKERFILE"; expect pass "a comment that names gVisor is ignored"
  copy; sed -i '/^GVISOR_SHA256=/d' "$t/r/gvisor.env"; expect fail "gvisor.env without the sha256"
  copy; sed -i 's/^GVISOR_VERSION=.*/GVISOR_VERSION=latest/' "$t/r/gvisor.env"; expect fail "gvisor.env version that is not release-YYYYMMDD.N"
  copy; printf 'EXTRA=1\n' >> "$t/r/gvisor.env"; expect fail "gvisor.env with an extra key"
  copy; printf 'x: curl https://storage.googleapis.com/gvisor/releases/release/latest/x86_64/runsc\n' > "$t/r/.github/workflows/z.yml"; (cd "$t/r" && git add -A >/dev/null 2>&1); expect fail "a consumer fetching gVisor from release/latest"

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
check "${1:-.}" || rc=1
check_gvisor "${1:-.}" || rc=1
check_kernel "${1:-.}" || rc=1
check_firecracker "${1:-.}" || rc=1
exit $rc
