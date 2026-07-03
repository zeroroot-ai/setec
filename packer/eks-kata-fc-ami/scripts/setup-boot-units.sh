#!/usr/bin/env bash
# setup-boot-units.sh — install the boot-time thin-pool unit, the containerd
# ordering drop-in, and the static RuntimeClass manifest from the files/
# payload uploaded to /tmp/setec-files.
#
# Runs as root inside the Packer build instance.
set -euo pipefail

SRC=/tmp/setec-files

# LVM tooling for the thin-pool (thin-provisioning-tools ships thin_check,
# which lvm's thin-pool activation requires).
echo ">>> installing lvm2 + thin-provisioning-tools"
dnf install -y lvm2 thin-provisioning-tools

echo ">>> installing boot-time thin-pool provisioner"
install -m 0755 "${SRC}/setec-thinpool.sh" /usr/local/sbin/setec-thinpool.sh
install -m 0644 "${SRC}/setec-thinpool.service" /etc/systemd/system/setec-thinpool.service

echo ">>> ordering containerd after the thin-pool"
install -d /etc/systemd/system/containerd.service.d
install -m 0644 "${SRC}/containerd-after-thinpool.conf" \
    /etc/systemd/system/containerd.service.d/10-setec-thinpool.conf

echo ">>> baking static RuntimeClass manifest"
install -d /etc/setec/manifests
install -m 0644 "${SRC}/runtimeclass-kata-fc.yaml" /etc/setec/manifests/runtimeclass-kata-fc.yaml

systemctl daemon-reload
systemctl enable setec-thinpool.service

rm -rf "${SRC}"
echo ">>> boot units installed"
