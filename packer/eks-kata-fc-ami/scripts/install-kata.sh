#!/usr/bin/env bash
# install-kata.sh — bake the pinned kata-containers static release (which
# bundles the Firecracker VMM, guest kernel, and rootfs/initrd images) into
# /opt/kata, kata-deploy-style, with NO kata-deploy DaemonSet.
#
# kata >= 3.28.0 publishes zstd-compressed release tarballs
# (kata-static-<version>-<arch>.tar.zst) and NO .sha256sum sidecar files
# (setec#198), so the sha256 pin is mandatory — the same
# fetch/verify/extract approach as Dockerfile.installer, which pins the
# identical digest so the AMI and the installer DaemonSet lay down the
# SAME payload.
#
# Runs as root inside the Packer build instance.
set -euo pipefail

: "${KATA_VERSION:?KATA_VERSION must be set (e.g. 3.28.0)}"
: "${KATA_SHA256:?KATA_SHA256 must be set — kata >= 3.28.0 releases carry no .sha256sum sidecars, so an unpinned bake cannot be verified. Pin the sha256 of kata-static-${KATA_VERSION}-amd64.tar.zst (keep in lockstep with Dockerfile.installer).}"

# x86 only (ADR-0001).
ARCH=amd64
TARBALL="kata-static-${KATA_VERSION}-${ARCH}.tar.zst"
URL="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/${TARBALL}"

echo ">>> installing zstd"
dnf install -y zstd

echo ">>> downloading ${URL}"
curl -fsSL --retry 3 -o "/tmp/${TARBALL}" "${URL}"

echo ">>> verifying pinned sha256"
echo "${KATA_SHA256}  /tmp/${TARBALL}" | sha256sum -c -

echo ">>> extracting to / (tarball is rooted at ./opt/kata)"
tar --zstd -C / -xf "/tmp/${TARBALL}"
rm -f "/tmp/${TARBALL}"

# kata-deploy parity: the containerd runtime_type io.containerd.kata-fc.v2
# resolves to a containerd-shim-kata-fc-v2 binary on PATH. Symlink it to the
# single multi-config shim shipped in the static tarball.
echo ">>> linking shim + CLI binaries"
ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-fc-v2
ln -sf /opt/kata/bin/kata-runtime /usr/local/bin/kata-runtime
ln -sf /opt/kata/bin/kata-collect-data.sh /usr/local/bin/kata-collect-data.sh

# Sanity: every artifact the kata-fc handler needs at runtime must exist NOW —
# a node baked from this AMI never mutates this config again.
FC_CONFIG=/opt/kata/share/defaults/kata-containers/configuration-fc.toml
for f in \
    /opt/kata/bin/containerd-shim-kata-v2 \
    /opt/kata/bin/firecracker \
    /opt/kata/bin/jailer \
    "${FC_CONFIG}"; do
    [[ -e "${f}" ]] || { echo "FATAL: expected kata artifact missing: ${f}" >&2; exit 1; }
done

echo ">>> kata ${KATA_VERSION} baked:"
/opt/kata/bin/kata-runtime --version
/opt/kata/bin/firecracker --version | head -1
