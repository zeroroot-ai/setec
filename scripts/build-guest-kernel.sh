#!/usr/bin/env bash
# Builds the guest kernel of a launcher machine from kernel/kernel.env and
# kernel/x86_64.defconfig (setec#191).
#
#   build-guest-kernel.sh <out-dir>          download, verify, build vmlinux
#   build-guest-kernel.sh --check <src-dir>  only check that the config is
#                                            minimal and consistent for <src-dir>
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
  if ! diff -u <(settings "$defconfig") <(settings "$src/defconfig"); then
    echo "::error::kernel/x86_64.defconfig is not the minimal config of linux-${KERNEL_VERSION}; replace it with the savedefconfig output above" >&2
    return 1
  fi
  if grep -q '^CONFIG_MODULES=y' "$src/.config"; then
    echo "::error::the guest kernel must not load modules" >&2
    return 1
  fi
  # A process in the machine must not reach the guest agent over vsock.
  if grep -q '^CONFIG_VSOCKETS_LOOPBACK=y' "$src/.config"; then
    echo "::error::the guest kernel must not have vsock loopback" >&2
    return 1
  fi
}

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
