#!/usr/bin/env bash
# Builds the guest kernel of a launcher machine from kernel/kernel.env and
# kernel/x86_64.defconfig (setec#191).
#
#   build-guest-kernel.sh <out-dir>          download, verify, build vmlinux
#   build-guest-kernel.sh --check <src-dir>  only check that the config is
#                                            minimal and consistent for <src-dir>
#   build-guest-kernel.sh --selftest         plant each defect the check refuses
#                                            and require it to fail
#
# The output is vmlinux, vmlinux.config and SHA256SUMS. The kernel has no
# modules: each driver the machine needs is built in.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=/dev/null
. "$root/kernel/kernel.env"
: "${KERNEL_VERSION:?kernel.env sets no KERNEL_VERSION}" "${KERNEL_SHA256:?kernel.env sets no KERNEL_SHA256}"
defconfig="$root/kernel/x86_64.defconfig"

# settings <file>: the config lines of a defconfig. A "# CONFIG_X is not set"
# line is a setting and stays; every other comment and blank line goes.
settings() { grep -E '^(CONFIG_|# CONFIG_.* is not set$)' "$1"; }

configure() { # configure <src-dir>: the config, and the check that it is minimal
  local src="$1"
  settings "$defconfig" > "$src/arch/x86/configs/setec_defconfig"
  make -s -C "$src" setec_defconfig
  make -s -C "$src" savedefconfig
  verdict "$defconfig" "$src/defconfig" "$src/.config"
}

# verdict <committed defconfig> <savedefconfig output> <.config>: the checks
# on the configured tree. The selftest runs them on planted files.
verdict() {
  local committed="$1" saved="$2" config="$3"
  if [ ! -s "$config" ]; then
    echo "::error::the kernel config $config is missing or empty, so nothing was checked" >&2
    return 1
  fi
  if ! diff -u <(settings "$committed") <(settings "$saved"); then
    echo "::error::kernel/x86_64.defconfig is not the minimal config of linux-${KERNEL_VERSION}; replace it with the savedefconfig output above" >&2
    return 1
  fi
  if grep -q '^CONFIG_MODULES=y' "$config"; then
    echo "::error::the guest kernel must not load modules" >&2
    return 1
  fi
  # A process in the machine must not reach the guest agent over vsock.
  if grep -q '^CONFIG_VSOCKETS_LOOPBACK=y' "$config"; then
    echo "::error::the guest kernel must not have vsock loopback" >&2
    return 1
  fi
}

if [ "${1:-}" = "--selftest" ]; then
  # Each planted defect must fail the verdict, and a consistent tree must
  # pass (gibson#807 condition 7). The kernel source is not needed: the
  # verdict reads three files.
  st="$(mktemp -d)"
  trap 'rm -rf "$st"' EXIT
  fails=0
  expect() { # expect <pass|fail> <desc> <committed> <saved> <config>
    local got=pass
    verdict "$3" "$4" "$5" >/dev/null 2>&1 || got=fail
    if [ "$got" = "$1" ]; then echo "  ok    $2 -> $got"; else echo "  FAIL  $2 -> $got, want $1"; fails=$((fails + 1)); fi
  }
  printf '# comment\nCONFIG_VIRTIO=y\n# CONFIG_MODULES is not set\n' > "$st/committed"
  printf 'CONFIG_VIRTIO=y\n# CONFIG_MODULES is not set\n' > "$st/saved"
  printf 'CONFIG_VIRTIO=y\n# CONFIG_MODULES is not set\n' > "$st/config"
  printf 'CONFIG_VIRTIO=y\nCONFIG_NET=y\n# CONFIG_MODULES is not set\n' > "$st/saved-more"
  printf 'CONFIG_VIRTIO=y\nCONFIG_MODULES=y\n' > "$st/config-modules"
  printf 'CONFIG_VIRTIO=y\nCONFIG_VSOCKETS_LOOPBACK=y\n' > "$st/config-loopback"
  : > "$st/empty"
  expect pass "a minimal, consistent config" "$st/committed" "$st/saved" "$st/config"
  expect fail "MUTATION the committed defconfig is not the savedefconfig output" "$st/committed" "$st/saved-more" "$st/config"
  expect fail "MUTATION the kernel loads modules" "$st/committed" "$st/saved" "$st/config-modules"
  expect fail "MUTATION the kernel has vsock loopback" "$st/committed" "$st/saved" "$st/config-loopback"
  expect fail "MUTATION an empty kernel config" "$st/committed" "$st/saved" "$st/empty"
  echo "build-guest-kernel --selftest: $fails failure(s)"
  exit $((fails > 0))
fi

if [ "${1:-}" = "--check" ]; then
  configure "${2:?usage: --check <src-dir>}"
  echo "the guest kernel config is minimal and consistent for linux-${KERNEL_VERSION}"
  exit 0
fi

out="${1:?usage: build-guest-kernel.sh <out-dir>}"
mkdir -p "$out"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
tarball="linux-${KERNEL_VERSION}.tar.xz"
curl -fsSL --retry 3 -o "$work/$tarball" "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x/$tarball"
echo "${KERNEL_SHA256}  $work/$tarball" | sha256sum -c -
tar -xf "$work/$tarball" -C "$work"
src="$work/linux-${KERNEL_VERSION}"
configure "$src"
make -s -C "$src" -j"$(nproc)" vmlinux KBUILD_BUILD_TIMESTAMP="@0" KBUILD_BUILD_USER=setec KBUILD_BUILD_HOST=setec
cp "$src/vmlinux" "$out/vmlinux"
cp "$src/.config" "$out/vmlinux.config"
(cd "$out" && sha256sum vmlinux vmlinux.config > SHA256SUMS)
echo "built linux-${KERNEL_VERSION} guest kernel into $out"
