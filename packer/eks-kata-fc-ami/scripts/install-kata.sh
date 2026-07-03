#!/usr/bin/env bash
# install-kata.sh — bake the pinned kata-containers static release (which
# bundles the Firecracker VMM, guest kernel, and rootfs/initrd images) into
# /opt/kata, kata-deploy-style, with NO kata-deploy DaemonSet.
#
# Runs as root inside the Packer build instance.
set -euo pipefail

: "${KATA_VERSION:?KATA_VERSION must be set (e.g. 3.28.0)}"
KATA_SHA256="${KATA_SHA256:-}"

ARCH=arm64
TARBALL="kata-static-${KATA_VERSION}-${ARCH}.tar.xz"
URL="https://github.com/kata-containers/kata-containers/releases/download/${KATA_VERSION}/${TARBALL}"

echo ">>> downloading ${URL}"
curl -fsSL --retry 3 -o "/tmp/${TARBALL}" "${URL}"

if [[ -n "${KATA_SHA256}" ]]; then
    echo ">>> verifying pinned sha256"
    echo "${KATA_SHA256}  /tmp/${TARBALL}" | sha256sum -c -
else
    # No pin supplied: try the release's sha256 sidecar. This protects
    # against transport corruption only — pin kata_sha256 in the Packer
    # build for a fully reproducible, tamper-evident bake.
    echo ">>> no kata_sha256 pin supplied; trying release sidecar ${TARBALL}.sha256sum"
    if curl -fsSL --retry 3 -o "/tmp/${TARBALL}.sha256sum" "${URL}.sha256sum"; then
        (cd /tmp && sed "s|  .*|  ${TARBALL}|" "${TARBALL}.sha256sum" | sha256sum -c -)
    else
        echo "WARNING: no sha256 pin and no release sidecar found — proceeding on TLS integrity alone." >&2
        echo "WARNING: set -var 'kata_sha256=<digest>' to pin the artifact." >&2
    fi
fi

echo ">>> extracting to / (tarball is rooted at ./opt/kata)"
tar -C / -xJf "/tmp/${TARBALL}"
rm -f "/tmp/${TARBALL}" "/tmp/${TARBALL}.sha256sum"

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
