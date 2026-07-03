#!/usr/bin/env bash
# configure-containerd.sh — statically register the kata-fc runtime handler
# and the devmapper snapshotter with containerd via a drop-in in
# /etc/containerd/config.d/.
#
# The EKS-optimized AL2023 AMI renders /etc/containerd/config.toml through
# nodeadm at boot; that rendered config imports /etc/containerd/config.d/
# *.toml drop-ins, so a file baked here is picked up on every boot with no
# live mutation (this is the supported containerd customization mechanism on
# AL2023 EKS AMIs — the polar opposite of kata-deploy's
# write-capture-inline dance).
#
# containerd 1.x consumes config version 2 table names
# (plugins."io.containerd.grpc.v1.cri") while containerd 2.x consumes
# version 3 names (plugins."io.containerd.cri.v1.runtime"). The base AMI
# pins the containerd major, so detect it ONCE at bake time and write the
# matching shape — decided at bake, immutable afterwards.
#
# Runs as root inside the Packer build instance.
set -euo pipefail

DROPIN_DIR=/etc/containerd/config.d
DROPIN="${DROPIN_DIR}/99-setec-kata-fc.toml"
FC_CONFIG=/opt/kata/share/defaults/kata-containers/configuration-fc.toml
POOL_NAME=setec-thinpool
DEVMAPPER_ROOT=/var/lib/containerd/io.containerd.snapshotter.v1.devmapper

# The drop-in mechanism must exist on the base image; hard-fail otherwise so
# a base-AMI regression is caught at bake time, not on a wedged node.
if ! grep -rqs 'config\.d' /etc/containerd/config.toml /etc/eks 2>/dev/null; then
    echo "FATAL: base image shows no /etc/containerd/config.d import support;" >&2
    echo "       the EKS AL2023 AMI contract changed — update this bake." >&2
    exit 1
fi

# Detect the containerd config schema version from the installed binary's
# default config.
CFG_VERSION="$(containerd config default | awk '$1 == "version" {print $3; exit}')"
CFG_VERSION="${CFG_VERSION:-2}"
echo ">>> containerd $(containerd --version | awk '{print $3}') — config version ${CFG_VERSION}"

mkdir -p "${DROPIN_DIR}" "${DEVMAPPER_ROOT}"

if [[ "${CFG_VERSION}" -ge 3 ]]; then
    RUNTIME_TABLE='plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata-fc'
else
    RUNTIME_TABLE='plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-fc'
fi

cat > "${DROPIN}" <<EOF
# Baked by zeroroot-ai/setec packer/eks-kata-fc-ami — DO NOT EDIT ON-NODE.
# Static kata-fc (Firecracker) runtime registration + devmapper snapshotter.
version = ${CFG_VERSION}

# Firecracker needs a block device per container rootfs (no overlayfs); the
# devmapper snapshotter carves thin volumes out of the pool that
# setec-thinpool.service builds from local NVMe at boot.
[plugins."io.containerd.snapshotter.v1.devmapper"]
  root_path = "${DEVMAPPER_ROOT}"
  pool_name = "${POOL_NAME}"
  base_image_size = "8589934592"
  discard_blocks = true

[${RUNTIME_TABLE}]
  runtime_type = "io.containerd.kata-fc.v2"
  privileged_without_host_devices = true
  pod_annotations = ["io.katacontainers.*"]
  snapshotter = "devmapper"
  [${RUNTIME_TABLE}.options]
    ConfigPath = "${FC_CONFIG}"
EOF

echo ">>> wrote ${DROPIN}:"
cat "${DROPIN}"
